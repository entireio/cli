package auth

import (
	"os"
	"testing"
)

// TestMain isolates the credential selectors by absence: an inherited
// ENTIRE_TOKEN or ENTIRE_SERVICE_ACCOUNT outranks every stored context and
// would turn context-path tests into env-credential tests (or live requests).
func TestMain(m *testing.M) {
	for _, name := range CredentialEnvVars() {
		os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
