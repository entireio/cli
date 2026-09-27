package tuiutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeTerminalText_StripsEscapeSequencesKeepsStructure(t *testing.T) {
	t.Parallel()

	// CSI and OSC sequences are removed whole, not just their ESC byte, and
	// tabs and newlines survive because this filter serves multi-line output.
	in := "a\x1b[31mred\x1b[0m\x1b]0;title\x07 b\tc\nd"
	assert.Equal(t, "ared b\tc\nd", SanitizeTerminalText(in))
}

func TestSanitizeTerminalText_DropsControlAndFormatRunes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "ab", SanitizeTerminalText("a\x00\rb"))
	// Bidi controls are format runes used for terminal spoofing and must not
	// survive, even though the emoji joiner (also a format rune) does.
	assert.Equal(t, "ab", SanitizeTerminalText("a\u202eb"))
}

func TestSanitizeTerminalText_PreservesEmojiSequences(t *testing.T) {
	t.Parallel()

	// This filter serves prose, where nothing is width-aligned: joiners,
	// skin-tone modifiers, and variation selectors must survive so a family
	// stays one emoji and a warning sign keeps its presentation.
	in := "Refactored by \U0001F468\u200d\U0001F469\u200d\U0001F467 the \U0001F44D\U0001F3FD team \u26a0\ufe0f done"
	assert.Equal(t, in, SanitizeTerminalText(in))
}

func TestSanitizeTerminalLabel_DropsEmojiModifiers(t *testing.T) {
	t.Parallel()

	// Skin-tone modifiers, zero-width joiners, and variation selectors confuse
	// terminal width calculations, so the label variant keeps only base runes.
	assert.Equal(t, "\U0001F44D", SanitizeTerminalLabel("\U0001F44D\U0001F3FD"))
	assert.Equal(t, "ab", SanitizeTerminalLabel("a\u200db"))
	assert.Equal(t, "❤", SanitizeTerminalLabel("❤️"))
}

func TestSanitizeTerminalLabel_StripsEscapeSequences(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "ared b", SanitizeTerminalLabel("a\x1b[31mred\x1b[0m b"))
}
