package recap

import (
	"strings"
	"testing"
)

func TestNewRecapStyles_PlainWhenNoColor(t *testing.T) {
	t.Parallel()
	s := NewStyles(false)
	// In plain mode, every style should render without ANSI escapes.
	out := s.accent.Render("hello")
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("plain mode leaked ANSI: %q", out)
	}
	if out != "hello" {
		t.Errorf("plain mode changed text: got %q, want %q", out, "hello")
	}
}

func TestNewRecapStyles_ColorPreservesText(t *testing.T) {
	t.Parallel()
	// lipgloss auto-detects terminal capability; in Go test runners stdout
	// isn't a TTY so ANSI may or may not be emitted. The contract we care
	// about: the original text is never dropped or mangled.
	s := NewStyles(true)
	out := s.accent.Render("hello")
	if !strings.Contains(out, "hello") {
		t.Errorf("color mode dropped text: %q", out)
	}
}

func TestStyles_Team(t *testing.T) {
	t.Parallel()
	s := NewStyles(true)
	out := s.team.Render("team")
	if !strings.Contains(out, "team") {
		t.Errorf("team style should render 'team'; got %q", out)
	}
	sOff := NewStyles(false)
	if got := sOff.team.Render("team"); got != "team" {
		t.Errorf("team style (color off) should be plain text; got %q", got)
	}
}

func TestStyleForHint_PreservesTextForEveryHint(t *testing.T) {
	t.Parallel()
	s := NewStyles(true)
	cases := []ActionHint{ActionResume, ActionCommit, ActionPush, ActionClean, ActionNone}
	for _, h := range cases {
		out := s.styleForHint(h).Render("x")
		if !strings.Contains(out, "x") {
			t.Errorf("hint %q dropped text: %q", h, out)
		}
	}
}
