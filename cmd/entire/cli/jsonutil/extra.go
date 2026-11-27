package jsonutil

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
)

// UnmarshalKeepingExtra decodes the knownKeys of data into known and returns
// every other key, plus the knownKeys data contained. It is for config files
// Entire rewrites but another tool owns (an agent's settings or hooks file), where
// every field Entire does not model must survive the round trip.
//
// Only exact knownKeys are decoded: encoding/json matches field names
// case-insensitively, so decoding data whole would read a key such as "Command",
// which the owning tool ignores, into the live command field.
func UnmarshalKeepingExtra(data []byte, known any, knownKeys ...string) (extra map[string]json.RawMessage, present []string, err error) {
	if err := json.Unmarshal(data, &extra); err != nil {
		return nil, nil, err //nolint:wrapcheck // returned from UnmarshalJSON; encoding/json adds context
	}
	modeled := make(map[string]json.RawMessage, len(knownKeys))
	for _, k := range knownKeys {
		if v, ok := extra[k]; ok {
			modeled[k] = v
			present = append(present, k)
			delete(extra, k)
		}
	}
	modeledJSON, err := json.Marshal(modeled)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // returned from UnmarshalJSON; encoding/json adds context
	}
	if err := json.Unmarshal(modeledJSON, known); err != nil {
		return nil, nil, err //nolint:wrapcheck // returned from UnmarshalJSON; encoding/json adds context
	}
	return extra, present, nil
}

// MarshalWithExtra encodes known, then appends extra in key order, so modeled
// fields keep their usual position and output stays deterministic. It is the
// write half of UnmarshalKeepingExtra.
func MarshalWithExtra(known any, extra map[string]json.RawMessage) ([]byte, error) {
	data, err := MarshalWithNoHTMLEscape(known)
	if err != nil || len(extra) == 0 {
		return data, err
	}
	var buf bytes.Buffer
	buf.Write(data[:len(data)-1])
	needComma := len(data) > len("{}")
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		key, err := MarshalWithNoHTMLEscape(k)
		if err != nil {
			return nil, err
		}
		if needComma {
			buf.WriteByte(',')
		}
		needComma = true
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(extra[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
