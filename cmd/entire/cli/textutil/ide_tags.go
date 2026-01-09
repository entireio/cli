package textutil

import (
	"regexp"
	"strings"
)

// ideContextTagRegex matches IDE-injected context tags like <ide_opened_file>...</ide_opened_file>
// and <ide_selection>...</ide_selection>. These are injected by the VSCode extension.
var ideContextTagRegex = regexp.MustCompile(`(?s)<ide_[^>]*>.*?</ide_[^>]*>`)

// systemTagRegexes matches system-injected context tags that shouldn't appear in user-facing text.
// Each tag type needs its own regex since Go's regexp doesn't support backreferences.
var systemTagRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?s)<local-command-caveat[^>]*>.*?</local-command-caveat>`),
	regexp.MustCompile(`(?s)<system-reminder[^>]*>.*?</system-reminder>`),
	regexp.MustCompile(`(?s)<command-name[^>]*>.*?</command-name>`),
	regexp.MustCompile(`(?s)<command-message[^>]*>.*?</command-message>`),
	regexp.MustCompile(`(?s)<command-args[^>]*>.*?</command-args>`),
	regexp.MustCompile(`(?s)<local-command-stdout[^>]*>.*?</local-command-stdout>`),
}

// StripIDEContextTags removes IDE-injected context tags from prompt text.
// The VSCode extension injects tags like:
//   - <ide_opened_file>...</ide_opened_file> - currently open file
//   - <ide_selection>...</ide_selection> - selected code in editor
//
// Also removes system-injected tags like:
//   - <local-command-caveat>...</local-command-caveat>
//   - <system-reminder>...</system-reminder>
//   - <command-name>...</command-name>
//
// These shouldn't appear in commit messages or session descriptions.
func StripIDEContextTags(text string) string {
	result := ideContextTagRegex.ReplaceAllString(text, "")
	for _, re := range systemTagRegexes {
		result = re.ReplaceAllString(result, "")
	}
	return strings.TrimSpace(result)
}
