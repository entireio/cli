package ticket

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

func TestCredService(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "entire-ticket:linear", credService(PlatformLinear))
}

// TestSaveLoadToken_RoundTrip uses the file-backed token store (never the real
// OS keychain) so it is hermetic and CI-safe. It cannot run in parallel because
// UseFileBackendForTesting swaps the package-global backend.
func TestSaveLoadToken_RoundTrip(t *testing.T) {
	cleanup := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	defer cleanup()

	require.NoError(t, SaveToken(PlatformLinear, "lin_api_test"))

	got, err := LoadToken(PlatformLinear)
	require.NoError(t, err)
	assert.Equal(t, "lin_api_test", got)
}

func TestDeleteToken(t *testing.T) {
	cleanup := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	defer cleanup()

	require.NoError(t, SaveToken(PlatformLinear, "lin_api_test"))
	require.NoError(t, DeleteToken(PlatformLinear))

	_, err := LoadToken(PlatformLinear)
	require.Error(t, err, "credential should be gone after DeleteToken")
}
