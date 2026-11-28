package review

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// silentMarkerError stands in for the CLI's SilentError so a test can tell the
// error was wrapped as already printed.
type silentMarkerError struct{ err error }

func (s *silentMarkerError) Error() string { return s.err.Error() }
func (s *silentMarkerError) Unwrap() error { return s.err }

func markSilent(err error) error { return &silentMarkerError{err: err} }

// TestSelfPrintedReviewErrors_SanitizeDynamicValues feeds terminal controls
// through review errors that print their own message before returning a
// silent error. Profile and agent names come from settings files a
// repository can commit, so they must not reach the terminal raw.
func TestSelfPrintedReviewErrors_SanitizeDynamicValues(t *testing.T) {
	t.Parallel()
	payloads := []struct{ name, payload string }{
		{"CSI clear screen", "\x1b[2J"},
		{"OSC 8 hyperlink terminated by BEL", "\x1b]8;;http://example.invalid\a"},
		{"OSC 52 clipboard terminated by ST", "\x1b]52;c;QQ==\x1b\\"},
		{"C0 controls", "\a\b\r\x00\x7f"},
		{"C1 controls", "\u009b2J\u0085"},
		{"bidi override", "\u202Egnp.exe\u202C"},
	}
	for _, tc := range payloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cause := errors.New(`review profile "x" has no eligible agents: profile-left` + tc.payload + "profile-right")
			cases := []struct {
				name string
				run  func(cmd *cobra.Command) error
			}{
				{"picker error", func(cmd *cobra.Command) error { return handlePickerError(cmd, markSilent, cause) }},
				{"print helper", func(cmd *cobra.Command) error {
					return printReviewSilentError(cmd.ErrOrStderr(), markSilent, cause)
				}},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					var stderr bytes.Buffer
					cmd := &cobra.Command{}
					cmd.SetErr(&stderr)
					err := c.run(cmd)

					var silent *silentMarkerError
					require.ErrorAs(t, err, &silent, "the message was printed here, so the error must be silent")
					require.ErrorIs(t, err, cause)
					out := stderr.String()
					assert.Contains(t, out, "profile-left")
					assert.Contains(t, out, "profile-right")
					assert.NotContains(t, out, "\x1b")
					assert.NotContains(t, out, "example.invalid")
					for _, r := range strings.TrimSuffix(out, "\n") {
						if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
							t.Fatalf("stderr carries %U:\n%q", r, out)
						}
					}
				})
			}
		})
	}
}
