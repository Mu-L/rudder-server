package marketobulkupload

import (
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
)

func (m *MarketoConfig) UnmarshalJSON(data []byte) error {
	var intermediate intermediateMarketoConfig

	// use fastjson to parse the JSON
	if err := jsonrs.Unmarshal(data, &intermediate); err != nil {
		return err
	}

	m.ClientId = intermediate.ClientId
	m.ClientSecret = intermediate.ClientSecret
	m.MunchkinId = intermediate.MunchkinId
	m.DeduplicationField = intermediate.DeduplicationField
	m.FieldsMapping = make(map[string]string)

	for _, mapping := range intermediate.ColumnFieldsMapping {
		from, fromOk := mapping["from"]
		to, toOk := mapping["to"]
		if fromOk && toOk {
			m.FieldsMapping[from] = to
		}
	}

	return nil
}
