package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// describeCheckpointDestination writes to an io.Writer from a plain struct, so
// the disabled-pushing caveat needs no repo, no remotes and no CLI run.
//
// What it must not do is present the URLs it lists as the read source: they
// are PUSH urls, and a fan-out remote's reads use its fetch url. It points at
// `entire status` for that instead, and does not promise status will always
// have an answer.
func TestDescribeCheckpointDestination_PushDisabledCaveat(t *testing.T) {
	t.Parallel()
	topology := remoteTopology{
		destinations: []remoteDestination{
			{name: "backup", pushURLs: []string{"https://github.com/org/backup.git"}},
			{name: "origin", pushURLs: []string{"https://github.com/org/repo.git"}},
		},
	}
	for _, tc := range []struct {
		name           string
		pushDisabled   bool
		want, unwanted []string
	}{
		{
			name:     "pushing enabled",
			want:     []string{"2 remotes", "single elected remote"},
			unwanted: []string{"push_sessions=false"},
		},
		{
			name:         "pushing disabled",
			pushDisabled: true,
			want: []string{
				// The note still renders its body: the ambiguity is what
				// re-enabling pushing would run into.
				"single elected remote",
				"Automatic checkpoint pushing is disabled (push_sessions=false)",
				"they would go if you re-enabled it",
			},
			// "waiting for it" implied a pending push that cannot happen.
			unwanted: []string{"checkpoints are waiting for it"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			topology := topology
			topology.pushDisabled = tc.pushDisabled
			var b strings.Builder
			topology.describeCheckpointDestination(&b, doctorCheckpointNoteHeaders)
			for _, want := range tc.want {
				if !strings.Contains(b.String(), want) {
					t.Errorf("missing %q:\n%s", want, b.String())
				}
			}
			for _, unwanted := range tc.unwanted {
				if strings.Contains(b.String(), unwanted) {
					t.Errorf("must not mention %q:\n%s", unwanted, b.String())
				}
			}
		})
	}
}

// Not parallel: remote inspection resolves Git state from CWD and uses t.Chdir.
func TestCheckpointDestinationNote_ExplicitSelection(t *testing.T) {
	const forkRemote = "fork"

	for _, tt := range []struct {
		name         string
		selected     string
		multipleURLs bool
		otherURLs    bool
		observed     bool
		pinned       bool
		wantChoice   bool
		wantDisabled bool
	}{
		{name: "explicit fork", selected: forkRemote},
		{name: "default still offers choice", wantChoice: true},
		// Was wantChoice: the ambiguity note used to print here. A missing
		// selection fails the election closed, so no push carries checkpoints
		// at all and the note's "any OTHER remote" framing is false — the
		// disabled report replaces it.
		{name: "missing selection is not resolved", selected: "gone", wantDisabled: true},
		{name: "explicit fork retains multiple URL warning", selected: forkRemote, multipleURLs: true},
		{name: "explicit fork ignores origin fanout", selected: forkRemote, otherURLs: true},
		{name: "explicit fork only describes its own fanout", selected: forkRemote, multipleURLs: true, otherURLs: true},
		{name: "observed fork still explains choice", observed: true, wantChoice: true},
		// A dedicated checkpoint_remote settles the destination, so the
		// selected remote's own push URLs carry no checkpoint data and must not
		// be warned about. Guards the !d.pinned term in fansOut(): without it
		// this repo would warn about two URLs that receive nothing.
		{name: "pinned selection stays silent about its own fanout", selected: forkRemote, multipleURLs: true, pinned: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testutil.IsolateGitConfigEnv(t)
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			testutil.WriteFile(t, dir, "f.txt", "init")
			testutil.GitAdd(t, dir, "f.txt")
			testutil.GitCommit(t, dir, "init")

			// A configured checkpoint_remote only takes effect when origin and
			// every push URL of the push remote share the checkpoint repo's
			// owner (checkpointRemoteIsInherited), so the pinned case needs
			// owner-aligned remotes rather than the fork-of-upstream shape.
			originURL := "https://example.com/upstream/app.git"
			originBackupURL := "https://example.com/upstream/backup.git"
			forkURL := "https://example.com/contributor/app.git"
			forkBackupURL := "https://example.com/contributor/backup.git"
			if tt.pinned {
				originURL = "https://github.com/acme/app.git"
				originBackupURL = "https://github.com/acme/app-backup.git"
				forkURL = "https://github.com/acme/fork.git"
				forkBackupURL = "https://github.com/acme/fork-backup.git"
			}
			testutil.AddRemote(t, dir, "origin", originURL)
			testutil.AddRemote(t, dir, forkRemote, forkURL)

			switch {
			case tt.pinned:
				testutil.WriteFile(t, dir, ".entire/settings.json",
					`{"enabled": true, "strategy_options": {"checkpoint_push_remote": "`+tt.selected+`", "checkpoint_remote": {"provider": "github", "repo": "acme/checkpoints"}}}`)
			case tt.selected != "":
				testutil.WriteCheckpointPushRemoteSetting(t, dir, tt.selected)
			default:
				testutil.WriteFile(t, dir, ".entire/settings.json", `{"enabled": true}`)
			}
			if tt.multipleURLs {
				testutil.RunGit(t, dir, "remote", "set-url", "--add", "--push", forkRemote, forkURL)
				testutil.RunGit(t, dir, "remote", "set-url", "--add", "--push", forkRemote, forkBackupURL)
			}
			if tt.otherURLs {
				testutil.RunGit(t, dir, "remote", "set-url", "--add", "--push", "origin", originURL)
				testutil.RunGit(t, dir, "remote", "set-url", "--add", "--push", "origin", originBackupURL)
			}
			if tt.observed {
				testutil.WriteFile(t, dir, ".git/entire-checkpoint-sync-remotes.json", `{"remotes":["fork"]}`)
			}
			t.Chdir(dir)
			if tt.observed {
				elected, err := strategy.ResolveCheckpointSyncRemote(t.Context())
				if err != nil || elected.Name != forkRemote || elected.Source != strategy.SyncRemoteSourceObserved {
					t.Fatalf("expected observed fork election, got %+v, error: %v", elected, err)
				}
			}
			if tt.pinned {
				// Without this the case could pass vacuously: an ineffective
				// checkpoint_remote or a fork that never fanned out would also
				// produce silence, for the wrong reason.
				topology := inspectRemoteTopology(t.Context())
				var fork remoteDestination
				for _, d := range topology.destinations {
					if d.name == forkRemote {
						fork = d
					}
				}
				if !fork.pinned || len(fork.pushURLs) != 2 {
					t.Fatalf("fixture must pin a fanning-out fork, got pinned=%v pushURLs=%v", fork.pinned, fork.pushURLs)
				}
				if topology.explicitRemote != forkRemote {
					t.Fatalf("fixture must select fork explicitly, got %q", topology.explicitRemote)
				}
			}

			var out bytes.Buffer
			printCheckpointDestinationNote(t.Context(), &out, doctorCheckpointNoteHeaders)
			got := out.String()
			if tt.otherURLs && strings.Contains(got, `Remote "origin"`) {
				t.Errorf("unselected origin must not be described as carrying checkpoints:\n%s", got)
			}
			if choice := strings.Contains(got, "This repo has 2 remotes"); choice != tt.wantChoice {
				t.Errorf("multi-remote choice warning = %v, want %v; output:\n%s", choice, tt.wantChoice, got)
			}
			if disabled := strings.Contains(got, "Checkpoint sync: DISABLED"); disabled != tt.wantDisabled {
				t.Errorf("disabled-sync report = %v, want %v; output:\n%s", disabled, tt.wantDisabled, got)
			}
			switch {
			case tt.pinned:
				if got != "" {
					t.Errorf("a pinned remote's own push URLs carry no checkpoints; want silence:\n%s", got)
				}
			case tt.multipleURLs:
				if !strings.Contains(got, `Remote "fork" pushes to 2 URLs`) {
					t.Errorf("missing multiple-push-URL warning:\n%s", got)
				}
			case !tt.wantChoice && !tt.wantDisabled:
				if got != "" {
					t.Errorf("valid explicit single-URL selection should need no destination note:\n%s", got)
				}
			}
		})
	}
}

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

func TestCheckpointNote_MixedDedicatedDestinationsReportPartialSync(t *testing.T) {
	t.Parallel()

	topo := remoteTopology{
		destinations: []remoteDestination{
			{name: "origin", pushURLs: []string{"https://example.com/a.git"}, pinned: true},
			{name: "publish", pushURLs: []string{"https://example.com/b.git"}},
		},
		electionErr: &strategy.CheckpointPushRemoteNotConfiguredError{Remote: "gone"},
	}

	out := describeTopology(t, topo)
	if !strings.Contains(out, "Checkpoint sync: PARTIAL") || !strings.Contains(out, "Affected remotes: publish") {
		t.Errorf("only the unpinned remote should be reported as affected, got:\n%s", out)
	}
	if strings.Contains(out, "Checkpoint sync: DISABLED") || strings.Contains(out, "Affected remotes: origin") {
		t.Errorf("the dedicated destination still syncs and must not be called disabled, got:\n%s", out)
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
