package recap

import (
	"regexp"
	"strings"
)

// Tool category names. Must match the api-side taxonomy exactly plus
// two local-only virtual categories (conversation, commit).
const (
	CategoryShell        = "shell"
	CategoryFileOps      = "fileOps"
	CategorySearch       = "search"
	CategoryMCP          = "mcp"
	CategoryAgent        = "agent"
	CategoryOther        = "other"
	CategoryConversation = "conversation"
	CategoryCommit       = "commit"
)

// AllCategories returns the 8 category names in rendering order.
func AllCategories() []string {
	return []string{
		CategoryFileOps,
		CategoryShell,
		CategorySearch,
		CategoryMCP,
		CategoryAgent,
		CategoryConversation,
		CategoryCommit,
		CategoryOther,
	}
}

// commitCmdRe matches bash commands that start with `git commit` or `git push`,
// the signal we use for the virtual "commit" category.
var commitCmdRe = regexp.MustCompile(`^\s*git\s+(commit|push)\b`)

// CategorizeTool returns the category for a tool invocation.
// bashCommand is only inspected when toolName == "Bash".
func CategorizeTool(toolName, bashCommand string) string {
	switch toolName {
	case "Bash":
		if commitCmdRe.MatchString(bashCommand) {
			return CategoryCommit
		}
		return CategoryShell
	case "Read", "Write", "Edit", "NotebookEdit":
		return CategoryFileOps
	case "Grep", "Glob":
		return CategorySearch
	case "Agent", "Skill":
		return CategoryAgent
	}
	if strings.HasPrefix(toolName, "mcp__") {
		return CategoryMCP
	}
	return CategoryOther
}
