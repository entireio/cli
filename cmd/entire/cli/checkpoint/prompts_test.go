package checkpoint

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJoinAndSplitPrompts_RoundTrip(t *testing.T) {
	t.Parallel()

	original := []string{
		"first line\nwith newline",
		"second prompt",
	}

	joined := JoinPrompts(original)
	split := SplitPromptContent(joined)

	require.Len(t, split, 2)
	assert.Equal(t, original, split)
}

func TestSplitPromptContent_EmptyContent(t *testing.T) {
	t.Parallel()

	assert.Nil(t, SplitPromptContent(""))
}
