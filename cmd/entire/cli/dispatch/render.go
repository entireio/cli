package dispatch

import "strings"

func RenderMarkdown(dispatch *Dispatch) string {
	if dispatch == nil {
		return ""
	}
	return strings.TrimSpace(dispatch.GeneratedText) + "\n"
}
