package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCoreURLFromEnvToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		aud     any // nil omits the aud claim entirely
		want    string
		wantErr bool
	}{
		{
			name: "https string aud",
			aud:  "https://core.us.entire.io",
			want: "https://core.us.entire.io",
		},
		{
			name: "https aud trailing slash trimmed",
			aud:  "https://core.us.entire.io/",
			want: "https://core.us.entire.io",
		},
		{
			name: "array aud skips opaque, picks URL-shaped https",
			aud:  []string{"entire-cli", "https://core.eu.entire.io"},
			want: "https://core.eu.entire.io",
		},
		{
			name:    "http aud rejected (cleartext)",
			aud:     "http://core.us.entire.io",
			wantErr: true,
		},
		{
			name:    "aud with path rejected",
			aud:     "https://core.us.entire.io/oauth/token",
			wantErr: true,
		},
		{
			name:    "aud with query rejected",
			aud:     "https://core.us.entire.io?x=1",
			wantErr: true,
		},
		{
			name:    "aud with fragment rejected",
			aud:     "https://core.us.entire.io#frag",
			wantErr: true,
		},
		{
			name:    "aud with userinfo rejected",
			aud:     "https://user:pass@core.us.entire.io",
			wantErr: true,
		},
		{
			name:    "url-shaped non-https aud fails closed even with later https entry",
			aud:     []string{"http://evil.example.com", "https://core.us.entire.io"},
			wantErr: true,
		},
		{
			name:    "opaque string aud rejected",
			aud:     "some-opaque-audience",
			wantErr: true,
		},
		{
			name:    "array of opaque audiences rejected",
			aud:     []string{"aud-a", "aud-b"},
			wantErr: true,
		},
		{
			name:    "missing aud rejected",
			aud:     nil,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload := map[string]any{"sub": "ci-runner"}
			if tc.aud != nil {
				payload["aud"] = tc.aud
			}
			raw, err := json.Marshal(payload)
			require.NoError(t, err)
			token := makeJWT(t, string(raw))

			got, err := CoreURLFromEnvToken(token)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), EnvTokenVar)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCoreURLFromEnvToken_MalformedToken(t *testing.T) {
	t.Parallel()
	_, err := CoreURLFromEnvToken("not-a-jwt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), EnvTokenVar)
}

func TestCoreURLFromEnvToken_DoesNotTrim(t *testing.T) {
	t.Parallel()
	// Trimming is the caller's job (done once at the env-var read site in
	// resolveCreds). This function takes the token verbatim, so a padded value
	// is a malformed JWT here — guards against re-introducing a redundant trim.
	token := makeJWT(t, `{"sub":"ci-runner","aud":"https://core.us.entire.io"}`)
	_, err := CoreURLFromEnvToken("  " + token + "\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), EnvTokenVar)
}

func TestCoreURLFromEnvToken_RejectsAlgNone(t *testing.T) {
	t.Parallel()
	// alg:none with a URL-shaped aud must still be rejected at the parse layer.
	enc := base64.RawURLEncoding
	token := enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		enc.EncodeToString([]byte(`{"aud":"https://core.us.entire.io"}`)) + "."
	_, err := CoreURLFromEnvToken(token)
	require.Error(t, err)
	assert.Contains(t, err.Error(), EnvTokenVar)
}

// ParseEnvToken owns the shared trim → blank-check → aud-derivation sequence.
// Unlike CoreURLFromEnvToken it DOES trim, and it returns the (trimmed) token
// so callers send the same bytes verbatim as the bearer.
func TestParseEnvToken(t *testing.T) {
	t.Parallel()
	const core = "https://core.us.entire.io"
	tok := makeJWT(t, `{"sub":"ci-runner","aud":"`+core+`"}`)

	t.Run("trims and returns aud core + trimmed token", func(t *testing.T) {
		t.Parallel()
		coreURL, token, err := ParseEnvToken("  " + tok + "\n")
		require.NoError(t, err)
		assert.Equal(t, core, coreURL)
		assert.Equal(t, tok, token, "token must be returned trimmed for verbatim bearer use")
	})

	t.Run("blank is fail-closed", func(t *testing.T) {
		t.Parallel()
		_, _, err := ParseEnvToken("   ")
		require.Error(t, err)
		assert.Contains(t, err.Error(), EnvTokenVar)
	})

	t.Run("no URL aud is rejected", func(t *testing.T) {
		t.Parallel()
		_, _, err := ParseEnvToken(makeJWT(t, `{"sub":"ci-runner"}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), EnvTokenVar)
	})
}
