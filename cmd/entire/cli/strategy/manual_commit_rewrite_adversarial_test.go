package strategy

// Adversarial review of PR #2574 (squashed / redone commits keep their
// checkpoint trailers). Each test asserts what a user would WANT; a failing
// test means the PR does something the user did not ask for. Tests marked
// GUARD document workflows the PR already handles correctly.
//
// These exercise PrepareCommitMsg only (trailer inheritance). The PostCommit
// condensation consequences are covered by
// integration_test/rewrite_adversarial_test.go.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

func advPrepare(t *testing.T, text, source string) string {
	t.Helper()
	msgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msgFile, []byte(text), 0o600))
	require.NoError(t, NewManualCommitStrategy().PrepareCommitMsg(context.Background(), msgFile, source))
	got, err := os.ReadFile(msgFile)
	require.NoError(t, err)
	return string(got)
}

// Regression vs main, false miss. Workflow: `git merge --squash feature` of a
// one-file branch, fix a typo in that file before committing, then `git commit`
// (editor; git seeds the message from SQUASH_MSG, which lists the trailer).
// User wants: the squash commit keeps the squashed commit's trailer.
// PR did: the stale-SQUASH_MSG guard found no staged file equal to a squashed
// commit's version, treats SQUASH_MSG as stale, and strips the trailer that git
// itself seeded. On main prepare skipped "squash" and the trailer survived.
func TestAdversarial_SquashWithTweakKeepsSeededTrailer(t *testing.T) {
	dir, branchCheckpoint := squashFixture(t)
	squashMsg, err := os.ReadFile(filepath.Join(dir, ".git", "SQUASH_MSG"))
	require.NoError(t, err)
	testutil.WriteFile(t, dir, "feature.txt", "agent work (typo fixed)\n")
	testutil.RunGit(t, dir, "add", "feature.txt")

	got := advPrepare(t, string(squashMsg), "squash")
	require.Contains(t, checkpointIDs(got), branchCheckpoint,
		"an in-progress squash whose only file was touched up is still that squash: %q", got)
}

// GUARD. Workflow: the squash message itself carries the right trailers and the
// user commits with `-m`, which reports source "message"; a second
// `Entire-Checkpoint` line for the same ID must not be added.
// User wants and PR does: exactly one copy per inherited trailer.
func TestAdversarial_Guard_SquashMessageWithUserCopiedTrailerIsNotDuplicated(t *testing.T) {
	_, branchCheckpoint := squashFixture(t)
	got := advPrepare(t, "Feature (squashed)\n\nEntire-Checkpoint: "+branchCheckpoint+"\n", "message")
	require.Equal(t, []string{branchCheckpoint}, checkpointIDs(got), "%q", got)
	require.Equal(t, 1, strings.Count(got, "Entire-Checkpoint: "+branchCheckpoint), "%q", got)
}

func checkpointIDs(message string) []string {
	var out []string
	for _, cpID := range trailers.ParseAllCheckpoints(message) {
		out = append(out, cpID.String())
	}
	return out
}
