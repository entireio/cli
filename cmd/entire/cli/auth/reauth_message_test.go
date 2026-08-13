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

// TestReauthError verifies the accessor used at the control-plane choke points
// (runCoreClient/renderCoreError) returns an error that displays the clean,
// wrapper-stripped message while still errors.Is-matching the underlying
// sentinel — so a re-auth failure stays machine-classifiable instead of
// collapsing into a flat string.
func TestReauthError(t *testing.T) {
	t.Parallel()

	c := &contexts.Context{Name: "us", CoreURL: "https://us.auth.entire.io"}
	expired := contextReauthError(c, tokenmanager.ErrReauthRequired)
	notLoggedIn := contextReauthError(c, tokenmanager.ErrNotLoggedIn)

	t.Run("deep-wrapped chain yields a clean, still-classifiable error", func(t *testing.T) {
		t.Parallel()
		chain := fmt.Errorf("resolve mirror placements: %w",
			fmt.Errorf("security \"BearerAuth\": %w", expired))
		got := ReauthError(chain)
		require.Error(t, got)
		// Clean display: only the friendly message, none of the wrapper noise.
		require.Equal(t, expired.Error(), got.Error())
		require.NotContains(t, got.Error(), "resolve mirror placements")
		require.NotContains(t, got.Error(), "BearerAuth")
		// Chain preserved: the sentinel is still reachable.
		require.ErrorIs(t, got, tokenmanager.ErrReauthRequired)
	})

	t.Run("not-logged-in sentinel is preserved", func(t *testing.T) {
		t.Parallel()
		got := ReauthError(notLoggedIn)
		require.ErrorIs(t, got, tokenmanager.ErrNotLoggedIn)
	})

	t.Run("non-reauth error is not matched", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, ReauthError(errors.New("dial tcp: connection refused")))
	})

	t.Run("nil is not matched", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, ReauthError(nil))
	})
}
