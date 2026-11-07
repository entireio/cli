package opencode

import transcriptlib "github.com/entireio/cli/transcript"

// OpenCode 2 exports a session as flat, typed messages. Everything downstream
// of the export reads the OpenCode 1 shape, so a 2.x export is converted to
// that shape once, when it is captured. The conversion lives in the
// transcript library (transcript.NormalizeOpenCodeExport), which a live
// stream of an OpenCode session shares.

// openCodeNativeKey is the message info key that keeps the OpenCode 2
// messages a converted message came from, verbatim; resume gives them back
// to OpenCode 2 (see openCodeV2ImportPayload).
const openCodeNativeKey = transcriptlib.OpenCodeNativeKey

// isOpenCodeV2Export reports whether data is an OpenCode 2 export.
func isOpenCodeV2Export(data []byte) bool {
	return transcriptlib.IsOpenCodeV2Export(data)
}

// normalizeOpenCodeExport converts an OpenCode 2 export to the OpenCode 1
// shape and returns other payloads unchanged (transcript.NormalizeOpenCodeExport).
func normalizeOpenCodeExport(data []byte) ([]byte, error) {
	return transcriptlib.NormalizeOpenCodeExport(data) //nolint:wrapcheck // a thin alias
}

// v2String reads m[key] as a string, "" when absent or not a string.
func v2String(m map[string]any, key string) string {
	s, ok := m[key].(string)
	if !ok {
		return ""
	}
	return s
}
