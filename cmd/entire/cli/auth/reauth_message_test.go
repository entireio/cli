package auth

import (
	"errors"
	"fmt"
	"testing"

	"github.com/entireio/auth-go/tokenmanager"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/contexts"
)

// TestReauthMessage verifies the cross-package accessor extracts the clean,
// context-named re-login message from a *reauthError anywhere in the chain
// (both sentinels) and reports ok=false for anything else — the behaviour
// renderCoreError and the clone precondition rely on to strip the noisy ogen
// wrappers off the reported `entire repo clone` failure.
func TestReauthMessage(t *testing.T) {
	t.Parallel()

	c := &contexts.Context{Name: "us", CoreURL: "https://us.auth.entire.io"}
	expired := contextReauthError(c, tokenmanager.ErrReauthRequired)
	notLoggedIn := contextReauthError(c, tokenmanager.ErrNotLoggedIn)
	require.Error(t, expired)
	require.Error(t, notLoggedIn)

	t.Run("expired sentinel", func(t *testing.T) {
		t.Parallel()
		msg, ok := ReauthMessage(expired)
		require.True(t, ok)
		require.Equal(t, expired.Error(), msg)
		require.Contains(t, msg, "expired")
		require.Contains(t, msg, "entire login")
	})

	t.Run("not-logged-in sentinel", func(t *testing.T) {
		t.Parallel()
		msg, ok := ReauthMessage(notLoggedIn)
		require.True(t, ok)
		require.Equal(t, notLoggedIn.Error(), msg)
		require.Contains(t, msg, "no usable login")
	})

	t.Run("deep-wrapped chain keeps the clean message", func(t *testing.T) {
		t.Parallel()
		// Reproduce the reported chain: command prefix + ogen security wrappers.
		chain := fmt.Errorf("resolve mirror placements: %w",
			fmt.Errorf("security \"BearerAuth\": %w",
				fmt.Errorf("security source \"BearerAuth\": %w", expired)))
		msg, ok := ReauthMessage(chain)
		require.True(t, ok)
		require.Equal(t, expired.Error(), msg)
		require.NotContains(t, msg, "resolve mirror placements")
		require.NotContains(t, msg, "BearerAuth")
	})

	t.Run("non-reauth error is not matched", func(t *testing.T) {
		t.Parallel()
		_, ok := ReauthMessage(errors.New("dial tcp: connection refused"))
		require.False(t, ok)
	})

	t.Run("nil is not matched", func(t *testing.T) {
		t.Parallel()
		_, ok := ReauthMessage(nil)
		require.False(t, ok)
	})
}
