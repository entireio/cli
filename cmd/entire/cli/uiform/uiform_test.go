package uiform

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestTheme_BlurredTitlesInheritTerminalForeground guards against the theme
// pinning base16 foreground colors on blurred (inactive) fields. ThemeBase16
// copies Focused into Blurred wholesale, so clearing only the Focused variants
// leaves inactive fields in multi-field forms with pinned colors that can't
// invert with the terminal background.
func TestThemeUsesSharedInteractiveMenuStyle(t *testing.T) {
	t.Parallel()
	styles := Theme().Theme(true)
	if got := ansi.Strip(styles.Focused.SelectSelector.String()); got != "> " {
		t.Fatalf("select cursor = %q, want >", got)
	}
	if got := ansi.Strip(styles.Focused.MultiSelectSelector.String()); got != "> " {
		t.Fatalf("multi-select cursor = %q, want >", got)
	}
	if got := ansi.Strip(styles.Focused.SelectedPrefix.String()); got != "◼ " {
		t.Fatalf("selected checkbox = %q, want filled checkbox", got)
	}
	if got := ansi.Strip(styles.Focused.UnselectedPrefix.String()); got != "◻ " {
		t.Fatalf("unselected checkbox = %q, want empty checkbox", got)
	}
	if !styles.Focused.Title.GetBold() || !styles.Blurred.Title.GetBold() {
		t.Fatal("questions must remain bold in focused and blurred fields")
	}
}

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
