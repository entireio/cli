package uiform

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// A validator wrapped by Lenient stands aside only while the key being handled
// is the back key, so leaving a page never needs a valid answer while moving
// forward still does.
func TestBackNav_LenientOnlyWhileGoingBack(t *testing.T) {
	t.Parallel()
	nav := NewBackNav()
	invalid := errors.New("invalid")
	validate := Lenient(nav, func(string) error { return invalid })

	require.ErrorIs(t, validate("x"), invalid, "no key yet: validation applies")

	nav.filter(nil, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.True(t, nav.Going())
	require.NoError(t, validate("x"), "Shift+Tab leaves the page")

	nav.filter(nil, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.False(t, nav.Going())
	require.ErrorIs(t, validate("x"), invalid, "Enter validates again")

	// Other messages leave the last key's answer alone.
	nav.filter(nil, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.False(t, nav.Going())
}

func TestLenient_NilNavWrapsNothing(t *testing.T) {
	t.Parallel()
	invalid := errors.New("invalid")
	require.ErrorIs(t, Lenient(nil, func(string) error { return invalid })("x"), invalid)
}
