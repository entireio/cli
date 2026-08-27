package uiform

import (
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// TestTheme_BlurredTitlesInheritTerminalForeground guards against the theme
// pinning base16 foreground colors on blurred (inactive) fields. ThemeBase16
// copies Focused into Blurred wholesale, so clearing only the Focused variants
// leaves inactive fields in multi-field forms with pinned colors that can't
// invert with the terminal background.
func TestTheme_BlurredTitlesInheritTerminalForeground(t *testing.T) {
	t.Parallel()

	for _, isDark := range []bool{true, false} {
		s := Theme().Theme(isDark)

		unset := map[string]lipgloss.Style{
			"Blurred.Title":            s.Blurred.Title,
			"Blurred.UnselectedOption": s.Blurred.UnselectedOption,
			"Focused.Title":            s.Focused.Title,
			"Focused.UnselectedOption": s.Focused.UnselectedOption,
			"Group.Title":              s.Group.Title,
		}
		for name, style := range unset {
			if _, ok := style.GetForeground().(lipgloss.NoColor); !ok {
				t.Errorf("isDark=%v: %s pins foreground %v, want unset so it inherits the terminal default",
					isDark, name, style.GetForeground())
			}
		}
	}
}

func TestSingleLineMultiSelectHeight(t *testing.T) {
	t.Parallel()

	options := []huh.Option[string]{
		huh.NewOption("Claude Code", "claude-code"),
		huh.NewOption("Codex", "codex"),
		huh.NewOption("Copilot CLI", "copilot-cli"),
		huh.NewOption("Cursor", "cursor"),
		huh.NewOption("Factory AI Droid", "factoryai-droid"),
		huh.NewOption("Gemini CLI", "gemini"),
		huh.NewOption("OpenCode", "opencode"),
		huh.NewOption("Pi", "pi"),
	}

	newForm := func() *huh.Form {
		field := huh.NewMultiSelect[string]().
			Title("Select the agents you want to use").
			Description("Use space to select, enter to confirm.").
			Options(options...).
			Height(SingleLineMultiSelectHeight(len(options)))
		form := New(huh.NewGroup(field))
		form.Init()
		return form
	}

	t.Run("shows every option when space is available", func(t *testing.T) {
		t.Parallel()

		form := newForm()
		form.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
		view := form.View()
		for _, option := range options {
			if !strings.Contains(view, option.Key) {
				t.Errorf("option %q is not visible in:\n%s", option.Key, view)
			}
		}
	})

	t.Run("scrolls when the terminal clamps the field", func(t *testing.T) {
		t.Parallel()

		form := newForm()
		form.Update(tea.WindowSizeMsg{Width: 100, Height: 8})
		if view := form.View(); strings.Contains(view, "Pi") {
			t.Fatalf("last option is visible before scrolling in:\n%s", view)
		}

		for range len(options) - 1 {
			form.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		}
		if view := form.View(); !strings.Contains(view, "Pi") {
			t.Errorf("last option is not visible after scrolling in:\n%s", view)
		}
	})
}
