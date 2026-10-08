package session

import "github.com/entireio/cli/cmd/entire/cli/stringutil"

// MaxLastPromptRunes is the maximum rune length for LastPrompt stored in
// session state.
const MaxLastPromptRunes = 100

// TruncatePromptForStorage collapses whitespace and truncates a user prompt for
// storage in State.LastPrompt. LastPrompt is a display/preview field only — the
// full prompt is preserved in the session transcript — so bounding it keeps the
// state file and `--json` output small without losing recoverable data.
func TruncatePromptForStorage(prompt string) string {
	return stringutil.TruncateRunes(stringutil.CollapseWhitespace(prompt), MaxLastPromptRunes, "...")
}
