package recap

import "testing"

func TestCategorizeTool(t *testing.T) {
	t.Parallel()
	cases := []struct {
		toolName, bashCommand, want string
	}{
		{"Bash", "ls -la", "shell"},
		{"Bash", "git commit -m 'foo'", "commit"},
		{"Bash", "git push origin main", "commit"},
		{"Bash", "git status", "shell"},
		{"Read", "", "fileOps"},
		{"Write", "", "fileOps"},
		{"Edit", "", "fileOps"},
		{"NotebookEdit", "", "fileOps"},
		{"Grep", "", "search"},
		{"Glob", "", "search"},
		{"mcp__anything", "", "mcp"},
		{"Agent", "", "agent"},
		{"Skill", "", "agent"},
		{"UnknownTool", "", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.toolName+"_"+tc.bashCommand, func(t *testing.T) {
			t.Parallel()
			got := CategorizeTool(tc.toolName, tc.bashCommand)
			if got != tc.want {
				t.Errorf("CategorizeTool(%q, %q) = %q, want %q",
					tc.toolName, tc.bashCommand, got, tc.want)
			}
		})
	}
}

func TestCategorizeTool_EmptyToolName(t *testing.T) {
	t.Parallel()
	if got := CategorizeTool("", ""); got != "other" {
		t.Errorf("empty tool name should categorize as 'other', got %q", got)
	}
}
