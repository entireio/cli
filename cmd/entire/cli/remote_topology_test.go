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

func electedSync(name string, source strategy.CheckpointSyncRemoteSource) checkpointSyncInfo {
	return checkpointSyncInfo{Remote: name, Source: string(source)}
}

const noteHeader = "Checkpoint destination: REVIEW"

// TestDescribeCheckpointDestination covers the note as users see it: which
// claims it makes, and — the part every past bug here got wrong — which it must
// not make about a remote that carries no checkpoints.
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
			topo:   remoteTopology{destinations: []remoteDestination{oneURL("origin", false)}, sync: electedSync("origin", strategy.SyncRemoteSourceDefault)},
			silent: true,
		},
		{
			// A checkpoint_remote in settings.local.json applies to every
			// remote, so the destination is the store whichever one is pushed.
			name: "every remote pinned says nothing",
			topo: remoteTopology{
				destinations: []remoteDestination{oneURL("origin", true), oneURL("upstream", true)},
				sync:         checkpointSyncInfo{Remote: "me/checkpoints", Source: checkpointSyncSourceDedicated},
			},
			silent: true,
		},
		{
			name: "open election says a push can still move it",
			topo: remoteTopology{
				destinations: []remoteDestination{oneURL("colleague", false), oneURL("origin", false)},
				sync:         electedSync("origin", strategy.SyncRemoteSourceDefault),
			},
			want: []string{"This repo has 2 remotes (colleague, origin).", `right now "origin"`, "To pin one repository"},
			// False on an open tier: the electing push is exactly the one that
			// carries its history to the remote it elects.
			absent: []string{"no session history"},
		},
		{
			name: "pinned by setting says nothing further",
			topo: remoteTopology{
				destinations: []remoteDestination{oneURL("colleague", false), oneURL("publish", false)},
				sync:         electedSync("publish", strategy.SyncRemoteSourceConfig),
			},
			want:   []string{`"publish"`, "checkpoint_push_remote", "no session history"},
			absent: []string{"right now", "Set strategy_options"},
		},
		{
			name: "captured election is settled",
			topo: remoteTopology{
				destinations: []remoteDestination{oneURL("colleague", false), oneURL("fork", false)},
				sync:         electedSync("fork", strategy.SyncRemoteSourceObserved),
			},
			want:   []string{`"fork"`, "push destination", "no session history", "Set strategy_options.checkpoint_push_remote"},
			absent: []string{"right now"},
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
			absent: []string{`right now "origin"`, "To pin one repository", "no session history"},
		},
		{
			// Narrow, but the sentence must not read "no remote carries them"
			// when a push is in fact reaching the store.
			name: "dedicated store with no carrier defers instead of denying",
			topo: remoteTopology{
				destinations: []remoteDestination{oneURL("colleague", false), oneURL("upstream", false)},
				sync:         checkpointSyncInfo{Remote: "me/checkpoints", Source: checkpointSyncSourceDedicated},
			},
			want:   []string{`"me/checkpoints"`, "whether your pushes are reaching it"},
			absent: []string{"no remote", "Pushes to"},
		},
		{
			name: "ignored store is reported, not swallowed",
			topo: remoteTopology{
				destinations: []remoteDestination{oneURL("colleague", false), oneURL("origin", false)},
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
				sync:          electedSync("origin", strategy.SyncRemoteSourceDefault),
			},
			want:   []string{"This repo has 2 remotes (origin, origin2).", `right now "origin"`},
			absent: []string{"pushes to 2 URLs", "first URL only"},
		},
		{
			name: "fan-out on the elected remote is explained",
			topo: remoteTopology{
				destinations:  []remoteDestination{oneURL("colleague", false), twoURLs("origin")},
				primaryIsRefs: true,
				sync:          electedSync("origin", strategy.SyncRemoteSourceDefault),
			},
			want: []string{`Remote "origin" pushes to 2 URLs`, "→ https://github.com/x/origin.git", "first URL only"},
		},
		{
			name: "git-branch backend explains the fan-out differently",
			topo: remoteTopology{
				destinations: []remoteDestination{oneURL("colleague", false), twoURLs("origin")},
				sync:         electedSync("origin", strategy.SyncRemoteSourceDefault),
			},
			want:   []string{"pushed to every URL", "fall permanently out of date"},
			absent: []string{"first URL only"},
		},
		{
			// The state a broken checkpoint_push_remote leaves: nothing is
			// syncing anywhere, so no URL breakdown can be true.
			name: "fail-closed election suppresses every URL claim",
			topo: remoteTopology{
				destinations:  []remoteDestination{oneURL("colleague", false), twoURLs("origin")},
				primaryIsRefs: true,
				sync:          checkpointSyncInfo{Err: `checkpoint_push_remote "gone" is not a configured git remote`},
			},
			want:   []string{"NOT syncing", `"gone"`},
			absent: []string{"pushes to 2 URLs", "first URL only", "To pin one repository"},
		},
		{
			// A lone fanning-out remote makes the note speak, and a fail-closed
			// election is the fact it must lead with — this printed a bare
			// header once, because the warning sat behind a remote-count gate.
			name: "fail-closed election speaks even with one remote",
			topo: remoteTopology{
				destinations:  []remoteDestination{twoURLs("origin")},
				primaryIsRefs: true,
				sync:          checkpointSyncInfo{Err: `checkpoint_push_remote "gone" is not a configured git remote`},
			},
			want:   []string{"NOT syncing", `"gone"`},
			absent: []string{"This repo has", "pushes to 2 URLs"},
		},
		{
			// Same bare-header regression from the other side: a store in
			// effect plus one unpinned remote that fans out.
			name: "dedicated store speaks even with one unpinned remote",
			topo: remoteTopology{
				destinations:  []remoteDestination{oneURL("origin", true), twoURLs("upstream")},
				primaryIsRefs: true,
				sync:          checkpointSyncInfo{Remote: "me/checkpoints", Source: checkpointSyncSourceDedicated},
			},
			want:   []string{`"me/checkpoints"`, `Pushes to "origin"`},
			absent: []string{"This repo has", "pushes to 2 URLs"},
		},
		{
			// The resolver answers with no name and no error when its own git
			// read fails transiently. Staying silent would leave the remote
			// count with nothing explaining what happens to it.
			name: "unreadable election still says how the destination works",
			topo: remoteTopology{
				destinations: []remoteDestination{oneURL("colleague", false), oneURL("origin", false)},
			},
			want:   []string{"single elected remote", "entire status"},
			absent: []string{"NOT syncing"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var b strings.Builder
			tt.topo.describeCheckpointDestination(&b, noteHeader)
			out := b.String()
			if tt.silent {
				if out != "" {
					t.Errorf("note should say nothing, got:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, noteHeader) {
				t.Errorf("note has a body but no header:\n%s", out)
			}
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("note should mention %q:\n%s", want, out)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(out, absent) {
					t.Errorf("note should not mention %q:\n%s", absent, out)
				}
			}
		})
	}
}

// TestNoteNeverPrintsABareHeader is the invariant behind the shape of this note:
// the header claims the repo needs explaining, so a topology that reaches it must
// produce an explanation. It was violated by gating each explaining branch on a
// condition narrower than the header's own, which no per-state assertion catches
// — every one of those states looked right on its own.
func TestNoteNeverPrintsABareHeader(t *testing.T) {
	t.Parallel()

	syncStates := map[string]checkpointSyncInfo{
		"config":     electedSync("origin", strategy.SyncRemoteSourceConfig),
		"observed":   electedSync("origin", strategy.SyncRemoteSourceObserved),
		"default":    electedSync("origin", strategy.SyncRemoteSourceDefault),
		"sole":       electedSync("origin", strategy.SyncRemoteSourceSole),
		"first":      electedSync("origin", strategy.SyncRemoteSourceFirst),
		"unresolved": {},
		"failclosed": {Err: `checkpoint_push_remote "gone" is not a configured git remote`},
		"dedicated":  {Remote: "me/checkpoints", Source: checkpointSyncSourceDedicated},
		"ignored": {
			Remote: "origin", Source: string(strategy.SyncRemoteSourceDefault),
			IgnoredStore: "them/checkpoints",
		},
	}
	layouts := map[string][]remoteDestination{
		"lone fan-out":              {twoURLs("origin")},
		"fan-out beside a pinned":   {oneURL("origin", true), twoURLs("upstream")},
		"two remotes":               {oneURL("origin", false), oneURL("upstream", false)},
		"two remotes, one fans out": {twoURLs("origin"), oneURL("upstream", false)},
		"fan-out on a non-elected":  {oneURL("origin", false), twoURLs("other")},
		"three, one pinned":         {oneURL("colleague", false), oneURL("origin", true), oneURL("upstream", false)},
	}

	for layoutName, dests := range layouts {
		for syncName, sync := range syncStates {
			for _, refs := range []bool{true, false} {
				t.Run(layoutName+"/"+syncName, func(t *testing.T) {
					t.Parallel()
					topo := remoteTopology{destinations: dests, sync: sync, primaryIsRefs: refs}
					if !topo.ambiguous() {
						return // silence is correct here; nothing to assert
					}
					if !topo.consistent() {
						return // a combination inspectRemoteTopology cannot produce
					}
					var b strings.Builder
					topo.describeCheckpointDestination(&b, noteHeader)
					out := b.String()
					if out == "" {
						t.Fatal("ambiguous topology printed nothing at all")
					}
					if strings.TrimSpace(strings.TrimPrefix(out, noteHeader)) == "" {
						t.Errorf("header with no body:\n%s", out)
					}
				})
			}
		}
	}
}

// consistent reports whether this hand-built topology is one
// inspectRemoteTopology could actually produce. The cross-product above includes
// combinations the producer rules out, and asserting on those would be asserting
// on states that cannot happen: the store being in effect for the elected remote
// is precisely what makes the destination the store, so an elected-remote
// destination cannot coexist with a pinned elected remote — both answers come
// from one probe of one snapshot.
func (t remoteTopology) consistent() bool {
	d := t.electedDestination()
	if d == nil {
		return true
	}
	if t.sync.dedicated() {
		return true // Remote is a store slug here, never a remote name
	}
	return !d.pinned
}

// TestInspectRemoteTopology_DedicatedStore drives the note against a real repo
// rather than a hand-built topology. The tables above assert wording; only this
// proves the note and `entire status` read the same state — the property that
// actually broke, because the note used to resolve it separately.
//
// Not parallel: setupTestRepo chdirs.
func TestInspectRemoteTopology_DedicatedStore(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	setupTestRepo(t)
	// Tracked settings.json, not settings.local.json: a local declaration
	// applies to every remote, which leaves nothing ambiguous to report.
	writeSettings(t, `{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`)
	// origin's owner matches the store's, so the store applies to it; the other
	// two are owned by someone else and do not, which is what makes the
	// destination ambiguous enough for the note to speak.
	testutil.AddRemote(t, ".", "origin", "https://github.com/org/app.git")
	testutil.AddRemote(t, ".", "upstream", "https://github.com/other/app.git")
	testutil.AddRemote(t, ".", "colleague", "https://github.com/someone/app.git")

	var b strings.Builder
	inspectRemoteTopology(context.Background()).describeCheckpointDestination(&b, noteHeader)
	out := b.String()

	for _, want := range []string{
		"This repo has 3 remotes (colleague, origin, upstream).",
		`"org/checkpoints"`,
		`Pushes to "origin"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("note should mention %q:\n%s", want, out)
		}
	}
	// The regression: origin was named as the destination while being absent
	// from the list, and checkpoints go to the store, not to origin.
	if strings.Contains(out, `right now "origin"`) {
		t.Errorf("note must not name a remote as the destination in dedicated mode:\n%s", out)
	}
	// The carrier came from the same read as the remote list, so it is named
	// rather than deferred.
	if strings.Contains(out, "whether your pushes are reaching it") {
		t.Errorf("carrier should be named when the store resolves for a remote:\n%s", out)
	}
}
