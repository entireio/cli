package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// oneURL and twoURLs build the two remote shapes these tests care about: an
// ordinary remote, and one whose pushes fan out across several repositories.
func oneURL(name string, pinned bool) remoteDestination {
	return remoteDestination{name: name, pushURLs: []string{"https://github.com/x/" + name + ".git"}, pinned: pinned}
}

func twoURLs(name string) remoteDestination {
	return remoteDestination{name: name, pushURLs: []string{
		"https://github.com/x/" + name + ".git",
		"https://github.com/x/" + name + "-mirror.git",
	}}
}

// assertNote checks the phrases a note must and must not contain, reporting the
// whole output on failure — the wording is the behaviour under test, so the
// surrounding lines are what make a failure diagnosable.
func assertNote(t *testing.T, out string, want, absent []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("note should mention %q:\n%s", w, out)
		}
	}
	for _, a := range absent {
		if strings.Contains(out, a) {
			t.Errorf("note should not mention %q:\n%s", a, out)
		}
	}
}

// TestDescribeSyncRemote covers every tier the note words differently, plus
// the fail-closed case where the election yields no name at all.
func TestDescribeSyncRemote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		topo   remoteTopology
		want   []string
		absent []string
	}{
		{
			name: "pinned by setting says nothing further",
			topo: remoteTopology{sync: checkpointSyncInfo{Remote: "publish", Source: string(strategy.SyncRemoteSourceConfig)}},
			// A settled election is the case where "no session history" is
			// unconditionally true: no push can displace the setting, so every
			// push elsewhere really does leave the transcripts behind.
			want: []string{`"publish"`, "checkpoint_push_remote", "no session history"},
			// The decision is already made; telling the user to set the key
			// they just set reads as a warning that something is unfinished.
			absent: []string{"Set strategy_options", "right now"},
		},
		{
			name:   "captured election is settled",
			topo:   remoteTopology{sync: checkpointSyncInfo{Remote: "fork", Source: string(strategy.SyncRemoteSourceObserved)}},
			want:   []string{`"fork"`, "push destination", "no session history", "Set strategy_options.checkpoint_push_remote"},
			absent: []string{"right now"},
		},
		{
			name: "open election says a push can move it",
			topo: remoteTopology{sync: checkpointSyncInfo{Remote: "origin", Source: string(strategy.SyncRemoteSourceDefault)}},
			want: []string{`right now "origin"`, "entire status", "Set strategy_options.checkpoint_push_remote"},
			// False on an open tier: the electing push is exactly the one that
			// carries its session history to the remote it elects.
			absent: []string{"no session history"},
		},
		{
			name:   "first-remote election words like any other open one",
			topo:   remoteTopology{sync: checkpointSyncInfo{Remote: "base", Source: string(strategy.SyncRemoteSourceFirst)}},
			want:   []string{`right now "base"`, "entire status"},
			absent: []string{"no session history"},
		},
		{
			name: "fail-closed election reports sync is off",
			topo: remoteTopology{sync: checkpointSyncInfo{Err: `checkpoint_push_remote "gone" is not a configured git remote`}},
			want: []string{"NOT syncing", `"gone"`},
			// No destination may be named when the push side has none.
			absent: []string{"sync to one remote", "single elected remote"},
		},
		{
			// The destination is the store, and the pushes that reach it are
			// the pinned ones — naming the elected remote instead was the bug:
			// it is absent from the remote list the caller just printed.
			name: "dedicated store names the store and its carrier",
			topo: remoteTopology{
				destinations: []remoteDestination{
					{name: "colleague", pushURLs: []string{"https://github.com/them/app.git"}},
					{name: "origin", pushURLs: []string{"https://github.com/me/app.git"}, pinned: true},
				},
				sync: checkpointSyncInfo{Remote: "me/checkpoints", Source: checkpointSyncSourceDedicated},
			},
			want: []string{`"me/checkpoints"`, "checkpoint_remote", `Pushes to "origin"`},
			// Checkpoints do not ride a code push to any remote here, so the
			// settled-tier sentence would be about nothing.
			absent: []string{"no session history", "right now", "sync to one remote"},
		},
		{
			// The resolver answers with no name and no error when its own git
			// read fails transiently. Staying silent would leave the caller's
			// remote count with nothing explaining what happens to it.
			name:   "unreadable election still says how the destination works",
			topo:   remoteTopology{},
			want:   []string{"single elected remote", "entire status"},
			absent: []string{"NOT syncing"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var b strings.Builder
			tt.topo.describeSyncRemote(&b)
			assertNote(t, b.String(), tt.want, tt.absent)
		})
	}
}

// TestDescribeCheckpointDestination covers the note as a whole: which claims it
// makes, and — the part every past bug here got wrong — which it must not make
// about a remote that carries no checkpoints.
func TestDescribeCheckpointDestination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		topo   remoteTopology
		silent bool
		want   []string
		absent []string
	}{
		{
			// One remote, one URL: nothing about the destination is a choice.
			name:   "single remote says nothing",
			topo:   remoteTopology{destinations: []remoteDestination{oneURL("origin", false)}},
			silent: true,
		},
		{
			// A checkpoint_remote in settings.local.json pins every remote, so
			// the destination is the store no matter which remote is pushed.
			name: "every remote pinned says nothing",
			topo: remoteTopology{
				destinations: []remoteDestination{oneURL("origin", true), oneURL("upstream", true)},
				sync:         checkpointSyncInfo{Remote: "me/checkpoints", Source: checkpointSyncSourceDedicated},
			},
			silent: true,
		},
		{
			name: "dedicated store is named instead of the elected remote",
			topo: remoteTopology{
				destinations: []remoteDestination{oneURL("colleague", false), oneURL("origin", true), oneURL("upstream", false)},
				sync:         checkpointSyncInfo{Remote: "me/checkpoints", Source: checkpointSyncSourceDedicated},
			},
			// The count covers every remote: omitting the pinned one is how the
			// note came to name a destination missing from its own list.
			want: []string{"This repo has 3 remotes (colleague, origin, upstream).", `"me/checkpoints"`, `Pushes to "origin"`},
			// Nothing may suggest a remote is the destination, and the closing
			// advice is to set the very setting that produced this state.
			absent: []string{`right now "origin"`, "To pin one repository"},
		},
		{
			name: "ignored store is reported, not swallowed",
			topo: remoteTopology{
				destinations: []remoteDestination{oneURL("colleague", false), oneURL("origin", false), oneURL("upstream", false)},
				sync: checkpointSyncInfo{
					Remote:       "origin",
					Source:       string(strategy.SyncRemoteSourceDefault),
					IgnoredStore: "them/checkpoints",
				},
			},
			// Phrases chosen to survive the paragraph's line wrapping.
			want: []string{`"them/checkpoints"`, "configured but not in", "settings.local.json", `right now "origin"`},
			// A checkpoint_remote already exists; advising one reads as a task.
			absent: []string{"To pin one repository"},
		},
		{
			// Pushes to a remote that is not the elected one are gated out
			// entirely, so its URLs are not a checkpoint destination at all.
			name: "fan-out on a non-elected remote makes no URL claim",
			topo: remoteTopology{
				destinations:  []remoteDestination{oneURL("origin", false), twoURLs("origin2")},
				primaryIsRefs: true,
				sync:          checkpointSyncInfo{Remote: "origin", Source: string(strategy.SyncRemoteSourceDefault)},
			},
			want:   []string{"This repo has 2 remotes (origin, origin2).", `right now "origin"`},
			absent: []string{"pushes to 2 URLs", "first URL only"},
		},
		{
			name: "fan-out on the elected remote is explained",
			topo: remoteTopology{
				destinations:  []remoteDestination{oneURL("colleague", false), twoURLs("origin")},
				primaryIsRefs: true,
				sync:          checkpointSyncInfo{Remote: "origin", Source: string(strategy.SyncRemoteSourceDefault)},
			},
			want: []string{`Remote "origin" pushes to 2 URLs`, "→ https://github.com/x/origin.git", "first URL only"},
		},
		{
			// Fail-closed: nothing is syncing anywhere, so a URL breakdown
			// would be describing delivery that does not happen.
			name: "fail-closed election suppresses every URL claim",
			topo: remoteTopology{
				destinations:  []remoteDestination{oneURL("colleague", false), twoURLs("origin")},
				primaryIsRefs: true,
				sync:          checkpointSyncInfo{Err: `checkpoint_push_remote "gone" is not a configured git remote`},
			},
			want:   []string{"NOT syncing", `"gone"`},
			absent: []string{"pushes to 2 URLs", "first URL only", "To pin one repository"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var b strings.Builder
			tt.topo.describeCheckpointDestination(&b, "Checkpoint destination: REVIEW")
			if tt.silent {
				if b.Len() != 0 {
					t.Errorf("note should say nothing, got:\n%s", b.String())
				}
				return
			}
			assertNote(t, b.String(), tt.want, tt.absent)
		})
	}
}

// TestInspectRemoteTopology_DedicatedStore drives the note against a real repo
// rather than a hand-built topology. The literal tables above assert wording;
// only this one proves the note and `entire status` read the same election —
// which is the property that actually broke, because inspectRemoteTopology had
// its own copy of it.
//
// Not parallel: setupTestRepo chdirs.
func TestInspectRemoteTopology_DedicatedStore(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	// Tracked settings.json, not settings.local.json: a local declaration is
	// "always ours" and pins every remote, which silences the note entirely.
	writeSettings(t, `{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`)
	// origin's owner matches the store's, so the store is in effect for it;
	// the other two are owned by someone else and stay unpinned, which is what
	// makes the destination ambiguous enough for the note to speak.
	testutil.AddRemote(t, ".", "origin", "https://github.com/org/app.git")
	testutil.AddRemote(t, ".", "upstream", "https://github.com/other/app.git")
	testutil.AddRemote(t, ".", "colleague", "https://github.com/someone/app.git")

	var b strings.Builder
	inspectRemoteTopology(context.Background()).describeCheckpointDestination(&b, "Checkpoint destination: REVIEW")
	out := b.String()

	assertNote(t, out,
		[]string{"This repo has 3 remotes (colleague, origin, upstream).", `"org/checkpoints"`, `Pushes to "origin"`},
		// The regression: origin was named as the destination while being
		// absent from the list, and checkpoints go to the store, not to origin.
		[]string{`right now "origin"`})
}
