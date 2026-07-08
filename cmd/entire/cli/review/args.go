package review

import "strings"

// AppendModelFlag appends a standard --model flag pair when model is non-empty.
// Review runner adapters share this so model override argv handling stays
// identical across claude-code, codex, and gemini.
func AppendModelFlag(args []string, model string) []string {
	if model = strings.TrimSpace(model); model != "" {
		args = append(args, "--model", model)
	}
	return args
}
