package opencode

// OpenCode 2 exports a session as flat, typed messages. Everything downstream
// of the export reads the OpenCode 1 shape, so a 2.x export is converted to
// that shape once, when it is captured, by opencodev2.Normalize, which the
// transcript library shares.

// v2String reads m[key] as a string, "" when absent or not a string.
func v2String(m map[string]any, key string) string {
	s, ok := m[key].(string)
	if !ok {
		return ""
	}
	return s
}
