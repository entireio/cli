package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
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
			topology.describeCheckpointDestination(&b, "Checkpoint destination: REVIEW")
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
	}{
		{name: "explicit fork", selected: forkRemote},
		{name: "default still offers choice", wantChoice: true},
		{name: "missing selection is not resolved", selected: "gone", wantChoice: true},
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
			printCheckpointDestinationNote(t.Context(), &out, "Checkpoint destination: REVIEW")
			got := out.String()
			if tt.otherURLs && strings.Contains(got, `Remote "origin"`) {
				t.Errorf("unselected origin must not be described as carrying checkpoints:\n%s", got)
			}
			if choice := strings.Contains(got, "This repo has 2 remotes"); choice != tt.wantChoice {
				t.Errorf("multi-remote choice warning = %v, want %v; output:\n%s", choice, tt.wantChoice, got)
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
			case !tt.wantChoice:
				if got != "" {
					t.Errorf("valid explicit single-URL selection should need no destination note:\n%s", got)
				}
			}
		})
	}
}

// The topology note once told users to "set checkpoint_remote", which names a
// separate {provider, repo} checkpoint repository and silently ignores a remote
// name. These tests pin the corrected advice and the positive line an elected
// Entire remote earns instead of a warning.

func twoPlainRemotes() remoteTopology {
	return remoteTopology{destinations: []remoteDestination{
		{name: "fork", pushURLs: []string{"https://github.com/me/r"}},
		{name: "origin", pushURLs: []string{"https://github.com/o/r"}},
	}}
}

func TestDescribeCheckpointDestination_MultiRemoteNamesPushRemoteSetting(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	twoPlainRemotes().describeCheckpointDestination(&buf, "Header")
	got := buf.String()

	assert.Contains(t, got, "Header\n")
	assert.Contains(t, got, "This repo has 2 remotes (fork, origin).")
	assert.Contains(t, got, "run `entire checkpoint migrate --to <remote>`")
	assert.Contains(t, got, "strategy_options.checkpoint_push_remote in .entire/settings.local.json")
	assert.Contains(t, got, "see\n  checkpoint_remote in the README.")
	assert.NotContains(t, got, "set checkpoint_remote")
	assert.NotContains(t, got, "To pin one repository")
}

func TestDescribeCheckpointDestination_EntireElectedIsNotAmbiguous(t *testing.T) {
	t.Parallel()

	topo := remoteTopology{destinations: []remoteDestination{
		{name: "entire", pushURLs: []string{"entire://cluster.test/gh/o/r"}},
		{name: "origin", pushURLs: []string{"https://github.com/o/r"}},
	}, entireElected: "entire"}
	assert.False(t, topo.ambiguous())

	var buf bytes.Buffer
	topo.describeCheckpointDestination(&buf, "Header")
	got := buf.String()
	assert.Equal(t, "\n✓ Checkpoints sync to entire (your Entire remote).\n", got)
	assert.NotContains(t, got, "Header")
	assert.NotContains(t, got, "This repo has")
}

func TestDescribeCheckpointDestination_SingleRemoteSaysNothing(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	remoteTopology{destinations: []remoteDestination{{name: "origin", pushURLs: []string{"entire://c/gh/o/r"}}}, entireElected: "origin"}.
		describeCheckpointDestination(&buf, "Header")
	assert.Empty(t, buf.String(), "a sole Entire remote is the ordinary repo; enable already says where checkpoints go")

	buf.Reset()
	remoteTopology{destinations: []remoteDestination{{name: "origin", pushURLs: []string{"https://github.com/o/r"}}}}.
		describeCheckpointDestination(&buf, "Header")
	assert.Empty(t, buf.String())
}

func TestDescribeCheckpointDestination_FanOutStillReported(t *testing.T) {
	t.Parallel()

	topo := remoteTopology{destinations: []remoteDestination{
		{name: "entire", pushURLs: []string{"entire://cluster.test/gh/o/r"}},
		{name: "origin", pushURLs: []string{"https://github.com/o/r", "https://user:pw@mirror.example/o/r"}},
	}, entireElected: "entire", primaryIsRefs: true}
	assert.True(t, topo.ambiguous())

	var buf bytes.Buffer
	topo.describeCheckpointDestination(&buf, "Header")
	got := buf.String()
	assert.Contains(t, got, "Header\n")
	assert.Contains(t, got, `Remote "origin" pushes to 2 URLs:`)
	assert.Contains(t, got, "https://github.com/o/r")
	assert.NotContains(t, got, "→ ", "no first-URL marker: checkpoints do not go to any of these URLs")
	assert.Contains(t, got, "Your code goes to every URL; checkpoints do not fan out with it.")
	assert.NotContains(t, got, "Checkpoints go to the first URL only", "that is the non-Entire story")
	assert.Contains(t, got, "https://mirror.example/o/r", "credentials are redacted")
	assert.NotContains(t, got, "user:pw")
	assert.Contains(t, got, "  ✓ Checkpoints sync to entire (your Entire remote).\n")
	assert.NotContains(t, got, "This repo has 2 remotes", "the multi-remote choice is settled by the Entire remote")
	assert.NotContains(t, got, "checkpoint migrate --to")

	// Without the election the fan-out block is followed by the picker advice.
	topo.entireElected = ""
	buf.Reset()
	topo.describeCheckpointDestination(&buf, "Header")
	assert.Contains(t, buf.String(), "This repo has 2 remotes (entire, origin).")
	assert.Contains(t, buf.String(), "entire checkpoint migrate --to <remote>")
	assert.Contains(t, buf.String(), "→ https://github.com/o/r")
	assert.Contains(t, buf.String(), "Checkpoints go to the first URL only")
}

func TestRemoteTopology_PinnedRemoteDoesNotCount(t *testing.T) {
	t.Parallel()

	topo := twoPlainRemotes()
	topo.destinations[0].pinned = true
	assert.False(t, topo.ambiguous(), "a remote pinned to a dedicated checkpoint_remote is not a choice")
	assert.Equal(t, []string{"origin"}, topo.unpinnedNames())
}
