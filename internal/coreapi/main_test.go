package coreapi

import (
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// TestMain isolates the credential selectors by absence: an inherited
// ENTIRE_TOKEN or ENTIRE_SERVICE_ACCOUNT makes New bypass the contexts these
// tests configure.
func TestMain(m *testing.M) {
	for _, name := range auth.CredentialEnvVars() {
		os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
