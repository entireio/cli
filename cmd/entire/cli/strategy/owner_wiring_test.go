//go:build linux || darwin

package strategy

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/proclive"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInitializeSession_CapturesOwner verifies that a turn start records the
// owning process identity, and that the live owner reads as not-exited.
func TestInitializeSession_CapturesOwner(t *testing.T) {
	if _, ok := proclive.ResolveOwner(); !ok {
		t.Skip("no stable process owner resolvable in this environment")
	}

	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	err := s.InitializeSession(context.Background(), "test-session-owner", "Claude Code", "", "", "")
	require.NoError(t, err)

	state, err := s.loadSessionState(context.Background(), "test-session-owner")
	require.NoError(t, err)
	require.NotNil(t, state.Owner, "InitializeSession should capture the owning process")
	assert.Positive(t, state.Owner.PID, "captured owner PID should be positive")
	assert.False(t, state.OwnerExited(), "a freshly-captured live owner must not read as exited")
}
