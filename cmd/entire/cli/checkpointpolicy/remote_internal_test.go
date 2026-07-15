package checkpointpolicy

import (
	"context"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestParseRemotePolicyHash(t *testing.T) {
	t.Parallel()
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)

	got, err := parseRemotePolicyHash(sha1)
	require.NoError(t, err)
	require.Equal(t, sha1, got.String())

	got, err = parseRemotePolicyHash(sha256)
	require.NoError(t, err)
	require.Equal(t, sha256, got.String())

	_, err = parseRemotePolicyHash(strings.Repeat("c", 41))
	require.ErrorContains(t, err, "invalid remote checkpoint policy hash")

	_, err = parseRemotePolicyHash(strings.Repeat("g", 40))
	require.ErrorContains(t, err, "invalid remote checkpoint policy hash")
}

func TestIsAncestorOfReturnsContextCancellation(t *testing.T) {
	t.Parallel()

	repo, err := git.PlainInit(t.TempDir(), false)
	require.NoError(t, err)
	ancestor, err := WriteLocal(t.Context(), repo, plumbing.ZeroHash, DefaultPolicy())
	require.NoError(t, err)
	target, err := WriteLocal(t.Context(), repo, ancestor, DefaultPolicy())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	found, err := isAncestorOf(ctx, repo, ancestor, target)
	require.False(t, found)
	require.ErrorIs(t, err, context.Canceled)
}
