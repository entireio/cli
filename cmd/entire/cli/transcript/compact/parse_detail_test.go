package compact

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildCondensedEntries_ToolDetail pins the raw ToolDetail condensation
// stores for a tool_use block: the FIRST non-empty string among description,
// command, file_path, filePath, path, pattern — verbatim. Checkpoints already
// carry these strings, so the table must keep passing byte-for-byte across any
// refactor of how the input is decoded.
func TestBuildCondensedEntries_ToolDetail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string // JSON of the tool_use "input" object
		want  string
	}{
		{name: "description alone", input: `{"description":"list files"}`, want: "list files"},
		{name: "command alone", input: `{"command":"ls -la"}`, want: "ls -la"},
		{name: "file_path alone", input: `{"file_path":"src/main.go"}`, want: "src/main.go"},
		{name: "filePath alone (Cursor)", input: `{"filePath":"src/main.go"}`, want: "src/main.go"},
		{name: "path alone (OpenCode, Pi)", input: `{"path":"src/main.go"}`, want: "src/main.go"},
		{name: "pattern alone", input: `{"pattern":"TODO"}`, want: "TODO"},
		{
			name:  "description wins over everything",
			input: `{"description":"d","command":"c","file_path":"f","filePath":"fc","path":"p","pattern":"pt"}`,
			want:  "d",
		},
		{name: "command wins over file_path", input: `{"command":"c","file_path":"f"}`, want: "c"},
		{name: "file_path wins over filePath and path", input: `{"file_path":"f","filePath":"fc","path":"p"}`, want: "f"},
		{name: "filePath wins over path", input: `{"filePath":"fc","path":"p"}`, want: "fc"},
		{name: "path wins over pattern", input: `{"path":"p","pattern":"pt"}`, want: "p"},
		{name: "empty string is skipped", input: `{"description":"","command":"c"}`, want: "c"},
		{name: "non-string under a matching key is skipped", input: `{"command":42,"file_path":"f"}`, want: "f"},
		{name: "non-string alone yields empty", input: `{"command":42}`, want: ""},
		{name: "object under a matching key is skipped", input: `{"description":{"x":1},"pattern":"pt"}`, want: "pt"},
		{name: "null under a matching key is skipped", input: `{"command":null,"pattern":"pt"}`, want: "pt"},
		{name: "unrelated keys yield empty", input: `{"url":"https://example.com","prompt":"p"}`, want: ""},
		{name: "empty object yields empty", input: `{}`, want: ""},
		{name: "null input yields empty", input: `null`, want: ""},
		{name: "non-object input yields empty", input: `"just a string"`, want: ""},
		{name: "array input yields empty", input: `["a","b"]`, want: ""},
		// Keys match exactly: case variants of a detail key are not details.
		{name: "Command (capitalised) is not a key", input: `{"Command":"ls"}`, want: ""},
		{name: "filepath (lowercase) is not a key", input: `{"filepath":"x"}`, want: ""},
		{name: "FILE_PATH (uppercase) is not a key", input: `{"FILE_PATH":"x"}`, want: ""},
		{name: "Path (capitalised) is not a key", input: `{"Path":"p"}`, want: ""},
		{name: "case variant is skipped, exact key later in order wins", input: `{"Command":"ls","pattern":"pt"}`, want: "pt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			line := `{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","content":[{"type":"tool_use","name":"Tool","input":` + tt.input + `}]}` + "\n"
			entries, err := BuildCondensedEntries([]byte(line))
			require.NoError(t, err)
			require.Len(t, entries, 1)
			assert.Equal(t, "tool", entries[0].Type)
			assert.Equal(t, "Tool", entries[0].ToolName)
			assert.Equal(t, tt.want, entries[0].ToolDetail)
		})
	}
}

// TestBuildCondensedEntries_ToolDetailMissingInput pins that a tool_use block
// with no input key at all still condenses, with an empty ToolDetail.
func TestBuildCondensedEntries_ToolDetailMissingInput(t *testing.T) {
	t.Parallel()

	line := `{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","content":[{"type":"tool_use","name":"Tool"}]}` + "\n"
	entries, err := BuildCondensedEntries([]byte(line))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Empty(t, entries[0].ToolDetail)
}
