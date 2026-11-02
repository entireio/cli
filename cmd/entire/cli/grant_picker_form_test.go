package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// These exercise the real huh forms, which the seam-based tests in
// grant_picker_test.go deliberately bypass. Accessible mode is what makes that
// possible without a terminal: huh then reads os.Stdin and writes os.Stdout as
// plain prompts (cmp.Or(f.input, os.Stdin) in its form runner) instead of
// opening a TTY and negotiating terminal capabilities, so the fields can be
// driven from a pipe. Nothing in the production path changes — the swap is of
// the process's own streams, which is why these tests are not parallel.
//
// Only ONE answer can be scripted per run: huh builds a fresh bufio.Scanner for
// every prompt (PromptString in its accessibility package), so the first prompt
// drains the whole pipe and later ones read EOF. A prompt at EOF falls back to
// its default, which the role test below turns into the assertion rather than
// working around.

// runAccessibleForm runs fn with ACCESSIBLE set and the process's stdin fed
// from input, returning what the form printed to the command's stderr.
//
// Only stdin is a process-global swap; the prompts are captured from the cobra
// command, because the picker renders to cmd.ErrOrStderr() whenever that is
// where the user can see them.
func runAccessibleForm(t *testing.T, input string, fn func(cmd *cobra.Command)) string {
	t.Helper()
	t.Setenv("ACCESSIBLE", "1")
	stubPromptTerminal(t)

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	oldIn := os.Stdin
	os.Stdin = inR
	t.Cleanup(func() { os.Stdin = oldIn })

	// The answers are buffered ahead of the run so the form never blocks.
	_, err = inW.WriteString(input)
	require.NoError(t, err)
	require.NoError(t, inW.Close())

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	var prompts bytes.Buffer
	cmd.SetErr(&prompts)
	fn(cmd)
	return prompts.String()
}

// TestPickRoles_EachRowKeepsItsOwnRole is the wiring this feature turns on: one
// row per grantee, each bound to its own value. A single shared binding would
// still compile and still produce a plausible-looking form, so the two rows
// have to end up different to prove they are separate.
//
// Only the first row is answered — admin, option 3 in the target's help order.
// The second reads EOF and takes its default, which is the least-privileged
// role the picker pre-selects. Rows that moved together could not land on two
// different roles, and the default being reader is itself worth pinning.
//
// Not parallel: swaps the process's stdin and stdout.
func TestPickRoles_EachRowKeepsItsOwnRole(t *testing.T) {
	pt := grantPickerTarget{noun: "project", ref: "widgets", roles: accessRoles, least: leastAccessRole}
	var got []grantSelection
	out := runAccessibleForm(t, "3\n", func(cmd *cobra.Command) {
		var err error
		got, err = pickRoles(cmd, pt, []grantCandidate{handleCandidate("github:alice"), handleCandidate("github:bob")}, "")
		require.NoError(t, err)
	})

	require.Equal(t, []grantSelection{
		{handle: "github:alice", role: "admin"},
		{handle: "github:bob", role: "reader"},
	}, got)
	// Each row is titled with the grantee it sets, so they can be told apart.
	require.Contains(t, out, "github:alice")
	require.Contains(t, out, "github:bob")
}

// TestPickRoles_FixedRoleIsShownAndNotAsked: with --role given the rows are a
// note rather than selects, so every grantee is shown carrying the role it will
// receive and nothing is asked. huh skips a note by default and un-skips one
// that is alone in its group, which is what keeps the display on screen.
//
// Not parallel: swaps the process's stdin and stdout.
func TestPickRoles_FixedRoleIsShownAndNotAsked(t *testing.T) {
	pt := grantPickerTarget{noun: "project", ref: "widgets", roles: accessRoles, least: leastAccessRole}
	var got []grantSelection
	// No answers at all: a form that asked anything would block or fail here.
	out := runAccessibleForm(t, "", func(cmd *cobra.Command) {
		var err error
		got, err = pickRoles(cmd, pt, []grantCandidate{handleCandidate("github:alice"), handleCandidate("github:bob")}, "writer")
		require.NoError(t, err)
	})

	require.Equal(t, []grantSelection{
		{handle: "github:alice", role: "writer"},
		{handle: "github:bob", role: "writer"},
	}, got)
	require.Contains(t, out, "github:alice")
	require.Contains(t, out, "github:bob")
	require.Contains(t, out, "writer")
	// The roles were stated, not offered.
	require.NotContains(t, out, "Enter a number")
}

// TestPickRoles_PromptsStayOffStdout: these commands can be asked for --json,
// and huh writes to stdout in accessible mode, so prompts would land inside the
// JSON a caller is parsing. The picker pins form output to stderr for exactly
// that reason; this fails if a form is ever built without it.
//
// Not parallel: swaps the process's stdin.
func TestPickRoles_PromptsStayOffStdout(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	stubPromptTerminal(t)

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	oldIn := os.Stdin
	os.Stdin = inR
	t.Cleanup(func() { os.Stdin = oldIn })
	require.NoError(t, inW.Close())

	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	oldOut := os.Stdout
	os.Stdout = outW
	t.Cleanup(func() { os.Stdout = oldOut })

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	var prompts, stdout bytes.Buffer
	cmd.SetErr(&prompts)
	cmd.SetOut(&stdout)

	pt := grantPickerTarget{noun: "project", ref: "widgets", roles: accessRoles, least: leastAccessRole}
	_, err = pickRoles(cmd, pt, []grantCandidate{handleCandidate("github:alice")}, "writer")
	require.NoError(t, err)

	require.NoError(t, outW.Close())
	os.Stdout = oldOut
	leaked, err := io.ReadAll(outR)
	require.NoError(t, err)

	require.Contains(t, prompts.String(), "github:alice", "the prompt goes to stderr")
	require.Empty(t, string(leaked), "nothing may reach the process's stdout")
	require.Empty(t, stdout.String(), "nor the command's stdout, which carries --json")
}

// TestRemovePicker_CancellingReadsAsARevoke drives the real remove screen
// rather than the seam. The grantee multi-select is shared with `grant add`, so
// whoever opens it has to say which action it belongs to; this fails if
// removePicker hands it the grant wording.
//
// The failure it provokes is the terminal not opening, because that is the one
// path into cancelledPicker reachable with no terminal at all — huh's
// accessible mode neither aborts on a cancelled context nor errors at EOF.
//
// Not parallel: swaps the terminal opener.
func TestRemovePicker_CancellingReadsAsARevoke(t *testing.T) {
	prev := openPromptTerminal
	openPromptTerminal = func() (promptTerminal, error) {
		return promptTerminal{}, errors.New("no controlling terminal")
	}
	t.Cleanup(func() { openPromptTerminal = prev })

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetErr(&bytes.Buffer{}) // not a terminal, so the fallback is attempted

	pt := grantPickerTarget{noun: "project", ref: "widgets", roles: accessRoles, least: leastAccessRole}
	_, err := removePicker(cmd, pt, []grantCandidate{{ref: "01HZX7Q", label: "github:alice", role: "admin", byID: true}})
	require.ErrorContains(t, err, "Revocation prompt failed")
	require.NotContains(t, err.Error(), "Grant", "a revoke never reports itself as a grant")
}

// stubPromptTerminal keeps a form on the command's own streams. A test's stderr
// is a buffer, never a terminal, so runPromptForm would otherwise fall back to
// the controlling terminal and read the developer's keyboard. The zero value
// leaves input and output alone, which is the branch these tests are about.
func stubPromptTerminal(t *testing.T) {
	t.Helper()
	prev := openPromptTerminal
	openPromptTerminal = func() (promptTerminal, error) { return promptTerminal{}, nil }
	t.Cleanup(func() { openPromptTerminal = prev })
}

// TestPromptForm_FallsBackToTheControllingTerminal is the routing the picker's
// screens depend on. Stderr is redirected often enough (`grant add … 2>log`)
// that pinning a prompt to it fails silently rather than loudly: Bubble Tea
// sets ttyOutput only for a terminal writer, then cannot query the window size
// and renders into a 0x0 viewport while stdin is in raw mode — an invisible
// prompt on an apparently hung command.
//
// So a non-terminal stderr falls back to the controlling terminal for BOTH
// halves. A test's stderr is a buffer, so this is the branch that runs here;
// the stub stands in for /dev/tty.
//
// Not parallel: sets ACCESSIBLE and swaps the terminal opener.
func TestPromptForm_FallsBackToTheControllingTerminal(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")

	var terminal bytes.Buffer
	prev := openPromptTerminal
	openPromptTerminal = func() (promptTerminal, error) {
		return promptTerminal{in: strings.NewReader("2\n"), out: &terminal}, nil
	}
	t.Cleanup(func() { openPromptTerminal = prev })

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	var stderr, stdout bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(&stdout)

	pt := grantPickerTarget{noun: "project", ref: "widgets", roles: accessRoles, least: leastAccessRole}
	got, err := pickRoles(cmd, pt, []grantCandidate{handleCandidate("github:alice")}, "")
	require.NoError(t, err)
	// Answered on the terminal's own input: option 2 of reader/writer/admin.
	require.Equal(t, []grantSelection{{handle: "github:alice", role: "writer"}}, got)

	require.Contains(t, terminal.String(), "github:alice", "the prompt goes where it can be seen")
	require.Empty(t, stderr.String(), "not to a stderr that is not a terminal")
	require.Empty(t, stdout.String(), "and never to stdout, which carries --json")
}

// TestPickGrantees_ShowsThePoolCaveatWithTheRows drives the real multi-select
// and checks the truncation caveat is rendered as part of it. Carrying the note
// on grantPickerTarget only helps if the screen actually prints it — and the
// whole reason it travels that way is that a line sent to stderr can be
// invisible exactly when the form is not.
//
// Not parallel: swaps the process's stdin and the terminal opener.
func TestPickGrantees_ShowsThePoolCaveatWithTheRows(t *testing.T) {
	const caveat = "Only the first 1200 members of the org owning project widgets were read"
	pt := grantPickerTarget{
		noun: "project", ref: "widgets", roles: accessRoles, least: leastAccessRole,
		poolNote: caveat,
	}
	out := runAccessibleForm(t, "\n", func(cmd *cobra.Command) {
		_, err := pickGrantees(cmd, pt, grantAction, "Select grantees for project widgets",
			[]grantCandidate{handleCandidate("github:alice"), handleCandidate("github:bob")})
		require.NoError(t, err)
	})

	require.Contains(t, out, caveat, "the caveat is shown with the rows it qualifies")
	require.Contains(t, out, "github:alice", "and the rows are still there")
}
