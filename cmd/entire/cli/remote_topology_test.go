package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// describeTopology renders a topology under the headings `entire doctor` really
// passes, so these tests assert on the text a user actually sees.
func describeTopology(t *testing.T, topo remoteTopology) string {
	t.Helper()
	var b strings.Builder
	topo.describeCheckpointDestination(&b, doctorCheckpointNoteHeaders)
	return b.String()
}

// TestCheckpointNote_FailedElectionReplacesTheDestinationClaim is the reported
// bug (see remoteTopology.electionErr). Asserts the false claims are gone, not
// merely that the new text is present.
func TestCheckpointNote_FailedElectionReplacesTheDestinationClaim(t *testing.T) {
	t.Parallel()

	topo := remoteTopology{
		destinations: []remoteDestination{
			{name: "origin", pushURLs: []string{"https://example.com/a.git"}},
			{name: "publish", pushURLs: []string{"https://example.com/b.git"}},
		},
		electionErr: &strategy.CheckpointPushRemoteNotConfiguredError{Remote: "gone"},
	}

	out := describeTopology(t, topo)

	for _, want := range []string{
		"Checkpoint sync: DISABLED",
		`checkpoint_push_remote names "gone"`,
		"origin, publish",
		"no session history",
		"settings.local.json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("disabled note should contain %q, got:\n%s", want, out)
		}
	}
	for _, absent := range []string{
		"single elected remote",
		"entire status",
		"Checkpoint destination: REVIEW",
	} {
		if strings.Contains(out, absent) {
			t.Errorf("disabled note must not claim %q, got:\n%s", absent, out)
		}
	}
}

// TestCheckpointNote_FailedElectionReportsASingleRemoteRepo covers the other
// half of the same bug: the note only ever spoke up about ambiguity, so a
// one-remote repo whose sync was switched off by the same typo got no report
// at all.
func TestCheckpointNote_FailedElectionReportsASingleRemoteRepo(t *testing.T) {
	t.Parallel()

	topo := remoteTopology{
		destinations: []remoteDestination{{name: "origin", pushURLs: []string{"https://example.com/a.git"}}},
		electionErr:  &strategy.CheckpointPushRemoteNotConfiguredError{Remote: "gone"},
	}
	if topo.ambiguous() {
		t.Fatal("setup: a single-remote repo must not be ambiguous, or this asserts nothing")
	}

	out := describeTopology(t, topo)
	if !strings.Contains(out, "Checkpoint sync: DISABLED") {
		t.Errorf("an unambiguous repo whose sync is off should still be reported, got:\n%s", out)
	}
	if !strings.Contains(out, "Affected remotes: origin") {
		t.Errorf("the report should name the remote that now carries nothing, got:\n%s", out)
	}
}

// TestCheckpointNote_UnrecognizedElectionErrorGetsNoRemedy pins the matched-cause
// rule documented on CheckpointPushRemoteNotConfiguredError: an unreadable
// settings file is fail-closed too, and "name a real remote" is not its fix.
func TestCheckpointNote_UnrecognizedElectionErrorGetsNoRemedy(t *testing.T) {
	t.Parallel()

	topo := remoteTopology{
		destinations: []remoteDestination{{name: "origin", pushURLs: []string{"https://example.com/a.git"}}},
		electionErr:  errors.New("cannot read settings to resolve the checkpoint sync remote: boom"),
	}

	out := describeTopology(t, topo)
	if !strings.Contains(out, "cannot read settings") {
		t.Errorf("the unmatched error should still be reported, got:\n%s", out)
	}
	if strings.Contains(out, "Fix:") {
		t.Errorf("an unmatched error must get no remedy, got:\n%s", out)
	}
}

// TestCheckpointNote_PinnedRemoteKeepsSyncingThroughAFailedElection guards the
// exemption the pre-push gate applies before consulting the election: a
// dedicated checkpoint_remote URL is addressed directly, not elected, so its
// checkpoints still ship and the user must not be told sync is off.
func TestCheckpointNote_PinnedRemoteKeepsSyncingThroughAFailedElection(t *testing.T) {
	t.Parallel()

	topo := remoteTopology{
		destinations: []remoteDestination{
			{name: "origin", pushURLs: []string{"https://example.com/a.git"}, pinned: true},
		},
		electionErr: &strategy.CheckpointPushRemoteNotConfiguredError{Remote: "gone"},
	}

	if out := describeTopology(t, topo); out != "" {
		t.Errorf("a repo whose every remote is pinned still syncs; got:\n%s", out)
	}
}

// TestCheckpointNote_FailedElectionWithNoRemotes covers syncDisabled's other
// disjunct: a repo with no remotes syncs nowhere regardless, but the broken
// setting travels with the settings file to clones that do have remotes, so it
// is still reported — without an affected-remotes list it cannot populate.
func TestCheckpointNote_FailedElectionWithNoRemotes(t *testing.T) {
	t.Parallel()

	topo := remoteTopology{electionErr: &strategy.CheckpointPushRemoteNotConfiguredError{Remote: "gone"}}

	out := describeTopology(t, topo)
	if !strings.Contains(out, "Checkpoint sync: DISABLED") || !strings.Contains(out, "Fix:") {
		t.Errorf("a remoteless repo should still get the report and its remedy, got:\n%s", out)
	}
	if strings.Contains(out, "Affected remotes") {
		t.Errorf("there are no remotes to affect, got:\n%s", out)
	}
}

// TestCheckpointNote_SuccessfulElectionKeepsTheAmbiguityNote checks the fix did
// not cost the note its original job.
func TestCheckpointNote_SuccessfulElectionKeepsTheAmbiguityNote(t *testing.T) {
	t.Parallel()

	topo := remoteTopology{
		destinations: []remoteDestination{
			{name: "origin", pushURLs: []string{"https://example.com/a.git"}},
			{name: "publish", pushURLs: []string{"https://example.com/b.git"}},
		},
	}

	out := describeTopology(t, topo)
	if !strings.Contains(out, "Checkpoint destination: REVIEW") ||
		!strings.Contains(out, "single elected remote") {
		t.Errorf("an elected repo with several remotes should still get the note, got:\n%s", out)
	}
	if strings.Contains(out, "DISABLED") {
		t.Errorf("nothing is disabled here, got:\n%s", out)
	}
}
