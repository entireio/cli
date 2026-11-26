package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// hostileErrorPayloads are the hostileMetadataPayloads a self-printed error
// must not pass through. Line breaks and tabs are left out: the error filter
// keeps them on purpose, as RenderUserFacingError does, because multi-line
// errors carry their structure in them.
func hostileErrorPayloads() []struct{ name, payload string } {
	var out []struct{ name, payload string }
	for _, p := range hostileMetadataPayloads {
		if strings.ContainsAny(p.payload, "\n\t") {
			continue
		}
		out = append(out, struct{ name, payload string }{p.name, p.payload})
	}
	return out
}

// assertSilentAndSanitized checks that err is a SilentError, so main.go
// prints nothing further, and that what the command printed itself carries
// no terminal control while keeping the visible text around the payload.
func assertSilentAndSanitized(t *testing.T, err error, printed, field string) {
	t.Helper()
	var silent *SilentError
	require.ErrorAs(t, err, &silent, "the message was printed here, so the error must be silent")
	assertSanitizedField(t, printed, field)
}

func TestPrintSilentError_SanitizesMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range hostileErrorPayloads() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cause := errors.New(hostileValue("cause", tc.payload))

			var out bytes.Buffer
			err := printSilentError(&out, cause)
			assertSilentAndSanitized(t, err, out.String(), "cause")
			require.ErrorIs(t, err, cause, "the returned error still matches its cause")
			assert.True(t, strings.HasSuffix(out.String(), "\n"), "the message ends its own line")

			var outf bytes.Buffer
			errf := printSilentErrorf(&outf, cause, "lookup %s failed: %v\n", hostileValue("id", tc.payload), cause)
			assertSilentAndSanitized(t, errf, outf.String(), "id")
			assertSanitizedField(t, outf.String(), "cause")
			require.ErrorIs(t, errf, cause)
		})
	}
}

func TestPrintSilentError_KeepsLineStructure(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := printSilentErrorf(&out, errors.New("x"), "first\n\tindented\nsecond\x1b[31m red\x1b[0m\n")
	var silent *SilentError
	require.ErrorAs(t, err, &silent)
	assert.Equal(t, "first\n\tindented\nsecond red\n", out.String())
}

func TestWriteSessionTranscript_SanitizesSelfPrintedErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range hostileErrorPayloads() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cases := []struct {
				name  string
				field string
				state *strategy.SessionState
			}{
				{
					name:  "no transcript path recorded",
					field: "session",
					state: &strategy.SessionState{SessionID: hostileValue("session", tc.payload)},
				},
				{
					// No registered agent, so resolution fails naming the stored path.
					name:  "transcript unavailable",
					field: "transcript",
					state: &strategy.SessionState{
						SessionID:      "sess-1",
						AgentType:      types.AgentType("no-such-agent"),
						TranscriptPath: filepath.Join(t.TempDir(), hostileValue("transcript", tc.payload)+".jsonl"),
					},
				},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					var stdout, stderr bytes.Buffer
					cmd := &cobra.Command{}
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					err := writeSessionTranscript(context.Background(), cmd, c.state)
					assertSilentAndSanitized(t, err, stderr.String(), c.field)
					assert.Empty(t, stdout.String())
				})
			}
		})
	}
}

func TestSelfPrintedErrors_SanitizeDynamicValues(t *testing.T) {
	t.Parallel()
	for _, tc := range hostileErrorPayloads() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cases := []struct {
				name  string
				field string
				run   func(w *bytes.Buffer) error
			}{
				{
					name:  "repo not onboarded names the repo",
					field: "repo",
					run: func(w *bytes.Buffer) error {
						return renderDataAPIAuthError(context.Background(), w, hostileValue("repo", tc.payload), errRepoNotOnboarded)
					},
				},
				{
					name:  "recap load error carries server text",
					field: "server",
					run: func(w *bytes.Buffer) error {
						return handleRecapFetchError(w, &api.HTTPError{StatusCode: http.StatusBadGateway, Message: hostileValue("server", tc.payload)})
					},
				},
				{
					name:  "explain failure block carries agent text",
					field: "claude",
					run: func(w *bytes.Buffer) error {
						return renderExplainFailure(w, "Claude rejected the summary request", []explainRow{
							{Label: explainLabelMessage, Value: hostileValue("claude", tc.payload)},
						}, errors.New("rejected"))
					},
				},
				{
					name:  "ambiguous prefix lists stored session IDs",
					field: "session",
					run: func(w *bytes.Buffer) error {
						renderAmbiguousPrefixFailure(w, "abc", "commits", []ambiguousMatch{
							{ShortID: "abc1234", SessionID: hostileValue("session", tc.payload)},
						})
						return NewSilentError(errAmbiguousCommitPrefix)
					},
				},
				{
					name:  "mirror removal failure carries server text",
					field: "server",
					run: func(w *bytes.Buffer) error {
						return reportMirrorRemoveResults(io.Discard, w, []mirrorResult{
							{owner: "acme", repo: "web", regionLabel: "us", status: "error", err: errors.New(hostileValue("server", tc.payload))},
						})
					},
				},
				{
					name:  "no session envelope names the caller session",
					field: "caller",
					run: func(w *bytes.Buffer) error {
						cmd := &cobra.Command{}
						cmd.SetOut(io.Discard)
						cmd.SetErr(w)
						id := hostileValue("caller", tc.payload)
						return reportNoSessionEnvelope(cmd, true, fmt.Errorf("no session state for caller session %s", id),
							fmt.Sprintf("This command is running inside Claude Code session %s, but Entire has no session state for it.", id))
					},
				},
				{
					name:  "checkpoint push refusal names the argument",
					field: "arg",
					run: func(w *bytes.Buffer) error {
						return refuseCheckpointPushWithoutTerminal(w, hostileValue("arg", tc.payload))
					},
				},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					var out bytes.Buffer
					err := c.run(&out)
					assertSilentAndSanitized(t, err, out.String(), c.field)
				})
			}
		})
	}
}

// TestRenderFailure_KeepsStylingSanitizesValues checks the styled failure
// block filters only the values it interpolates: the color escapes it adds
// itself are deliberate and must survive.
func TestRenderFailure_KeepsStylingSanitizesValues(t *testing.T) {
	t.Parallel()
	styles := statusStylesFor(true, 80)
	out := styles.renderFailure("label "+hostileValue("label", "\x1b]8;;http://example.invalid\a"), []explainRow{
		{Label: "session", Value: hostileValue("value", "\x1b[2J\u202E")},
	})
	assert.NotContains(t, out, "example.invalid")
	assert.NotContains(t, out, "\x1b[2J")
	assert.NotContains(t, out, "\u202E")
	assert.Contains(t, out, "label-leftlabel-right")
	assert.Contains(t, out, "value-leftvalue-right")
	assert.Equal(t, styles.renderFailure("label label-leftlabel-right", []explainRow{
		{Label: "session", Value: "value-leftvalue-right"},
	}), out, "sanitizing the values leaves the styling exactly as it renders clean values")
}
