// Package uiform builds huh forms wired to Entire's standard theme and
// accessibility behavior. Centralises the Theme()+WithAccessible() recipe
// so picker UI stays consistent across callers.
package uiform

import (
	"context"
	"errors"
	"fmt"
	"os"

	"charm.land/huh/v2"
)

// IsAccessibleMode reports whether accessibility mode is enabled via the
// ACCESSIBLE environment variable. Set ACCESSIBLE=1 (or any non-empty
// value) to enable simpler prompts that work better with screen readers.
func IsAccessibleMode() bool {
	return os.Getenv("ACCESSIBLE") != ""
}

// Theme returns Entire's standard huh theme.
//
//nolint:ireturn // huh.Theme is an interface in v2
func Theme() huh.Theme {
	return huh.ThemeFunc(huh.ThemeDracula)
}

// New creates a huh form with the standard theme, switching to accessible
// mode when ACCESSIBLE is set. WithAccessible is only available on forms
// (not individual fields), so wrap confirmations and other prompts in a
// form to opt into accessibility.
func New(groups ...*huh.Group) *huh.Form {
	form := huh.NewForm(groups...).WithTheme(Theme())
	if IsAccessibleMode() {
		form = form.WithAccessible(true)
	}
	return form
}

// PromptYN renders a Confirm form with the standard theme/accessibility
// behavior and returns the user's answer. On user cancellation (Ctrl+C or
// context.Canceled) returns (false, nil) so callers treat it as a "no";
// on real form errors the error is returned wrapped.
func PromptYN(ctx context.Context, question string, def bool) (bool, error) {
	answer := def
	form := New(huh.NewGroup(
		huh.NewConfirm().
			Title(question).
			Value(&answer),
	))
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			return false, nil
		}
		return false, fmt.Errorf("confirm form: %w", err)
	}
	return answer, nil
}
