package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/stretchr/testify/require"
)

// stubExchangeRT intercepts any /oauth/token POST and returns a canned identity
// token, so the --jurisdiction command path can be tested without a real core.
type stubExchangeRT struct{ token string }

func (s stubExchangeRT) RoundTrip(r *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, r.Body) //nolint:errcheck // test transport
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":3600}`, s.token))),
		Request:    r,
	}, nil
}

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

// TestAuthTokenCmd covers the `entire auth token` scripting helper.
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

// TestAuthTokenCmd_Jurisdiction covers `entire auth token --jurisdiction`.
//
// Not parallel: it manipulates ENTIRE_TOKEN / ENTIRE_CONFIG_DIR and the
// package-global cell-exchange seams.
func TestAuthTokenCmd_Jurisdiction(t *testing.T) {
	if v, ok := os.LookupEnv("ENTIRE_TOKEN"); ok {
		os.Unsetenv("ENTIRE_TOKEN")
		t.Cleanup(func() { os.Setenv("ENTIRE_TOKEN", v) }) //nolint:usetesting // restoring a captured value; no t.Unsetenv equivalent
	}

	t.Run("mints and prints a jurisdictional token from ENTIRE_TOKEN", func(t *testing.T) {
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		envToken := makeTestJWT(t, fmt.Sprintf(`{"aud":"https://us.auth.entire.io","home_jurisdiction":"us","exp":%d}`, time.Now().Add(2*time.Hour).Unix()))
		t.Setenv("ENTIRE_TOKEN", envToken)
		t.Cleanup(auth.SetCellExchangeTransportForTest(t, stubExchangeRT{token: "jurisdiction-token"}))

		cmd := newAuthTokenCmd()
		cmd.SetArgs([]string{"--jurisdiction", "us"})
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)

		require.NoError(t, cmd.ExecuteContext(t.Context()))
		require.Equal(t, "jurisdiction-token\n", out.String())
		require.Empty(t, errOut.String(), "stdout-only: no diagnostics on success")
	})

	t.Run("-j shorthand behaves like --jurisdiction", func(t *testing.T) {
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		envToken := makeTestJWT(t, fmt.Sprintf(`{"aud":"https://us.auth.entire.io","home_jurisdiction":"us","exp":%d}`, time.Now().Add(2*time.Hour).Unix()))
		t.Setenv("ENTIRE_TOKEN", envToken)
		t.Cleanup(auth.SetCellExchangeTransportForTest(t, stubExchangeRT{token: "jurisdiction-token"}))

		cmd := newAuthTokenCmd()
		cmd.SetArgs([]string{"-j", "us"})
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)

		require.NoError(t, cmd.ExecuteContext(t.Context()))
		require.Equal(t, "jurisdiction-token\n", out.String())
		require.Empty(t, errOut.String(), "stdout-only: no diagnostics on success")
	})

	t.Run("not logged in errors silently with a hint", func(t *testing.T) {
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
		// No ENTIRE_TOKEN → stored path. Stub discovery to surface ErrNotLoggedIn
		// without a network call, so the command maps it to the clean hint.
		t.Cleanup(auth.SetResolveContextForCellAPIForTest(t, func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
			return nil, fmt.Errorf("no eligible context: %w", auth.ErrNotLoggedIn)
		}))

		cmd := newAuthTokenCmd()
		cmd.SetArgs([]string{"--jurisdiction", "us"})
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
