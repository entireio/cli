package main

import (
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// TestMain isolates the credential selectors by absence: an inherited
// ENTIRE_TOKEN or ENTIRE_SERVICE_ACCOUNT would put every resolveCreds and
// userMessage test on the env-credential path.
func TestMain(m *testing.M) {
	for _, name := range auth.CredentialEnvVars() {
		os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
