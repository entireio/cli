package remote

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithHTTPAuthFailure(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		output string
		want   error
	}{
		{"username", "fatal: could not read Username for 'https://github.com': terminal prompts disabled", ErrHTTPAuthUnavailable},
		{"password", "fatal: could not read Password for 'https://user:secret@github.com': terminal prompts disabled", ErrHTTPAuthUnavailable},
		{"rejected", "fatal: Authentication failed for 'https://user:secret@github.com/repo'", ErrHTTPAuthRejected},
		{"basic", "remote: HTTP Basic: Access denied", ErrHTTPAuthRejected},
		{"ssh", "Permission denied (publickey)", nil},
		{"network", "Could not resolve host: github.com", nil},
		{"not found", "Repository not found", nil},
		{"forbidden", "The requested URL returned error: 403", nil},
		{"divergence", "! [rejected] main -> main (non-fast-forward)", nil},
		{"unrelated prompt", "terminal prompts disabled", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			original := errors.New("exit status 128")
			err := withHTTPAuthFailure(original, tt.output)
			require.ErrorIs(t, err, original)
			assert.NotContains(t, err.Error(), "secret")
			if tt.want == nil {
				assert.Equal(t, original, err)
			} else {
				require.ErrorIs(t, err, tt.want)
			}
		})
	}
}
