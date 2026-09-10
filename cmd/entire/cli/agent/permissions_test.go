package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// perms builds a rawPermissions map from a JSON object literal.
func perms(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &m))
	return m
}

// denyOf reads back permissions.deny, or nil when the key is gone.
func denyOf(t *testing.T, m map[string]json.RawMessage) []string {
	t.Helper()
	raw, ok := m["deny"]
	if !ok {
		return nil
	}
	var rules []string
	require.NoError(t, json.Unmarshal(raw, &rules))
	return rules
}

func TestRemoveMetadataDenyRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		wantChanged bool
		wantDeny    []string
		// wantDenyAbsent asserts the key itself is gone, which is stronger than
		// an empty slice: an empty `deny` array left behind is config litter.
		wantDenyAbsent bool
	}{
		{
			name:           "sole rule removes the whole key",
			input:          `{"deny":["Read(./.entire/metadata/**)"]}`,
			wantChanged:    true,
			wantDenyAbsent: true,
		},
		{
			name:        "user rules survive",
			input:       `{"deny":["Bash(rm -rf *)","Read(./.entire/metadata/**)","Read(./.env)"]}`,
			wantChanged: true,
			wantDeny:    []string{"Bash(rm -rf *)", "Read(./.env)"},
		},
		{
			name:        "absent rule is a no-op",
			input:       `{"deny":["Bash(rm -rf *)"]}`,
			wantChanged: false,
			wantDeny:    []string{"Bash(rm -rf *)"},
		},
		{
			name:           "no deny key at all",
			input:          `{"allow":["Read(**)"]}`,
			wantChanged:    false,
			wantDenyAbsent: true,
		},
		{
			// Duplicates are possible in a hand-edited config; every copy goes.
			name:           "every duplicate is removed",
			input:          `{"deny":["Read(./.entire/metadata/**)","Read(./.entire/metadata/**)"]}`,
			wantChanged:    true,
			wantDenyAbsent: true,
		},
		{
			// A near-miss is somebody else's rule. Only the exact string Entire
			// wrote is ours to delete.
			name:        "a similar-but-different rule is left alone",
			input:       `{"deny":["Read(./.entire/metadata/**/*)","Read(.entire/metadata/**)"]}`,
			wantChanged: false,
			wantDeny:    []string{"Read(./.entire/metadata/**/*)", "Read(.entire/metadata/**)"},
		},
		{
			// Not a string array, so not ours to interpret — rewriting what we
			// cannot read is worse than leaving it.
			name:        "unparseable deny is left untouched",
			input:       `{"deny":{"weird":true}}`,
			wantChanged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := perms(t, tt.input)
			changed, err := RemoveMetadataDenyRule(m)
			require.NoError(t, err)
			assert.Equal(t, tt.wantChanged, changed)

			if tt.wantDenyAbsent {
				_, present := m["deny"]
				assert.False(t, present, "permissions.deny should be removed, not left empty")
				return
			}
			if tt.wantDeny != nil {
				assert.Equal(t, tt.wantDeny, denyOf(t, m))
			}
		})
	}
}

// TestRemoveMetadataDenyRule_PreservesSiblingKeys pins that removal touches only
// `deny`: ask/allow and unknown fields belong to the user or to a newer Claude
// Code, and a migration that eats them is a far worse bug than the one it fixes.
func TestRemoveMetadataDenyRule_PreservesSiblingKeys(t *testing.T) {
	t.Parallel()

	m := perms(t, `{
	  "allow": ["Read(**)"],
	  "ask": ["Bash(*)"],
	  "deny": ["Read(./.entire/metadata/**)"],
	  "customField": {"nested": "value"}
	}`)

	changed, err := RemoveMetadataDenyRule(m)
	require.NoError(t, err)
	assert.True(t, changed)

	for _, key := range []string{"allow", "ask", "customField"} {
		assert.Contains(t, m, key, "sibling permission key %q must survive", key)
	}
	assert.NotContains(t, m, "deny")
}

func TestHasMetadataDenyRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"present alone", `{"deny":["Read(./.entire/metadata/**)"]}`, true},
		{"present among others", `{"deny":["Bash(x)","Read(./.entire/metadata/**)"]}`, true},
		{"absent", `{"deny":["Bash(x)"]}`, false},
		{"no deny key", `{"allow":["Read(**)"]}`, false},
		{"unparseable deny", `{"deny":"nope"}`, false},
		{"near miss", `{"deny":["Read(.entire/metadata/**)"]}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, HasMetadataDenyRule(perms(t, tt.input)))
		})
	}
}

// TestHasMetadataDenyRule_DoesNotMutate keeps the detector safe for the
// diagnostics that call it on a config they must not touch (the SessionStart
// notice reports, it does not repair).
func TestHasMetadataDenyRule_DoesNotMutate(t *testing.T) {
	t.Parallel()

	const body = `{"deny":["Read(./.entire/metadata/**)","Bash(x)"]}`
	m := perms(t, body)
	require.True(t, HasMetadataDenyRule(m))
	assert.Equal(t, []string{"Read(./.entire/metadata/**)", "Bash(x)"}, denyOf(t, m))
}

// TestMetadataDenyRuleString pins the exact constant. It is a migration key:
// removal matches on this string, so a repo written by an older CLI only heals
// while it stays byte-identical to what that CLI wrote.
func TestMetadataDenyRuleString(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "Read(./.entire/metadata/**)", MetadataDenyRule)
}
