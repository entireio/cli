package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode"
)

var rawJSONType = reflect.TypeFor[json.RawMessage]()

// normalizeLegacyTrailJSON accepts the former BFF's snake_case trail payloads
// while the CLI moves to entire-api's camelCase contract. Normalization follows
// the destination schema so map, interface, RawMessage, and unknown fields keep
// their user-defined keys unchanged.
func normalizeLegacyTrailJSON(data []byte, dest any) ([]byte, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("decode trail JSON: %w", err)
	}
	normalized, err := json.Marshal(normalizeTrailJSONValue(value, reflect.TypeOf(dest)))
	if err != nil {
		return nil, fmt.Errorf("encode normalized trail JSON: %w", err)
	}
	return normalized, nil
}

func decodeNormalizedTrailJSON(data []byte, dest any) error {
	normalized, err := normalizeLegacyTrailJSON(data, dest)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(normalized, dest); err != nil {
		return fmt.Errorf("decode normalized trail JSON: %w", err)
	}
	return nil
}

func legacyTrailRequestBody(body any) (any, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal trail request: %w", err)
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("decode trail request: %w", err)
	}
	return snakeCaseTrailJSONValue(value, reflect.TypeOf(body)), nil
}

func snakeCaseTrailJSONValue(value any, typ reflect.Type) any {
	typ = indirectTrailJSONType(typ)
	if typ == nil || typ == rawJSONType {
		return value
	}
	switch v := value.(type) {
	case map[string]any:
		if typ.Kind() != reflect.Struct {
			return value
		}
		out := make(map[string]any, len(v))
		for key, child := range v {
			fieldType, canonical, ok := trailJSONStructField(typ, key)
			if !ok {
				out[key] = child
				continue
			}
			out[lowerCamelToSnake(canonical)] = snakeCaseTrailJSONValue(child, fieldType)
		}
		return out
	case []any:
		if typ.Kind() != reflect.Slice && typ.Kind() != reflect.Array {
			return value
		}
		for i := range v {
			v[i] = snakeCaseTrailJSONValue(v[i], typ.Elem())
		}
	}
	return value
}

func lowerCamelToSnake(value string) string {
	var out strings.Builder
	for i, r := range value {
		if unicode.IsUpper(r) {
			if i > 0 {
				out.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		out.WriteRune(r)
	}
	return out.String()
}

func normalizeTrailJSONValue(value any, typ reflect.Type) any {
	typ = indirectTrailJSONType(typ)
	if typ == nil || typ == rawJSONType {
		return value
	}
	switch v := value.(type) {
	case map[string]any:
		if typ.Kind() != reflect.Struct {
			return value
		}
		out := make(map[string]any, len(v))
		for key, child := range v {
			fieldType, canonical, ok := trailJSONStructField(typ, NormalizeTrailJSONKey(key))
			if !ok {
				// Unknown fields are ignored by encoding/json. Preserve their entire
				// subtree so a future free-form field cannot be silently re-cased.
				out[key] = child
				continue
			}
			out[canonical] = normalizeTrailJSONValue(child, fieldType)
		}
		return out
	case []any:
		if typ.Kind() != reflect.Slice && typ.Kind() != reflect.Array {
			return value
		}
		for i := range v {
			v[i] = normalizeTrailJSONValue(v[i], typ.Elem())
		}
	}
	return value
}

func indirectTrailJSONType(typ reflect.Type) reflect.Type {
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ
}

func trailJSONStructField(typ reflect.Type, key string) (reflect.Type, string, bool) {
	for i := range typ.NumField() {
		field := typ.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" {
			name = field.Name
		}
		if name == "-" || name != key {
			continue
		}
		return field.Type, name, true
	}
	return nil, "", false
}

// NormalizeTrailJSONKey converts a legacy trail key to its canonical entire-api
// spelling. Event and response normalization share this helper so aliases do
// not diverge between SSE and regular JSON decoding.
func NormalizeTrailJSONKey(value string) string {
	camel := snakeToLowerCamel(value)
	if camel == "reviewSessionId" {
		return "reviewId"
	}
	return camel
}

func snakeToLowerCamel(value string) string {
	if !strings.ContainsRune(value, '_') {
		return value
	}
	var out strings.Builder
	upperNext := false
	for _, r := range value {
		if r == '_' {
			upperNext = true
			continue
		}
		if upperNext {
			r = unicode.ToUpper(r)
			upperNext = false
		}
		out.WriteRune(r)
	}
	return out.String()
}
