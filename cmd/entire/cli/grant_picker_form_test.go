package cli

import (
	"bytes"
	"io"
	"os"
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
// command, because the picker pins its forms' output to cmd.ErrOrStderr().
func runAccessibleForm(t *testing.T, input string, fn func(cmd *cobra.Command)) string {
	t.Helper()
	t.Setenv("ACCESSIBLE", "1")

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
	pt := grantPickerTarget{noun: "project", ref: "widgets", roles: accessRoles}
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
	pt := grantPickerTarget{noun: "project", ref: "widgets", roles: accessRoles}
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

	pt := grantPickerTarget{noun: "project", ref: "widgets", roles: accessRoles}
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
