package orcarouter

import (
	"os"
)

// Environment variable names for the OrcaRouter gateway.
const (
	// EnvAPIKey is the API key OrcaRouter issues (starts with "sk-orca-").
	EnvAPIKey = "ORCAROUTER_API_KEY" //nolint:gosec // G101: env var name, not a credential
	// EnvAPIBaseURL overrides the default gateway endpoint. It is optional;
	// the default is the production gateway.
	EnvAPIBaseURL = "ORCAROUTER_API_BASE_URL"
)

// defaultAPIBaseURL is the OrcaRouter OpenAI-compatible endpoint. Clients that
// speak the OpenAI protocol append /chat/completions to this base.
const defaultAPIBaseURL = "https://api.orcarouter.ai/v1"

// APIKey returns the configured OrcaRouter API key, or "" when unset.
func APIKey() string {
	return os.Getenv(EnvAPIKey)
}

// APIBaseURL returns the configured gateway base URL, or the production
// default when unset. The trailing slash is trimmed so callers can append
// path segments with a single "/".
func APIBaseURL() string {
	base := os.Getenv(EnvAPIBaseURL)
	if base == "" {
		base = defaultAPIBaseURL
	}
	return base
}
