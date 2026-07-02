package checkpoint

import (
	"strings"

	"github.com/entireio/cli/redact"
)

// PromptSeparator is the canonical separator used in prompt.txt when multiple
// prompts are stored in a single file.
const PromptSeparator = "\n\n---\n\n"

// JoinPrompts serializes prompts to prompt.txt format.
func JoinPrompts(prompts []string) string {
	return strings.Join(prompts, PromptSeparator)
}

// SplitPromptContent deserializes prompt.txt content into individual prompts.
func SplitPromptContent(content string) []string {
	if content == "" {
		return nil
	}

	prompts := strings.Split(content, PromptSeparator)
	for len(prompts) > 0 && prompts[len(prompts)-1] == "" {
		prompts = prompts[:len(prompts)-1]
	}
	return prompts
}

// RedactedJoinedPrompts joins prompts and runs the 7-layer redaction
// pipeline. OPF runs exclusively in the pre-push rewrite (not here),
// so the writer's hot path stays predictable. Exported so alternate
// persistent backends produce identically-redacted prompt blobs.
func RedactedJoinedPrompts(prompts []string) string {
	return redact.String(strings.Join(prompts, PromptSeparator))
}
