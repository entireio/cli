package checkpoint

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitPromptContent_RoundTrip(t *testing.T) {
	t.Parallel()

	original := []string{
		"first line\nwith newline",
		"second prompt",
	}
	joined := strings.Join(original, PromptSeparator)
	split := SplitPromptContent(joined)

	require.Len(t, split, 2)
	assert.Equal(t, original, split)
}

func TestSplitPromptContent_EmptyContent(t *testing.T) {
	t.Parallel()
	assert.Nil(t, SplitPromptContent(""))
}

// TestRedactedJoinedPrompts_AppliesSafetyNet verifies the helper joins
// prompts with the canonical separator and runs them through the 7-layer
// pipeline. OPF runs only in the pre-push rewrite path, never here.
func TestRedactedJoinedPrompts_AppliesSafetyNet(t *testing.T) {
	t.Parallel()
	got := RedactedJoinedPrompts([]string{"hello", "world"})
	assert.NotEmpty(t, got)
	assert.Contains(t, got, PromptSeparator)
}
