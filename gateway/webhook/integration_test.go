package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/google/uuid"
	"github.com/ory/dockertest/v3"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/errgroup"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	kithelper "github.com/rudderlabs/rudder-go-kit/testhelper"
	"github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/postgres"
	transformertest "github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/transformer"
	kituuid "github.com/rudderlabs/rudder-go-kit/uuid"
	"github.com/rudderlabs/rudder-schemas/go/stream"
	"github.com/rudderlabs/rudder-server/app"
	"github.com/rudderlabs/rudder-server/gateway/throttler"
	"github.com/rudderlabs/rudder-server/jobsdb"
	sourcedebugger "github.com/rudderlabs/rudder-server/services/debugger/source"
	"github.com/rudderlabs/rudder-server/services/rsources"
	"github.com/rudderlabs/rudder-server/services/transformer"
	"github.com/rudderlabs/rudder-server/testhelper/backendconfigtest"

	"github.com/rudderlabs/rudder-go-kit/stats/memstats"
	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/gateway"

	"github.com/rudderlabs/rudder-go-kit/requesttojson"
	"github.com/rudderlabs/rudder-transformer/go/webhook/testcases"
)

func TestIntegrationWebhook(t *testing.T) {
	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	var (
		p                    *postgres.Resource
		transformerContainer *transformertest.Resource
	)

	g, _ := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		p, err = postgres.Setup(pool, t)
		if err != nil {
			return fmt.Errorf("starting postgres: %w", err)
		}
		return nil
	})

	g.Go(func() (err error) {
		transformerContainer, err = transformertest.Setup(pool, t)
		if err != nil {
			return fmt.Errorf("starting transformer: %w", err)
		}
		return nil
	})
	err = g.Wait()
	require.NoError(t, err)

	var gw gateway.Handle

	conf := config.New()
	logger := logger.NOP
	stat, err := memstats.New()
	require.NoError(t, err)

	conf.Set("Gateway.enableSuppressUserFeature", false)

	gatewayDB := jobsdb.NewForReadWrite(
		"gateway",
		jobsdb.WithDBHandle(p.DB),
		jobsdb.WithStats(stats.NOP),
	)

	require.NoError(t, gatewayDB.Start())
	defer gatewayDB.TearDown()

	errDB := jobsdb.NewForReadWrite(
		"err",
		jobsdb.WithDBHandle(p.DB),
		jobsdb.WithStats(stats.NOP),
	)
	require.NoError(t, errDB.Start())
	defer errDB.TearDown()

	var (
		rateLimiter        throttler.Throttler
		versionHandler     func(w http.ResponseWriter, r *http.Request)
		streamMsgValidator func(message *stream.Message) error
		application        app.App
	)

	transformerURL, ok := os.LookupEnv("TEST_OVERRIDE_TRANSFORMER_URL")
	if !ok {
		transformerURL = transformerContainer.TransformerURL
	}

	transformerFeaturesService := transformer.NewFeaturesService(ctx, conf, transformer.FeaturesServiceOptions{
		PollInterval:             config.GetDuration("Transformer.pollInterval", 10, time.Second),
		TransformerURL:           transformerURL,
		FeaturesRetryMaxAttempts: 10,
	})
	t.Setenv("DEST_TRANSFORM_URL", transformerURL)

	<-transformerFeaturesService.Wait()

	bcs := make(map[string]backendconfig.ConfigT)

	testSetup := testcases.Load(t)

	sourceConfigs := make([]backendconfig.SourceT, len(testSetup.Cases))

	for i, tc := range testSetup.Cases {
		sConfig := backendconfigtest.NewSourceBuilder().
			WithSourceType(strings.ToUpper(tc.Name)).
			WithSourceCategory("webhook").
			WithConnection(
				backendconfigtest.NewDestinationBuilder("WEBHOOK").Build(),
			).
			Build()

		// If source config exists in test case, unmarshal and set it
		if len(tc.Input.Source.Config) > 0 {
			sConfig.Config = []byte(tc.Input.Source.Config)
		}

		bc := backendconfigtest.NewConfigBuilder().WithSource(
			sConfig,
		).Build()

		// fix this in backendconfigtest
		bc.Sources[0].WorkspaceID = bc.WorkspaceID
		sConfig.WorkspaceID = bc.WorkspaceID
		bcs[bc.WorkspaceID] = bc
		sourceConfigs[i] = sConfig
	}
	httpPort, err := kithelper.GetFreePort()
	require.NoError(t, err)

	conf.Set("Gateway.webPort", httpPort)

	err = gw.Setup(ctx,
		conf, logger, stat,
		application,
		backendconfigtest.NewStaticLibrary(bcs),
		gatewayDB, errDB,
		rateLimiter, versionHandler, rsources.NewNoOpService(), transformerFeaturesService, sourcedebugger.NewNoOpService(),
		streamMsgValidator,
		gateway.WithNow(func() time.Time {
			return testSetup.Context.Now
		}))
	require.NoError(t, err)
	g.Go(func() error {
		return gw.StartWebHandler(ctx)
	})
	defer func() {
		if err := gw.Shutdown(); err != nil {
			require.NoError(t, err)
		}
	}()

	gwURL := fmt.Sprintf("http://localhost:%d", httpPort)

	require.Eventually(t, func() bool {
		resp, err := http.Get(fmt.Sprintf("%s/health", gwURL))
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, time.Millisecond*500, time.Millisecond)

	for i, tc := range testSetup.Cases {
		sConfig := sourceConfigs[i]

		writeKey := sConfig.WriteKey
		sourceID := sConfig.ID
		workspaceID := sConfig.WorkspaceID

		t.Run(tc.Name+"/"+tc.Description, func(t *testing.T) {
			if tc.Skip != "" {
				t.Skip(tc.Skip)
				return
			}
			t.Logf("writeKey: %s", writeKey)
			t.Logf("sourceID: %s", sourceID)
			t.Logf("workspaceID: %s", workspaceID)

			query := url.Values{}
			query.Set("writeKey", writeKey)
			for k, v := range tc.Input.Request.RawQuery {
				if k == "writeKey" {
					continue
				}
				query.Set(k, v[0])
			}

			t.Log("Request URL:", fmt.Sprintf("%s/v1/webhook?%s", gwURL, query.Encode()))
			method := tc.Input.Request.Method
			if method == "" {
				method = http.MethodPost
			}
			req, err := http.NewRequest(method, fmt.Sprintf("%s/v1/webhook?%s", gwURL, query.Encode()), bytes.NewBuffer([]byte(tc.Input.Request.Body)))
			require.NoError(t, err)

			req.Header.Set("X-Forwarded-For", testSetup.Context.RequestIP)
			for k, v := range tc.Input.Request.Headers {
				req.Header.Set(k, v)
			}

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			require.Equal(t, tc.Output.Response.StatusCode, resp.StatusCode)
			if resp.Header.Get("Content-Type") == "application/json" {
				require.JSONEq(t, strings.ToLower(string(tc.Output.Response.Body)), strings.ToLower(string(b)))
			} else {
				require.Equal(t, strings.ToLower(string(tc.Output.Response.Body)), strings.ToLower(fmt.Sprintf("%q", b)))
			}
			r, err := gatewayDB.GetUnprocessed(ctx, jobsdb.GetQueryParams{
				WorkspaceID: workspaceID,
				ParameterFilters: []jobsdb.ParameterFilterT{{
					Name:  "source_id",
					Value: sourceID,
				}},
				JobsLimit: 10,
			})
			require.NoError(t, err)

			require.Len(t, r.Jobs, len(tc.Output.Queue), "enqueued items mismatch")
			for i, p := range tc.Output.Queue {
				var batch struct {
					Batch []json.RawMessage `json:"batch"`
				}
				err := jsonrs.Unmarshal(r.Jobs[i].EventPayload, &batch)
				require.NoError(t, err)
				require.Len(t, batch.Batch, 1)

				if gjson.GetBytes(p, "messageId").String() == uuid.Nil.String() {
					rawMsgID := gjson.GetBytes(batch.Batch[0], "messageId").String()
					msgID, err := uuid.Parse(rawMsgID)
					require.NoErrorf(t, err, "messageId (%q) is not a valid UUID", rawMsgID)

					p, err = sjson.SetBytes(p, "messageId", msgID.String())
					require.NoError(t, err)
				}

				userID := gjson.GetBytes(batch.Batch[0], "userId").String()
				anonID := gjson.GetBytes(batch.Batch[0], "anonymousId").String()

				rudderID, err := kituuid.GetMD5UUID(userID + ":" + anonID)
				require.NoError(t, err)

				if anonID != "" {
					p, err = sjson.SetBytes(p, "anonymousId", anonID)
					require.NoError(t, err)
				}

				p, err = sjson.SetBytes(p, "rudderId", rudderID)
				require.NoError(t, err)

				if gjson.GetBytes(batch.Batch[0], "properties.writeKey").String() != "" {
					p, err = sjson.SetBytes(p, "properties.writeKey", writeKey)
					require.NoError(t, err)
				}

				if gjson.GetBytes(p, "receivedAt").String() != "" {
					p, err = sjson.SetBytes(p, "receivedAt", "2006-01-02T15:04:05.000Z07:00")
					require.NoError(t, err)
				}

				batch.Batch[0], err = sjson.SetBytes(batch.Batch[0], "receivedAt", "2006-01-02T15:04:05.000Z07:00")
				require.NoError(t, err)

				batch.Batch[0], err = sjson.DeleteBytes(batch.Batch[0], "context.url")
				require.NoError(t, err)

				p, err = sjson.DeleteBytes(p, "context.url")
				require.NoError(t, err)

				batch.Batch[0], err = sjson.DeleteBytes(batch.Batch[0], "context.query_parameters.writeKey")
				require.NoError(t, err)
				p, err = sjson.DeleteBytes(p, "context.query_parameters.writeKey")
				require.NoError(t, err)

				require.JSONEq(t, string(p), string(batch.Batch[0]))
			}

			require.Eventually(t, func() bool {
				r, err = errDB.GetUnprocessed(ctx, jobsdb.GetQueryParams{
					WorkspaceID: workspaceID,
					ParameterFilters: []jobsdb.ParameterFilterT{{
						Name:  "source_id",
						Value: sourceID,
					}},
					JobsLimit: 1,
				})
				return err == nil && len(r.Jobs) == len(tc.Output.ErrQueue)
			}, time.Second, time.Millisecond*10)

			require.NoError(t, err)
			require.Len(t, r.Jobs, len(tc.Output.ErrQueue))
			for i, p := range tc.Output.ErrQueue {
				var errPayload []byte

				var requestPayload *requesttojson.RequestJSON
				var requestPayloadBytes []byte

				// set defaults assigned by go http client
				req.Body = io.NopCloser(bytes.NewReader(p))
				req.Method = "POST"
				req.Proto = "HTTP/1.1"
				req.Header.Set("Accept-Encoding", "gzip")
				req.Header.Set("Content-Length", strconv.Itoa(len(p)))
				req.Header.Set("User-Agent", "Go-http-client/1.1")

				requestPayload, err = requesttojson.RequestToJSON(req, "{}")
				requestPayloadBytes, err = jsonrs.Marshal(requestPayload)

				errPayload, err = jsonrs.Marshal(struct {
					Request json.RawMessage       `json:"request"`
					Source  backendconfig.SourceT `json:"source"`
				}{
					Source:  sConfig,
					Request: requestPayloadBytes,
				})
				require.NoError(t, err)
				errPayload, err = sjson.SetBytes(errPayload, "source.Destinations", nil)
				require.NoError(t, err)

				payload := gjson.GetBytes(r.Jobs[i].EventPayload, "request.body").String()
				if !gjson.ValidBytes([]byte(payload)) {
					marshaledPayload, err := jsonrs.Marshal(payload)
					require.NoError(t, err)
					r.Jobs[i].EventPayload, err = sjson.SetBytes(r.Jobs[i].EventPayload, "request.body", marshaledPayload)
					require.NoError(t, err)
				}

				r.Jobs[i].EventPayload, err = sjson.DeleteBytes(r.Jobs[i].EventPayload, "request.headers.Content-Length")
				require.NoError(t, err)
				errPayload, err = sjson.DeleteBytes(errPayload, "request.headers.Content-Length")
				require.NoError(t, err)

				require.JSONEq(t, string(errPayload), string(r.Jobs[i].EventPayload))
			}
		})

	}

	cancel()
	err = g.Wait()
	require.NoError(t, err)
}
