package agent

import "encoding/json"

// SafeErrorMessage decodes operational error text from a raw NDJSON line
// without leaking the raw line itself.
//
// Stream parsers MUST NOT propagate raw protocol lines into error messages:
// CLI stderr/stdout can carry echoed user content or model-message
// fragments, which would then surface in logs, telemetry, and user-facing
// error output. This helper partially decodes the line against the three
// commonly-observed message paths (`message`, `error.message`,
// `data.message`) and falls back to "unspecified error" if none match.
//
// Callers pass the raw scanner line. Decode errors are tolerated: if the
// line is malformed or has none of these paths, the result is the safe
// sentinel rather than the raw bytes.
func SafeErrorMessage(line []byte) string {
	var details struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	_ = json.Unmarshal(line, &details) //nolint:errcheck // partial decode tolerated; raw line MUST NOT leak
	switch {
	case details.Message != "":
		return details.Message
	case details.Error.Message != "":
		return details.Error.Message
	case details.Data.Message != "":
		return details.Data.Message
	default:
		return "unspecified error"
	}
}
