package compact

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCondensedEntries_RejectsNonCompactInput(t *testing.T) {
	t.Parallel()

	_, err := BuildCondensedEntries([]byte(`{"type":"user","content":"hello"}` + "\n"))
	require.Error(t, err)
}

func TestBuildCondensedEntries_RejectsMalformedLine(t *testing.T) {
	t.Parallel()

	input := []byte(`{"v":1,"type":"user","content":[{"text":"ok"}]}` + "\n" + `{"v":1,"type":"user","agent":42}` + "\n")
	_, err := BuildCondensedEntries(input)
	require.Error(t, err)
}

func TestBuildCondensedEntries_AcceptsLineWithoutCLIVersion(t *testing.T) {
	t.Parallel()

	entries, err := BuildCondensedEntries([]byte(`{"v":1,"type":"user","content":[{"text":"hello"}]}` + "\n"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "hello", entries[0].Content)
}

func TestBuildCondensedEntries_StringContent(t *testing.T) {
	t.Parallel()

	entries, err := BuildCondensedEntries([]byte(`{"v":1,"type":"user","content":"hello"}` + "\n"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "user", entries[0].Type)
	assert.Equal(t, "hello", entries[0].Content)
}

func TestBuildCondensedEntries_EmptyInput(t *testing.T) {
	t.Parallel()

	_, err := BuildCondensedEntries([]byte("\n  \n"))
	require.Error(t, err)
}

func TestBuildCondensedEntries_ParsesCompactTranscript(t *testing.T) {
	t.Parallel()

	input := []byte(
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","content":[{"text":"hello"}]}` + "\n" +
			`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"Read","input":{"filePath":"a.txt"}}]}` + "\n",
	)

	entries, err := BuildCondensedEntries(input)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	assert.Equal(t, "user", entries[0].Type)
	assert.Equal(t, "hello", entries[0].Content)
	assert.Equal(t, "assistant", entries[1].Type)
	assert.Equal(t, "hi", entries[1].Content)
	assert.Equal(t, "tool", entries[2].Type)
	assert.Equal(t, "Read", entries[2].ToolName)
	assert.Equal(t, "a.txt", entries[2].ToolDetail)
}
