package settings

import (
	"bytes"
	stdjson "encoding/json"
	"errors"
	"io"

	"github.com/knadh/koanf/parsers/json"
)

type migrationJSON struct{ json.JSON }

func (parser migrationJSON) Unmarshal(raw []byte) (map[string]any, error) {
	var values map[string]any
	decoder := stdjson.NewDecoder(bytes.NewReader(raw))
	// Default float64 decoding rounds legacy user IDs above 2^53.
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing settings data")
	}
	if values["legacy_migration_id"] == nil {
		return parser.JSON.Unmarshal(raw)
	}
	return preciseNumbers(values).(map[string]any), nil
}

func preciseNumbers(value any) any {
	switch typed := value.(type) {
	case stdjson.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer
		}
		if floating, err := typed.Float64(); err == nil {
			return floating
		}
		return typed.String()
	case map[string]any:
		for key, child := range typed {
			typed[key] = preciseNumbers(child)
		}
	case []any:
		for index, child := range typed {
			typed[index] = preciseNumbers(child)
		}
	}
	return value
}
