package compact

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLines_ParsesCompactTranscript(t *testing.T) {
	t.Parallel()

	input := []byte(
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","content":[{"text":"hello"}]}` + "\n" +
			`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"Read","input":{"filePath":"a.txt"}}]}` + "\n",
	)

	lines, err := parseLines(input)
	require.NoError(t, err)
	require.Len(t, lines, 2)

	assert.Equal(t, 1, lines[0].V)
	assert.Equal(t, "user", lines[0].Type)

	var userBlocks []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(lines[0].Content, &userBlocks))
	require.Len(t, userBlocks, 1)
	var userText string
	require.NoError(t, json.Unmarshal(userBlocks[0]["text"], &userText))
	assert.Equal(t, "hello", userText)

	assert.Equal(t, "assistant", lines[1].Type)

	var assistantBlocks []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(lines[1].Content, &assistantBlocks))
	require.Len(t, assistantBlocks, 2)

	var blockType string
	require.NoError(t, json.Unmarshal(assistantBlocks[0]["type"], &blockType))
	assert.Equal(t, "text", blockType)
	var assistantText string
	require.NoError(t, json.Unmarshal(assistantBlocks[0]["text"], &assistantText))
	assert.Equal(t, "hi", assistantText)

	require.NoError(t, json.Unmarshal(assistantBlocks[1]["type"], &blockType))
	assert.Equal(t, "tool_use", blockType)
	var toolName string
	require.NoError(t, json.Unmarshal(assistantBlocks[1]["name"], &toolName))
	assert.Equal(t, "Read", toolName)
}

func TestParseLines_RejectsNonCompactLine(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"user","content":"hello"}` + "\n")

	_, err := parseLines(input)
	require.Error(t, err)
}

func TestParseLines_RejectsMalformedWireFields(t *testing.T) {
	t.Parallel()

	for _, field := range []string{
		`"agent":42`,
		`"id":42`,
		`"input_tokens":"invalid"`,
		`"output_tokens":"invalid"`,
	} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			input := []byte(`{"v":1,"cli_version":"0.5.1","type":"assistant","content":[{"type":"text","text":"hi"}],` + field + "}\n")
			_, err := parseLines(input)
			require.Error(t, err)
		})
	}
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
