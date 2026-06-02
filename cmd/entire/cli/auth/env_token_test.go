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
			name: "string aud",
			aud:  "https://core.us.entire.io",
			want: "https://core.us.entire.io",
		},
		{
			name: "string aud trailing slash trimmed",
			aud:  "https://core.us.entire.io/",
			want: "https://core.us.entire.io",
		},
		{
			name: "array aud picks first URL-shaped",
			aud:  []string{"entire-cli", "https://core.eu.entire.io"},
			want: "https://core.eu.entire.io",
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
