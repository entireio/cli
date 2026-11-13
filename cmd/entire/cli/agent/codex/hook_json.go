package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// Codex hook entries and matcher groups are rewritten whenever Entire installs
// or removes its own hooks, in files that also hold the user's hooks. These
// methods keep every field Entire does not model, so that rewrite never drops
// a user's timeoutSec, async, statusMessage, or a field Codex adds later.

type matcherGroupFields struct {
	Matcher *string     `json:"matcher"`
	Hooks   []HookEntry `json:"hooks"`
}

type hookEntryFields struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// UnmarshalJSON implements json.Unmarshaler, keeping unmodeled fields.
func (g *MatcherGroup) UnmarshalJSON(data []byte) error {
	var known matcherGroupFields
	if err := json.Unmarshal(data, &known); err != nil {
		return err //nolint:wrapcheck // callers name the hook event
	}
	extra, err := unmodeledFields(data, "matcher", "hooks")
	if err != nil {
		return err
	}
	*g = MatcherGroup{Matcher: known.Matcher, Hooks: known.Hooks, extra: extra}
	return nil
}

// MarshalJSON implements json.Marshaler: modeled fields first, then the
// preserved ones in key order.
func (g MatcherGroup) MarshalJSON() ([]byte, error) {
	return marshalWithExtra(matcherGroupFields{Matcher: g.Matcher, Hooks: g.Hooks}, g.extra)
}

// UnmarshalJSON implements json.Unmarshaler, keeping unmodeled fields.
func (e *HookEntry) UnmarshalJSON(data []byte) error {
	var known hookEntryFields
	if err := json.Unmarshal(data, &known); err != nil {
		return err //nolint:wrapcheck // callers name the hook event
	}
	extra, err := unmodeledFields(data, "type", "command", "timeout")
	if err != nil {
		return err
	}
	*e = HookEntry{Type: known.Type, Command: known.Command, Timeout: known.Timeout, extra: extra}
	return nil
}

// MarshalJSON implements json.Marshaler: modeled fields first, then the
// preserved ones in key order.
func (e HookEntry) MarshalJSON() ([]byte, error) {
	return marshalWithExtra(hookEntryFields{Type: e.Type, Command: e.Command, Timeout: e.Timeout}, e.extra)
}

func unmodeledFields(data []byte, modeled ...string) (map[string]json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err //nolint:wrapcheck // callers name the hook event
	}
	for _, name := range modeled {
		delete(all, name)
	}
	if len(all) == 0 {
		all = nil // keep entries without extra fields comparable to literals
	}
	return all, nil
}

func marshalWithExtra(known any, extra map[string]json.RawMessage) ([]byte, error) {
	out, err := jsonutil.MarshalWithNoHTMLEscape(known)
	if err != nil || len(extra) == 0 {
		return out, err //nolint:wrapcheck // jsonutil names the failure
	}
	var buf bytes.Buffer
	buf.Write(bytes.TrimSuffix(out, []byte("}")))
	for _, name := range slices.Sorted(maps.Keys(extra)) {
		key, err := jsonutil.MarshalWithNoHTMLEscape(name)
		if err != nil {
			return nil, err //nolint:wrapcheck // jsonutil names the failure
		}
		fmt.Fprintf(&buf, ",%s:%s", key, extra[name])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
