package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v6/x/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignCommit_DisabledReturnsErrSigningDisabled(t *testing.T) { //nolint:paralleltest // t.Chdir requires non-parallel
	setupSigningEnv(t, true) // signing disabled in settings

	objectSignerLoader = func(context.Context) (plugin.Signer, bool) {
		t.Fatal("signer should not be called when signing is disabled")
		return nil, true
	}

	commit := newTestCommit()
	err := SignCommit(context.Background(), commit)

	require.ErrorIs(t, err, ErrSigningDisabled)
	assert.Empty(t, commit.Signature)
}

func TestSignCommit_PropagatesSignerError(t *testing.T) { //nolint:paralleltest // t.Chdir requires non-parallel
	setupSigningEnv(t, false)

	signerErr := errors.New("signing failed")
	objectSignerLoader = func(context.Context) (plugin.Signer, bool) {
		return &stubSigner{err: signerErr}, true
	}

	commit := newTestCommit()
	err := SignCommit(context.Background(), commit)

	require.ErrorIs(t, err, signerErr)
	assert.Empty(t, commit.Signature)
}

func TestSignCommit_AttachesSignatureOnSuccess(t *testing.T) { //nolint:paralleltest // t.Chdir requires non-parallel
	setupSigningEnv(t, false)

	objectSignerLoader = func(context.Context) (plugin.Signer, bool) {
		return &stubSigner{sig: []byte("FAKESIG")}, true
	}

	commit := newTestCommit()
	err := SignCommit(context.Background(), commit)

	require.NoError(t, err)
	assert.Equal(t, "FAKESIG", commit.Signature)
}
