package checkpoint

import (
	"context"
	"io"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/x/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateCommit_DoesNotSign verifies CreateCommit produces unsigned
// commits even when signing is enabled and a working signer is registered.
// Signing is deferred to push time.
func TestCreateCommit_DoesNotSign(t *testing.T) { //nolint:paralleltest // t.Chdir + setupSigningEnv conventions
	setupSigningEnv(t, false) // signing enabled in settings

	signerCalled := false
	prev := objectSignerLoader
	objectSignerLoader = func(context.Context) (plugin.Signer, bool) {
		return signerFn(func(io.Reader) ([]byte, error) {
			signerCalled = true
			return []byte("sig"), nil
		}), true
	}
	t.Cleanup(func() { objectSignerLoader = prev })

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	// CreateCommit accepts ZeroHash for tree and parent during init.
	// The point of the test is solely to confirm the signer is not invoked.
	_, err = CreateCommit(context.Background(), repo, plumbing.ZeroHash, plumbing.ZeroHash, "msg", "User", "u@e")
	require.NoError(t, err)

	assert.False(t, signerCalled, "CreateCommit must not invoke the signer")
}

type signerFn func(io.Reader) ([]byte, error)

func (f signerFn) Sign(r io.Reader) ([]byte, error) { return f(r) }
