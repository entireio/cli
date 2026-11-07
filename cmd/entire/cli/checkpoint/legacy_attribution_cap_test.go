package checkpoint

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oversizedPromptAttributions builds a syntactically valid prompt_attributions
// array of at least n bytes, shaped like the records older CLIs wrote.
func oversizedPromptAttributions(n int) json.RawMessage {
	const elem = `{"checkpoint_number":1,"user_lines_added":3,"agent_lines_added":12}`
	count := n/(len(elem)+1) + 1
	return json.RawMessage("[" + strings.Repeat(elem+",", count) + elem + "]")
}

func TestCapLegacyPromptAttributions(t *testing.T) {
	t.Parallel()
	small := json.RawMessage(`[{"checkpoint_number":1,"user_lines_added":3}]`)
	atCap := json.RawMessage(strings.Repeat("x", MaxLegacyPromptAttributionsBytes))
	overCap := json.RawMessage(strings.Repeat("x", MaxLegacyPromptAttributionsBytes+1))

	for _, tc := range []struct {
		name string
		in   json.RawMessage
		kept bool
	}{
		{"absent stays absent", nil, true},
		{"empty stays empty", json.RawMessage{}, true},
		{"a healthy record is kept", small, true},
		{"exactly at the cap is kept", atCap, true},
		{"one byte over the cap is dropped", overCap, false},
		{"a pathological record is dropped", oversizedPromptAttributions(8 << 20), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CapLegacyPromptAttributions(context.Background(), tc.in, "sess-1")
			if tc.kept {
				assert.Equal(t, tc.in, got, "a value within the cap must pass through byte for byte")
				return
			}
			assert.Nil(t, got, "a value over the cap must be dropped, not truncated")
		})
	}
}

// The cap is what keeps a carried-forward field under GitHub's per-blob limit,
// so it has to leave room for the rest of the metadata document too.
func TestMaxLegacyPromptAttributionsBytesLeavesHeadroom(t *testing.T) {
	t.Parallel()
	const gitHubBlobHardLimit = 100 << 20
	require.Less(t, MaxLegacyPromptAttributionsBytes, gitHubBlobHardLimit/2,
		"the cap must stay well inside the limit it exists to respect")
}
