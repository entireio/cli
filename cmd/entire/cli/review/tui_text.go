package review

import "github.com/entireio/cli/cmd/entire/cli/tuiutil"

func stripANSI(s string) string                  { return tuiutil.StripANSI(s) }
func sanitizeDisplayText(s string) string        { return tuiutil.SanitizeDisplayText(s) }
func padDisplayWidth(s string, width int) string { return tuiutil.PadDisplayWidth(s, width) }
func padDisplayWidthWith(s string, width int, pad string) string {
	return tuiutil.PadDisplayWidthWith(s, width, pad)
}
func truncateDisplayWidth(s string, width int) string { return tuiutil.TruncateDisplayWidth(s, width) }
func wrapDisplayWidth(s string, width int) []string   { return tuiutil.WrapDisplayWidth(s, width) }
