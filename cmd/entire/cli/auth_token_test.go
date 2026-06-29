package cli

import (
	"bytes"
	"encoding/base64"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// makeTestJWT builds an unsigned JWT with the given payload JSON. ParseClaims
// (used by ENTIRE_TOKEN resolution) reads the payload without verifying the
// signature, so an unsigned token is enough to exercise the resolution path.
func makeTestJWT(t *testing.T, payloadJSON string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := enc.EncodeToString([]byte(payloadJSON))
	return header + "." + payload + "." + enc.EncodeToString([]byte("sig"))
}

// TestAuthTokenCmd covers the hidden `entire auth token` scripting helper.
//
// Not parallel: it manipulates ENTIRE_TOKEN / ENTIRE_CONFIG_DIR.
func TestAuthTokenCmd(t *testing.T) {
	// Guard against a real ENTIRE_TOKEN in the dev's environment leaking into
	// the not-logged-in case; restore it afterward.
	if v, ok := os.LookupEnv("ENTIRE_TOKEN"); ok {
		os.Unsetenv("ENTIRE_TOKEN")
		// t.Setenv can't unset, and there's no t.Unsetenv, so restore manually.
		t.Cleanup(func() { os.Setenv("ENTIRE_TOKEN", v) }) //nolint:usetesting // restoring a captured value; no t.Unsetenv equivalent
	}

	t.Run("prints the env token verbatim", func(t *testing.T) {
		token := makeTestJWT(t, `{"sub":"ci","aud":"https://core.us.entire.io"}`)
		t.Setenv("ENTIRE_TOKEN", token)

		cmd := newAuthTokenCmd()
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		require.NoError(t, cmd.ExecuteContext(t.Context()))
		require.Equal(t, token+"\n", out.String())
		require.Empty(t, errOut.String())
	})

	t.Run("not logged in errors silently with a hint", func(t *testing.T) {
		// Isolated empty config so there's no active context to resolve.
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

		cmd := newAuthTokenCmd()
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		err := cmd.ExecuteContext(t.Context())

		var silent *SilentError
		require.ErrorAs(t, err, &silent)
		require.Empty(t, out.String(), "stdout must stay clean for command substitution")
		require.Contains(t, errOut.String(), "Not logged in")
	})
}
