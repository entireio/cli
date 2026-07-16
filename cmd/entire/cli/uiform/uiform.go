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
	"charm.land/lipgloss/v2"

	"github.com/entireio/cli/cmd/entire/cli/palette"
)

// IsAccessibleMode reports whether accessibility mode is enabled via the
// ACCESSIBLE environment variable. Set ACCESSIBLE=1 (or any non-empty
// value) to enable simpler prompts that work better with screen readers.
func IsAccessibleMode() bool {
	return os.Getenv("ACCESSIBLE") != ""
}

// Theme returns Entire's standard huh theme: base16 (ANSI 0–15) colors so
// form prompts respect the user's terminal palette and stay consistent with
// the rest of the CLI's styling. Derived from huh.ThemeBase16 with a few
// overrides — magenta selection (pointer + chosen options); titles and
// unselected options use the terminal's default foreground so they invert
// with the background (dark text on light, light text on dark) like the
// checkbox bracket does.
func Theme() huh.Theme {
	return huh.ThemeFunc(func(isDark bool) *huh.Styles {
		t := huh.ThemeBase16(isDark)

		accent := lipgloss.Color(palette.Accent)
		lightDark := lipgloss.LightDark(isDark)

		// Titles and unselected options drop their explicit color and inherit
		// the terminal's default text color, which already inverts with the
		// background. A pinned base16 slot (e.g. black "0") can't invert because
		// it always maps to that slot in both themes. Group.Title is copied from
		// Focused.Title inside ThemeBase16, and ThemeBase16 copies Focused into
		// Blurred wholesale, so clear the blurred variants too — otherwise
		// inactive fields in multi-field forms keep the pinned base16 color.
		t.Focused.Title = t.Focused.Title.UnsetForeground()
		t.Group.Title = t.Group.Title.UnsetForeground()
		t.Focused.UnselectedOption = t.Focused.UnselectedOption.UnsetForeground()
		t.Blurred.Title = t.Blurred.Title.UnsetForeground()
		t.Blurred.UnselectedOption = t.Blurred.UnselectedOption.UnsetForeground()

		// Magenta selection: the pointer (single + multi select) and the
		// chosen option(s), replacing ThemeBase16's yellow/green.
		t.Focused.SelectSelector = t.Focused.SelectSelector.Foreground(accent)
		t.Focused.MultiSelectSelector = t.Focused.MultiSelectSelector.Foreground(accent)
		t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(accent)
		t.Focused.SelectedPrefix = t.Focused.SelectedPrefix.Foreground(accent)

		// Blurred (inactive) button: no background chip and default text color,
		// so it reads as plain text that inverts with the terminal theme while
		// the focused button keeps its magenta/accent chip. Set both the focused
		// and blurred field variants (ThemeBase16 copies Focused into Blurred).
		t.Focused.BlurredButton = t.Focused.BlurredButton.UnsetForeground().UnsetBackground()
		t.Blurred.BlurredButton = t.Blurred.BlurredButton.UnsetForeground().UnsetBackground()

		// Focused (selected) button: keep the magenta chip, but use the inverse
		// of the default foreground — i.e. the terminal's background color — so
		// the label reads as reverse video against the chip (light on light
		// terminals, black on dark), the opposite of the unset blurred button.
		buttonText := lightDark(lipgloss.Color(palette.BrightWhite), lipgloss.Color(palette.Black))
		t.Focused.FocusedButton = t.Focused.FocusedButton.Foreground(buttonText)
		t.Blurred.FocusedButton = t.Blurred.FocusedButton.Foreground(buttonText)

		return t
	})
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
