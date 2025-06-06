package rules

import (
	"fmt"
	"strings"

	"github.com/samber/lo"

	"github.com/rudderlabs/rudder-server/processor/internal/transformer/destination_transformer/embedded/warehouse/internal/response"
	wtypes "github.com/rudderlabs/rudder-server/processor/internal/transformer/destination_transformer/embedded/warehouse/internal/types"
	"github.com/rudderlabs/rudder-server/processor/internal/transformer/destination_transformer/embedded/warehouse/internal/utils"
	"github.com/rudderlabs/rudder-server/processor/types"
	"github.com/rudderlabs/rudder-server/utils/misc"
)

type Rules func(event *wtypes.TransformerEvent) (any, error)

var (
	DefaultRules = map[string]Rules{
		"id":                 messageIDFromEvent(),
		"anonymous_id":       staticRule("anonymousId"),
		"user_id":            staticRule("userId"),
		"sent_at":            staticRule("sentAt"),
		"timestamp":          staticRule("timestamp"),
		"received_at":        receivedAtFromEvent(),
		"original_timestamp": staticRule("originalTimestamp"),
		"channel":            staticRule("channel"),
		"context_ip": func(event *wtypes.TransformerEvent) (any, error) {
			return firstValidValue(event.Message, []string{"context.ip", "request_ip"}), nil
		},
		"context_request_ip": staticRule("request_ip"),
		"context_passed_ip":  staticRule("context.ip"),
	}

	TrackRules = map[string]Rules{
		"event_text": staticRule("event"),
	}
	TrackEventTableRules = map[string]Rules{
		"id": func(event *wtypes.TransformerEvent) (any, error) {
			eventType := event.Metadata.EventType
			canUseRecordID := utils.CanUseRecordID(event.Metadata.SourceCategory)
			if eventType == "track" && canUseRecordID {
				return extractCloudRecordID(event.Message, &event.Metadata, event.Metadata.MessageID)
			}
			return event.Metadata.MessageID, nil
		},
	}
	TrackTableRules = map[string]Rules{
		"record_id": func(event *wtypes.TransformerEvent) (any, error) {
			eventType := event.Metadata.EventType
			canUseRecordID := utils.CanUseRecordID(event.Metadata.SourceCategory)
			if eventType == "track" && canUseRecordID {
				cr, err := extractCloudRecordID(event.Message, &event.Metadata, nil)
				if err != nil {
					return nil, fmt.Errorf("extracting cloud record id: %w", err)
				}
				return utils.ToString(cr), nil
			}
			return nil, nil // nolint: nilnil
		},
	}

	IdentifyRules = map[string]Rules{
		"context_ip": func(event *wtypes.TransformerEvent) (any, error) {
			return firstValidValue(event.Message, []string{"context.ip", "request_ip"}), nil
		},
		"context_request_ip": staticRule("request_ip"),
		"context_passed_ip":  staticRule("context.ip"),
	}
	IdentifyRulesNonDataLake = map[string]Rules{
		"context_ip": func(event *wtypes.TransformerEvent) (any, error) {
			return firstValidValue(event.Message, []string{"context.ip", "request_ip"}), nil
		},
		"context_request_ip": staticRule("request_ip"),
		"context_passed_ip":  staticRule("context.ip"),
		"sent_at":            staticRule("sentAt"),
		"timestamp":          staticRule("timestamp"),
		"original_timestamp": staticRule("originalTimestamp"),
	}

	PageRules = map[string]Rules{
		"name": func(event *wtypes.TransformerEvent) (any, error) {
			return firstValidValue(event.Message, []string{"name", "properties.name"}), nil
		},
	}

	ScreenRules = map[string]Rules{
		"name": func(event *wtypes.TransformerEvent) (any, error) {
			return firstValidValue(event.Message, []string{"name", "properties.name"}), nil
		},
	}

	AliasRules = map[string]Rules{
		"previous_id": staticRule("previousId"),
	}

	GroupRules = map[string]Rules{
		"group_id": staticRule("groupId"),
	}

	ExtractRules = map[string]Rules{
		"id": func(event *wtypes.TransformerEvent) (any, error) {
			return extractRecordID(&event.Metadata)
		},
		"received_at": receivedAtFromEvent(),
		"event":       staticRule("event"),
	}
)

func staticRule(key string) Rules {
	return func(event *wtypes.TransformerEvent) (any, error) {
		return misc.MapLookup(event.Message, strings.Split(key, ".")...), nil
	}
}

func messageIDFromEvent() Rules {
	return func(event *wtypes.TransformerEvent) (any, error) {
		return event.Metadata.MessageID, nil
	}
}

func receivedAtFromEvent() Rules {
	return func(event *wtypes.TransformerEvent) (any, error) {
		return event.Metadata.ReceivedAt, nil
	}
}

var rudderReservedColumns = map[string]map[string]struct{}{
	"track":    createReservedColumns(DefaultRules, TrackRules, TrackTableRules, TrackEventTableRules),
	"identify": createReservedColumns(DefaultRules, IdentifyRules),
	"page":     createReservedColumns(DefaultRules, PageRules),
	"screen":   createReservedColumns(DefaultRules, ScreenRules),
	"group":    createReservedColumns(DefaultRules, GroupRules),
	"alias":    createReservedColumns(DefaultRules, AliasRules),
	"extract":  createReservedColumns(ExtractRules),
}

func createReservedColumns(rules ...map[string]Rules) map[string]struct{} {
	return lo.MapEntries(lo.Assign(rules...), func(key string, _ Rules) (string, struct{}) {
		return key, struct{}{}
	})
}

func firstValidValue(message map[string]any, props []string) any {
	for _, prop := range props {
		propKeys := strings.Split(prop, ".")
		if val := misc.MapLookup(message, propKeys...); !utils.IsEmptyString(val) {
			return val
		}
	}
	return nil
}

func extractRecordID(metadata *wtypes.Metadata) (any, error) {
	if utils.IsEmptyString(metadata.RecordID) {
		return nil, response.ErrRecordIDEmpty
	}
	if utils.IsObject(metadata.RecordID) {
		return nil, response.ErrRecordIDObject
	}
	if utils.IsArray(metadata.RecordID) {
		return nil, response.ErrRecordIDArray
	}
	return metadata.RecordID, nil
}

func extractCloudRecordID(message types.SingularEventT, metadata *wtypes.Metadata, fallbackValue any) (any, error) {
	if sv := misc.MapLookup(message, "context", "sources", "version"); !utils.IsEmptyString(sv) {
		return extractRecordID(metadata)
	}
	return fallbackValue, nil
}

func IsRudderReservedColumn(eventType, columnName string) bool {
	lowerEventType := strings.ToLower(eventType)
	if _, ok := rudderReservedColumns[lowerEventType]; !ok {
		return false
	}
	lowerColumnName := strings.ToLower(columnName)
	if _, ok := rudderReservedColumns[lowerEventType][lowerColumnName]; ok {
		return true
	}
	return false
}
