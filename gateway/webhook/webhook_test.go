package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	gwtypes "github.com/rudderlabs/rudder-server/gateway/types"

	"go.uber.org/mock/gomock"

	"github.com/rudderlabs/rudder-go-kit/bytesize"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	gwStats "github.com/rudderlabs/rudder-server/gateway/internal/stats"
	mockWebhook "github.com/rudderlabs/rudder-server/gateway/mocks"
	"github.com/rudderlabs/rudder-server/gateway/response"
	mock_features "github.com/rudderlabs/rudder-server/mocks/services/transformer"
	"github.com/rudderlabs/rudder-server/services/transformer"
	"github.com/rudderlabs/rudder-server/utils/misc"
)

const (
	sampleWriteKey = "SampleWriteKey"
	sourceDefName  = "webhook"
	sampleError    = "someError"
	sampleJson     = "{\"hello\":\"world\"}"
)

var (
	once            sync.Once
	outputToGateway = map[string]interface{}{"hello": "world"}
	outputToWebhook = &outputToSource{
		Body:        []byte(sampleJson),
		ContentType: "application/json",
	}
)

func initWebhook() {
	once.Do(func() {
		config.Reset()
		logger.Reset()
		misc.Init()
		config.Set("Gateway.webhook.maxTransformerProcess", 1)
		config.Set("WriteTimeout", "1s")
	})
}

type mockSourceTransformAdapter struct {
	url string
}

func (v0 *mockSourceTransformAdapter) getTransformerEvent(_ *gwtypes.AuthRequestContext, body []byte) ([]byte, error) {
	return body, nil
}

func (v0 *mockSourceTransformAdapter) getTransformerURL(string) (string, error) {
	return v0.url, nil
}

func (v0 *mockSourceTransformAdapter) getAdapterVersion() string {
	return transformer.V0
}

func getMockSourceTransformAdapterFunc(url string) func(ctx context.Context) (sourceTransformAdapter, error) {
	return func(ctx context.Context) (sourceTransformAdapter, error) {
		mst := &mockSourceTransformAdapter{}
		mst.url = url
		return mst, nil
	}
}

func TestWebhookMaxRequestSize(t *testing.T) {
	initWebhook()

	ctrl := gomock.NewController(t)

	mockGW := mockWebhook.NewMockGateway(ctrl)
	mockGW.EXPECT().TrackRequestMetrics(gomock.Any()).Times(1)

	mockTransformerFeaturesService := mock_features.NewMockFeaturesService(ctrl)

	maxReqSizeInKB := 1

	webhookHandler := Setup(mockGW, mockTransformerFeaturesService, stats.NOP, config.Default, newSourceStatReporter, func(bt *batchWebhookTransformerT) {
		bt.sourceTransformAdapter = func(ctx context.Context) (sourceTransformAdapter, error) {
			return &mockSourceTransformAdapter{}, nil
		}
	})
	webhookHandler.config.maxReqSize = config.SingleValueLoader(maxReqSizeInKB)
	t.Cleanup(func() {
		_ = webhookHandler.Shutdown()
	})

	webhookHandler.Register(sourceDefName)

	payload := fmt.Sprintf(`{"hello":"world", "data": %q}`, strings.Repeat("a", 2*maxReqSizeInKB*int(bytesize.KB)))
	require.Greater(t, len(payload), maxReqSizeInKB*int(bytesize.KB))

	req := httptest.NewRequest(http.MethodPost, "/v1/webhook", bytes.NewBufferString(payload))
	resp := httptest.NewRecorder()

	reqCtx := context.WithValue(req.Context(), gwtypes.CtxParamCallType, "webhook")
	reqCtx = context.WithValue(reqCtx, gwtypes.CtxParamAuthRequestContext, &gwtypes.AuthRequestContext{
		SourceDefName: sourceDefName,
	})

	webhookHandler.RequestHandler(resp, req.WithContext(reqCtx))
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.Result().StatusCode)
}

func newSourceStatReporter(_ *gwtypes.AuthRequestContext, _ string) gwtypes.StatReporter {
	return &gwStats.SourceStat{}
}

func TestWebhookRequestHandlerWithTransformerBatchGeneralError(t *testing.T) {
	initWebhook()
	ctrl := gomock.NewController(t)
	mockGW := mockWebhook.NewMockGateway(ctrl)
	transformerServer := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, sampleError, http.StatusBadRequest)
		}))
	webhookHandler := Setup(mockGW, transformer.NewNoOpService(), stats.NOP, config.Default, newSourceStatReporter, func(bt *batchWebhookTransformerT) {
		bt.sourceTransformAdapter = getMockSourceTransformAdapterFunc(transformerServer.URL)
	})

	mockGW.EXPECT().TrackRequestMetrics(gomock.Any()).Times(1)
	mockGW.EXPECT().SaveWebhookFailures(gomock.Any()).Return(nil).Times(1)
	arctx := &gwtypes.AuthRequestContext{
		SourceDefName: sourceDefName,
		WriteKey:      sampleWriteKey,
	}
	webhookHandler.Register(sourceDefName)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook", bytes.NewBufferString(sampleJson))
	w := httptest.NewRecorder()
	ctx := context.WithValue(req.Context(), gwtypes.CtxParamCallType, "webhook")
	ctx = context.WithValue(ctx, gwtypes.CtxParamAuthRequestContext, arctx)
	req = req.WithContext(ctx)
	webhookHandler.RequestHandler(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Result().StatusCode)
	assert.Contains(t, strings.TrimSpace(w.Body.String()), "source Transformer returned non-success status")
	_ = webhookHandler.Shutdown()
}

func TestWebhookRequestHandlerWithTransformerBatchPayloadLengthMismatchError(t *testing.T) {
	initWebhook()
	ctrl := gomock.NewController(t)
	mockGW := mockWebhook.NewMockGateway(ctrl)
	transformerServer := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { _ = r.Body.Close() }()
			body, _ := io.ReadAll(r.Body)
			var requests []interface{}
			_ = jsonrs.Unmarshal(body, &requests)
			var responses []transformerResponse
			// return payload of length = len(requests) + 1
			for i := 0; i < len(requests)+1; i++ {
				responses = append(responses, transformerResponse{
					Err:        sampleError,
					StatusCode: http.StatusBadRequest,
				})
			}
			respBody, _ := jsonrs.Marshal(responses)
			_, _ = w.Write(respBody)
		}))
	webhookHandler := Setup(mockGW, transformer.NewNoOpService(), stats.NOP, config.Default, newSourceStatReporter, func(bt *batchWebhookTransformerT) {
		bt.sourceTransformAdapter = getMockSourceTransformAdapterFunc(transformerServer.URL)
	})

	mockGW.EXPECT().TrackRequestMetrics(gomock.Any()).Times(1)

	mockGW.EXPECT().SaveWebhookFailures(gomock.Any()).Return(nil).Times(1)

	webhookHandler.Register(sourceDefName)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook?writeKey="+sampleWriteKey, bytes.NewBufferString(sampleJson))
	w := httptest.NewRecorder()
	ctx := context.WithValue(req.Context(), gwtypes.CtxParamCallType, "webhook")
	ctx = context.WithValue(ctx, gwtypes.CtxParamAuthRequestContext, &gwtypes.AuthRequestContext{
		SourceDefName: sourceDefName,
	})
	req = req.WithContext(ctx)
	webhookHandler.RequestHandler(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Result().StatusCode)
	assert.Contains(t, strings.TrimSpace(w.Body.String()), response.SourceTransformerInvalidResponseFormat)
	_ = webhookHandler.Shutdown()
}

func TestWebhookRequestHandlerWithTransformerRequestError(t *testing.T) {
	initWebhook()
	ctrl := gomock.NewController(t)
	mockGW := mockWebhook.NewMockGateway(ctrl)
	transformerServer := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { _ = r.Body.Close() }()
			body, _ := io.ReadAll(r.Body)
			var requests []interface{}
			_ = jsonrs.Unmarshal(body, &requests)
			var responses []transformerResponse
			for i := 0; i < len(requests); i++ {
				responses = append(responses, transformerResponse{
					Err:        sampleError,
					StatusCode: http.StatusBadRequest,
				})
			}
			respBody, _ := jsonrs.Marshal(responses)
			_, _ = w.Write(respBody)
		}))
	webhookHandler := Setup(mockGW, transformer.NewNoOpService(), stats.NOP, config.Default, newSourceStatReporter, func(bt *batchWebhookTransformerT) {
		bt.sourceTransformAdapter = getMockSourceTransformAdapterFunc(transformerServer.URL)
	})

	mockGW.EXPECT().TrackRequestMetrics(gomock.Any()).Times(1)

	mockGW.EXPECT().SaveWebhookFailures(gomock.Any()).Return(nil).Times(1)

	webhookHandler.Register(sourceDefName)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook?writeKey="+sampleWriteKey, bytes.NewBufferString(sampleJson))
	w := httptest.NewRecorder()
	ctx := context.WithValue(req.Context(), gwtypes.CtxParamCallType, "webhook")
	ctx = context.WithValue(ctx, gwtypes.CtxParamAuthRequestContext, &gwtypes.AuthRequestContext{
		SourceDefName: sourceDefName,
	})
	req = req.WithContext(ctx)
	webhookHandler.RequestHandler(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Result().StatusCode)
	assert.Contains(t, sampleError, strings.TrimSpace(w.Body.String()))
	_ = webhookHandler.Shutdown()
}

func TestWebhookRequestHandlerWithOutputToSource(t *testing.T) {
	initWebhook()
	ctrl := gomock.NewController(t)
	mockGW := mockWebhook.NewMockGateway(ctrl)
	transformerServer := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { _ = r.Body.Close() }()
			body, _ := io.ReadAll(r.Body)
			var requests []interface{}
			_ = jsonrs.Unmarshal(body, &requests)
			var responses []transformerResponse
			for i := 0; i < len(requests); i++ {
				responses = append(responses, transformerResponse{
					OutputToSource: outputToWebhook,
					StatusCode:     http.StatusOK,
				})
			}
			respBody, _ := jsonrs.Marshal(responses)
			_, _ = w.Write(respBody)
		}))
	webhookHandler := Setup(mockGW, transformer.NewNoOpService(), stats.NOP, config.Default, newSourceStatReporter, func(bt *batchWebhookTransformerT) {
		bt.sourceTransformAdapter = getMockSourceTransformAdapterFunc(transformerServer.URL)
	})
	mockGW.EXPECT().TrackRequestMetrics("").Times(1)

	webhookHandler.Register(sourceDefName)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook?writeKey="+sampleWriteKey, bytes.NewBufferString(sampleJson))
	w := httptest.NewRecorder()
	ctx := context.WithValue(req.Context(), gwtypes.CtxParamCallType, "webhook")
	ctx = context.WithValue(ctx, gwtypes.CtxParamAuthRequestContext, &gwtypes.AuthRequestContext{
		SourceDefName: sourceDefName,
	})
	req = req.WithContext(ctx)
	webhookHandler.RequestHandler(w, req)

	assert.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Equal(t, sampleJson, strings.TrimSpace(w.Body.String()))
	_ = webhookHandler.Shutdown()
}

func TestWebhookRequestHandlerWithOutputToGateway(t *testing.T) {
	initWebhook()
	ctrl := gomock.NewController(t)
	mockGW := mockWebhook.NewMockGateway(ctrl)
	outputToGateway := map[string]interface{}{"text": "hello world"}
	transformerServer := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { _ = r.Body.Close() }()
			body, _ := io.ReadAll(r.Body)
			var requests []interface{}
			_ = jsonrs.Unmarshal(body, &requests)
			var responses []transformerResponse
			for i := 0; i < len(requests); i++ {
				responses = append(responses, transformerResponse{
					Output:     outputToGateway,
					StatusCode: http.StatusOK,
				})
			}
			respBody, _ := jsonrs.Marshal(responses)
			_, _ = w.Write(respBody)
		}))
	webhookHandler := Setup(mockGW, transformer.NewNoOpService(), stats.NOP, config.Default, newSourceStatReporter, func(bt *batchWebhookTransformerT) {
		bt.sourceTransformAdapter = getMockSourceTransformAdapterFunc(transformerServer.URL)
	})
	mockGW.EXPECT().TrackRequestMetrics("").Times(1)

	gwPayload, _ := jsonrs.Marshal(outputToGateway)
	arctx := &gwtypes.AuthRequestContext{
		WriteKey:      sampleWriteKey,
		SourceDefName: sourceDefName,
	}
	mockGW.EXPECT().ProcessTransformedWebhookRequest(gomock.Any(), gomock.Any(), "batch", gwPayload, arctx).Times(1)

	webhookHandler.Register(sourceDefName)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook", bytes.NewBufferString(sampleJson))
	w := httptest.NewRecorder()
	ctx := context.WithValue(req.Context(), gwtypes.CtxParamCallType, "webhook")
	ctx = context.WithValue(ctx, gwtypes.CtxParamAuthRequestContext, arctx)
	req = req.WithContext(ctx)
	webhookHandler.RequestHandler(w, req)

	assert.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Equal(t, response.Ok, strings.TrimSpace(w.Body.String()))
	_ = webhookHandler.Shutdown()
}

func TestWebhookRequestHandlerWithOutputToGatewayAndSource(t *testing.T) {
	initWebhook()
	ctrl := gomock.NewController(t)
	mockGW := mockWebhook.NewMockGateway(ctrl)
	transformerServer := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { _ = r.Body.Close() }()
			body, _ := io.ReadAll(r.Body)
			var requests []interface{}
			_ = jsonrs.Unmarshal(body, &requests)
			var responses []transformerResponse
			for i := 0; i < len(requests); i++ {
				responses = append(responses, transformerResponse{
					Output:         outputToGateway,
					OutputToSource: outputToWebhook,
					StatusCode:     http.StatusOK,
				})
			}
			respBody, _ := jsonrs.Marshal(responses)
			_, _ = w.Write(respBody)
		}))
	webhookHandler := Setup(mockGW, transformer.NewNoOpService(), stats.NOP, config.Default, newSourceStatReporter, func(bt *batchWebhookTransformerT) {
		bt.sourceTransformAdapter = getMockSourceTransformAdapterFunc(transformerServer.URL)
	})
	mockGW.EXPECT().TrackRequestMetrics("").Times(1)

	gwPayload, _ := jsonrs.Marshal(outputToGateway)
	arctx := &gwtypes.AuthRequestContext{
		WriteKey:      sampleWriteKey,
		SourceDefName: sourceDefName,
	}
	mockGW.EXPECT().ProcessTransformedWebhookRequest(gomock.Any(), gomock.Any(), "batch", gwPayload, arctx).Times(1)

	webhookHandler.Register(sourceDefName)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook", bytes.NewBufferString(sampleJson))
	w := httptest.NewRecorder()
	ctx := context.WithValue(req.Context(), gwtypes.CtxParamCallType, "webhook")
	ctx = context.WithValue(ctx, gwtypes.CtxParamAuthRequestContext, arctx)
	req = req.WithContext(ctx)
	webhookHandler.RequestHandler(w, req)

	assert.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Equal(t, sampleJson, strings.TrimSpace(w.Body.String()))
	_ = webhookHandler.Shutdown()
}

func TestRecordWebhookErrors(t *testing.T) {
	initWebhook()
	ctrl := gomock.NewController(t)
	mockGW := mockWebhook.NewMockGateway(ctrl)
	statsStore, err := memstats.New()
	require.NoError(t, err)
	webhookHandler := Setup(mockGW, transformer.NewNoOpService(), statsStore, config.Default, newSourceStatReporter)
	reqs := []*webhookT{
		{authContext: &gwtypes.AuthRequestContext{WriteKey: "w1", SourceID: "sourceID1", WorkspaceID: "workspaceID1", SourceCategory: "webhook1"}},
		{authContext: &gwtypes.AuthRequestContext{WriteKey: "w2", SourceID: "sourceID2", WorkspaceID: "workspaceID2", SourceCategory: "webhook2"}},
		{authContext: &gwtypes.AuthRequestContext{WriteKey: "w1", SourceID: "sourceID1", WorkspaceID: "workspaceID1", SourceCategory: "webhook1"}},
		{authContext: &gwtypes.AuthRequestContext{WriteKey: "w3", SourceID: "sourceID3", WorkspaceID: "workspaceID3", SourceCategory: "webhook3"}},
		{authContext: &gwtypes.AuthRequestContext{WriteKey: "w2", SourceID: "sourceID2", WorkspaceID: "workspaceID2", SourceCategory: "webhook2"}},
		{authContext: &gwtypes.AuthRequestContext{WriteKey: "w1", SourceID: "sourceID1", WorkspaceID: "workspaceID1", SourceCategory: "webhook1"}},
	}

	webhookHandler.recordWebhookErrors("cio", "err1", reqs, 400)

	m := statsStore.Get("webhook_num_errors", stats.Tags{
		"writeKey":    "w1",
		"workspaceId": "workspaceID1",
		"sourceID":    "sourceID1",
		"statusCode":  "400",
		"sourceType":  "cio",
		"reason":      "err1",
	})
	require.EqualValues(t, m.LastValue(), 3)
	m = statsStore.Get("webhook_num_errors", stats.Tags{
		"writeKey":    "w2",
		"workspaceId": "workspaceID2",
		"sourceID":    "sourceID2",
		"statusCode":  "400",
		"sourceType":  "cio",
		"reason":      "err1",
	})
	require.EqualValues(t, m.LastValue(), 2)
	m = statsStore.Get("webhook_num_errors", stats.Tags{
		"writeKey":    "w3",
		"workspaceId": "workspaceID3",
		"sourceID":    "sourceID3",
		"statusCode":  "400",
		"sourceType":  "cio",
		"reason":      "err1",
	})
	require.EqualValues(t, m.LastValue(), 1)
}

func TestPrepareTransformerEventRequestV1(t *testing.T) {
	type requestOpts struct {
		method  string
		target  string
		body    io.Reader
		params  map[string]string
		headers map[string]string
	}

	createRequest := func(reqOpts requestOpts) *http.Request {
		r := httptest.NewRequest(reqOpts.method, reqOpts.target, reqOpts.body)
		for k, v := range reqOpts.headers {
			r.Header.Set(k, v)
		}

		q := r.URL.Query()
		for k, v := range reqOpts.params {
			q.Add(k, v)
		}
		r.URL.RawQuery = q.Encode()
		return r
	}

	testCases := []struct {
		name               string
		req                *http.Request
		sourceType         string
		includeQueryParams bool
		wantError          bool
		expectedResponse   string
	}{
		{
			name:             "Empty request body with no query parameters for webhook",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com"}),
			sourceType:       "webhook",
			expectedResponse: "{}",
		},
		{
			name:             "Empty request body with query parameters for webhook",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", params: map[string]string{"key": "value"}}),
			sourceType:       "webhook",
			expectedResponse: "{}",
		},
		{
			name:             "Some payload with no query parameters for webhook",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", body: strings.NewReader(`{"key":"value"}`)}),
			sourceType:       "webhook",
			expectedResponse: `{"key":"value"}`,
		},
		{
			name:             "Empty request body with query parameters for shopify",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", params: map[string]string{"key": "value"}}),
			sourceType:       "shopify",
			expectedResponse: `{"query_parameters":{"key":["value"]}}`,
		},
		{
			name:             "Error reading request body for Shopify",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", body: iotest.ErrReader(errors.New("some error"))}),
			sourceType:       "Shopify",
			wantError:        true,
			expectedResponse: "",
		},
		{
			name:             "Some payload with no query parameters for shopify",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", body: strings.NewReader(`{"key":"value"}`)}),
			sourceType:       "shopify",
			expectedResponse: `{"key":"value","query_parameters":{}}`,
		},
		{
			name:             "Some payload with query parameters for Adjust",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", body: strings.NewReader(`{"key1":"value1"}`), params: map[string]string{"key2": "value2"}}),
			sourceType:       "Adjust",
			expectedResponse: `{"key1":"value1","query_parameters":{"key2":["value2"]}}`,
		},
		{
			name:             "No payload with query parameters for Adjust",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", params: map[string]string{"key2": "value2"}}),
			sourceType:       "adjust",
			expectedResponse: `{"query_parameters":{"key2":["value2"]}}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := prepareTransformerEventRequestV1(tc.req, tc.sourceType, []string{"adjust", "shopify"})
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expectedResponse, string(result))
		})
	}
}

func TestPrepareTransformerEventRequestV2(t *testing.T) {
	type requestOpts struct {
		method  string
		target  string
		body    io.Reader
		params  map[string]string
		headers map[string]string
	}

	createRequest := func(reqOpts requestOpts) *http.Request {
		r := httptest.NewRequest(reqOpts.method, reqOpts.target, reqOpts.body)
		for k, v := range reqOpts.headers {
			r.Header.Set(k, v)
		}

		q := r.URL.Query()
		for k, v := range reqOpts.params {
			q.Add(k, v)
		}
		r.URL.RawQuery = q.Encode()
		return r
	}

	testCases := []struct {
		name               string
		req                *http.Request
		includeQueryParams bool
		wantError          bool
		expectedResponse   string
	}{
		{
			name:             "Empty request body with no query parameters and no headers",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com"}),
			expectedResponse: `{"method":"POST","url":"/","proto":"HTTP/1.1","headers":{},"body":"{}","query_parameters":{}}`,
		},
		{
			name:             "Empty request body with query parameters and no headers",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", params: map[string]string{"key": "value"}}),
			expectedResponse: `{"method":"POST","url":"/?key=value","proto":"HTTP/1.1","headers":{},"body":"{}","query_parameters":{"key":["value"]}}`,
		},
		{
			name:             "Some payload with no query parameters and no headers",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", body: strings.NewReader(`{"key":"value"}`)}),
			expectedResponse: `{"method":"POST","url":"/","proto":"HTTP/1.1","headers":{},"body":"{\"key\":\"value\"}","query_parameters":{}}`,
		},
		{
			name:             "Empty request body with headers and no query parameters",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", headers: map[string]string{"content-type": "application/json"}}),
			expectedResponse: `{"method":"POST","url":"/","proto":"HTTP/1.1","headers":{"Content-Type":["application/json"]},"body":"{}","query_parameters":{}}`,
		},
		{
			name:             "Empty request body with headers and query parameters",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", params: map[string]string{"key": "value"}, headers: map[string]string{"content-type": "application/json"}}),
			expectedResponse: `{"method":"POST","url":"/?key=value","proto":"HTTP/1.1","headers":{"Content-Type":["application/json"]},"body":"{}","query_parameters":{"key":["value"]}}`,
		},
		{
			name:             "Some payload with headers and no parameters",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", body: strings.NewReader(`{"key":"value"}`), headers: map[string]string{"content-type": "application/json"}}),
			expectedResponse: `{"method":"POST","url":"/","proto":"HTTP/1.1","headers":{"Content-Type":["application/json"]},"body":"{\"key\":\"value\"}","query_parameters":{}}`,
		},
		{
			name:             "Some payload with parameters and no headers",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", body: strings.NewReader(`{"key":"value"}`), params: map[string]string{"key": "value"}}),
			expectedResponse: `{"method":"POST","url":"/?key=value","proto":"HTTP/1.1","headers":{},"body":"{\"key\":\"value\"}","query_parameters":{"key":["value"]}}`,
		},
		{
			name:             "Error reading request body",
			req:              createRequest(requestOpts{method: http.MethodPost, target: "http://example.com", body: iotest.ErrReader(errors.New("some error"))}),
			wantError:        true,
			expectedResponse: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := prepareTransformerEventRequestV2(tc.req)
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expectedResponse, string(result))
		})
	}
}

func TestAllowGetReqForWebhookSrc(t *testing.T) {
	cases := []struct {
		name                       string
		forwardGetRequestForSrcMap map[string]struct{}
		method                     string
		srcDef                     string
		expected                   bool
	}{
		{
			name:   "should allow get request for adjust",
			method: http.MethodGet,
			forwardGetRequestForSrcMap: map[string]struct{}{
				"adjust": {},
			},
			srcDef:   "adjust",
			expected: false,
		},
		{
			name:   "should allow post request for adjust",
			method: http.MethodPost,
			forwardGetRequestForSrcMap: map[string]struct{}{
				"adjust": {},
			},
			srcDef:   "adjust",
			expected: false,
		},
		{
			name: "should not allow get request for shopify",
			forwardGetRequestForSrcMap: map[string]struct{}{
				"adjust":     {},
				"customerio": {},
			},
			method:   http.MethodGet,
			srcDef:   "Shopify",
			expected: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wbh := HandleT{}
			wbh.config.forwardGetRequestForSrcMap = tc.forwardGetRequestForSrcMap

			isGetAndNotAllow := wbh.IsGetAndNotAllow(tc.method, tc.srcDef)
			require.Equal(t, tc.expected, isGetAndNotAllow)
		})
	}
}

func TestWebhookRequestHandlerWithRetries(t *testing.T) {
	t.Run("transformer retries", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockGW := mockWebhook.NewMockGateway(ctrl)
		initWebhook()
		var responses []transformerResponse
		responses = append(responses, transformerResponse{
			Output: map[string]interface{}{
				"hello": "world",
			},
			StatusCode: http.StatusOK,
		})
		resp, err := jsonrs.Marshal(responses)
		require.NoError(t, err)
		transformerServer := newMockTransformerServer(1, resp, []byte(sampleError), http.StatusInternalServerError)

		conf := config.Default
		webhookHandler := Setup(mockGW, transformer.NewNoOpService(), stats.NOP, conf, newSourceStatReporter, func(bt *batchWebhookTransformerT) {
			bt.sourceTransformAdapter = getMockSourceTransformAdapterFunc(transformerServer.URL)
		})
		mockGW.EXPECT().TrackRequestMetrics("").Times(1)
		mockGW.EXPECT().ProcessTransformedWebhookRequest(gomock.Any(), gomock.Any(), "batch", gomock.Any(), gomock.Any()).Times(1)

		webhookHandler.Register(sourceDefName)
		req := httptest.NewRequest(http.MethodPost, "/v1/webhook?writeKey="+sampleWriteKey, bytes.NewBufferString(sampleJson))
		w := httptest.NewRecorder()
		ctx := context.WithValue(req.Context(), gwtypes.CtxParamCallType, "webhook")
		ctx = context.WithValue(ctx, gwtypes.CtxParamAuthRequestContext, &gwtypes.AuthRequestContext{
			SourceDefName: sourceDefName,
		})
		req = req.WithContext(ctx)
		webhookHandler.RequestHandler(w, req)

		assert.Equal(t, http.StatusOK, w.Result().StatusCode)
		_ = webhookHandler.Shutdown()
	})
}

type mockTransformerServer struct {
	*httptest.Server
	retries             int
	successAfter        int
	successRespBody     []byte
	failureRespBody     []byte
	failureResponseCode int
}

func newMockTransformerServer(successAfter int, successRespBody, failureRespBody []byte, failureResponseCode int) *mockTransformerServer {
	mockServer := &mockTransformerServer{
		successAfter:        successAfter,
		successRespBody:     successRespBody,
		failureRespBody:     failureRespBody,
		failureResponseCode: failureResponseCode,
		retries:             0,
	}
	handler := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mockServer.retries < mockServer.successAfter {
				mockServer.retries++
				w.WriteHeader(mockServer.failureResponseCode)
				_, _ = w.Write(mockServer.failureRespBody)
			} else {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(mockServer.successRespBody)
			}
		}))
	mockServer.Server = handler
	return mockServer
}
