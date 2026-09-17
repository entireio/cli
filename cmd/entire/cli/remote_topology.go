package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// Checkpoint destinations are unambiguous in the ordinary single-remote,
// single-URL repo and stop being unambiguous in two topologies users set up
// deliberately. Neither is broken, but in both the destination is decided by
// something other than "the repo I work in", so it is worth saying out loud once
// at `entire enable` and on demand from `entire doctor` rather than letting
// someone discover it when a resume comes up empty.

// remoteDestination is one remote and what checkpoints pushed to it would do.
type remoteDestination struct {
	name string
	// pushURLs are the URLs a push to this remote delivers to, in git's order.
	pushURLs []string
	// pinned reports that remote.PushURL resolves this remote to a configured
	// checkpoint_remote, so its own push URLs are irrelevant to checkpoints.
	//
	// Asked of the resolver rather than derived from settings on purpose: a
	// checkpoint_remote that is *present* is not necessarily *in effect* —
	// PushURL falls back to the push remote on an owner mismatch, an
	// unparseable URL, or a protocol it cannot map. Reading settings directly
	// would report "pinned" while pushes really went elsewhere, the same class
	// of bug the CoreOrigin() rule in CLAUDE.md exists to prevent.
	pinned bool
}

// fansOut reports whether checkpoints pushed to this remote face more than one
// destination.
func (d remoteDestination) fansOut() bool { return !d.pinned && len(d.pushURLs) > 1 }

// remoteTopology summarizes checkpoint-destination ambiguity in this repo.
type remoteTopology struct {
	// destinations is every configured remote, sorted by name.
	destinations []remoteDestination
	// primaryIsRefs reports whether the git-refs backend is active, which
	// decides what a fanning-out remote means for checkpoints.
	primaryIsRefs bool
	// electionErr is the fail-closed outcome of the checkpoint sync election,
	// nil when a remote was elected (or when there is simply nothing to elect).
	//
	// Asked of the resolver for the same reason `pinned` is: every sentence
	// this type writes about where checkpoints go is a claim about what the
	// election decided, and a note that narrates the election without consulting
	// it states its rules as facts. It did — it told a repo whose election had
	// failed closed that checkpoints "sync to a single elected remote" and to run
	// `entire status` to see which one, when the answer was none and nothing was
	// syncing anywhere.
	electionErr error
	// pushDisabled reports that push_sessions is off, which makes every claim
	// below about where a push delivers checkpoints conditional: nothing is
	// pushed at all. Read as a caveat on the note, not a reason to suppress
	// it — the ambiguity is still what re-enabling pushing would run into,
	// and still what a user pins with checkpoint_remote.
	pushDisabled bool
	// explicitRemote is the remote a valid checkpoint_push_remote settles on.
	// Named for its provenance rather than for the election: every resolver
	// tier elects a remote, but only this one is an explicit checkpoint
	// destination choice, and that is what justifies suppressing the note.
	// That remote's own push URLs may still need a warning.
	//
	// Empty when no valid explicit selection resolved, which is three states,
	// not one: no setting at all; a setting naming a remote that is not
	// configured (fail-closed — checkpoint sync is disabled entirely, which
	// electionErr above carries and this note reports); or settings that could
	// not be read.
	//
	// Observed elections deliberately retain the choice explanation: they are
	// inferred from a past push, not an explicit checkpoint destination choice.
	explicitRemote string
}

// inspectRemoteTopology reads the repo's remotes and checkpoint configuration.
// Best-effort and offline: every failure yields an empty topology, which reports
// nothing, because this is advisory output that must never obstruct enable or
// doctor.
func inspectRemoteTopology(ctx context.Context) remoteTopology {
	var t remoteTopology

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return t
	}

	// One `git remote -v` rather than `git remote` plus a get-url per remote:
	// it already applies git's pushurl-replaces-url rule and lists every push
	// URL in order, so N+1 subprocesses collapse to one — and all of it runs in
	// repoRoot instead of mixing dir-aware and cwd-dependent lookups.
	pushURLs, err := pushURLsByRemote(ctx, repoRoot)
	if err != nil {
		logging.Debug(ctx, "remote topology: could not read remotes", slog.String("error", err.Error()))
		return t
	}

	names := make([]string, 0, len(pushURLs))
	for name := range pushURLs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		dest := remoteDestination{name: name, pushURLs: pushURLs[name]}
		if _, enabled, err := remote.PushURL(ctx, name); err == nil {
			dest.pinned = enabled
		}
		t.destinations = append(t.destinations, dest)
	}

	if cpCfg, err := settings.LoadCheckpointsConfig(ctx); err == nil {
		t.primaryIsRefs = checkpoint.PrimaryIsRefs(cpCfg)
	}
	// Best-effort like everything else here: an unreadable settings file reads
	// as "pushing enabled", which is the note this code has always printed.
	if s, err := settings.Load(ctx); err == nil {
		t.pushDisabled = s.IsPushSessionsDisabled()
	}
	// One election, read two ways: its failure is what this note reports as a
	// disabled sync, and an explicit checkpoint_push_remote is what suppresses
	// the ambiguity advice. Local-only — the resolver reads settings and
	// .git/config, both memoized by the root context's git-remote cache, and
	// never dials.
	elected, electionErr := strategy.ResolveCheckpointSyncRemote(ctx)
	t.electionErr = electionErr
	if electionErr == nil && elected.Source == strategy.SyncRemoteSourceConfig {
		t.explicitRemote = elected.Name
	}

	return t
}

// pushURLsByRemote parses `git remote -v` into remote name -> push URLs in git's
// own order.
func pushURLsByRemote(ctx context.Context, dir string) (map[string][]string, error) {
	out, err := gitRunner(ctx, dir, "remote", "-v")
	if err != nil {
		return nil, fmt.Errorf("list git remotes: %w", err)
	}
	urls := make(map[string][]string)
	for _, line := range strings.Split(out, "\n") {
		// "<name>\t<url> (push)" — fetch lines are the same shape and ignored.
		name, rest, found := strings.Cut(strings.TrimSpace(line), "\t")
		if !found || !strings.HasSuffix(rest, "(push)") {
			continue
		}
		url := strings.TrimSpace(strings.TrimSuffix(rest, "(push)"))
		if url != "" {
			urls[name] = append(urls[name], url)
		}
	}
	// An empty map is a legitimate answer (a repo with no remotes), so it is
	// returned as such rather than as an error the caller would have to classify.
	return urls, nil
}

// ambiguous reports whether anything is worth telling the user: a remote whose
// checkpoints face several push URLs, or several remotes to choose between.
func (t remoteTopology) ambiguous() bool {
	unpinned := 0
	for _, d := range t.destinations {
		if t.fanOutMatters(d) {
			return true
		}
		if !d.pinned {
			unpinned++
		}
	}
	return t.explicitRemote == "" && unpinned > 1
}

// fanOutMatters excludes every remote but the explicit one once
// checkpoint_push_remote settles the destination: the others' push URLs carry
// code but no checkpoint data. A pinned remote fans out to nothing regardless,
// so fansOut() is asked first.
func (t remoteTopology) fanOutMatters(d remoteDestination) bool {
	return d.fansOut() && (t.explicitRemote == "" || d.name == t.explicitRemote)
}

// syncAffected reports that the election failed closed AND that the failure
// costs this repo something: at least one remote whose pushes would otherwise
// have carried checkpoints now carries none.
//
// A pinned remote is exempt because the pre-push gate exempts it — a dedicated
// checkpoint_remote URL is addressed directly rather than elected, so its
// checkpoints still ship (the gate applies the election only when
// ps.hasCheckpointURL() is false). A repo whose every remote is pinned is
// therefore syncing normally and is told nothing. A repo with no remotes at all
// is still told: nothing syncs there either way, but the broken setting is real
// and travels with the settings file to clones that do have remotes.
func (t remoteTopology) syncAffected() bool {
	if t.electionErr == nil {
		return false
	}
	return len(t.destinations) == 0 || len(t.unpinnedNames()) > 0
}

func (t remoteTopology) hasPinnedDestination() bool {
	for _, destination := range t.destinations {
		if destination.pinned {
			return true
		}
	}
	return false
}

// describeSyncDisabled writes the fail-closed election: what is broken, what it
// costs, and — only for the cause we recognize — how to fix it.
//
// The remedy is gated on errors.As rather than printed unconditionally, because
// the resolver also fails closed on an unreadable settings file; advice about
// naming a real remote would be the wrong answer to a file we could not open.
func (t remoteTopology) describeSyncDisabled(w io.Writer, header string) {
	fmt.Fprintln(w, header)

	var misconfigured *strategy.CheckpointPushRemoteNotConfiguredError
	if errors.As(t.electionErr, &misconfigured) {
		fmt.Fprintf(w, "  checkpoint_push_remote names %q, which is not one of this repo's remotes,\n",
			misconfigured.Remote)
		fmt.Fprintln(w, "  so nothing is elected to carry checkpoints.")
	} else {
		fmt.Fprintf(w, "  %s.\n", t.electionErr)
	}

	// The remote list gets a line of its own rather than a slot inside a
	// sentence: every line here is hand-wrapped, and a name list interpolated
	// mid-sentence moves the wrap by however long the names happen to be.
	if names := t.unpinnedNames(); len(names) > 0 {
		fmt.Fprintf(w, "  Affected remotes: %s\n", strings.Join(names, ", "))
		fmt.Fprintln(w, "  Pushes to them carry your code but no session history, which stays in")
		fmt.Fprintln(w, "  this clone.")
	}

	if misconfigured == nil {
		return
	}
	fmt.Fprintln(w, "  Fix: remove strategy_options.checkpoint_push_remote to let Entire elect one,")
	fmt.Fprintln(w, "  or point it at a remote this repo actually has.")
	fmt.Fprintln(w, "  A remote name is a per-clone fact: prefer .entire/settings.local.json, since a")
	fmt.Fprintln(w, "  committed one disables checkpoint sync in every clone that lacks that name.")
}

// describeCheckpointDestination writes an explanation of where checkpoints go,
// under the header matching what it found. Writes nothing when the destination
// is unambiguous and the election succeeded.
//
// Election-failure and ambiguity reports are exclusive on purpose: while an
// unpinned destination is disabled there is no elected destination to describe.
// A mixed topology uses the partial heading because pinned destinations bypass
// election and continue syncing.
func (t remoteTopology) describeCheckpointDestination(w io.Writer, headers checkpointNoteHeaders) {
	if t.syncAffected() {
		header := headers.disabled
		if t.hasPinnedDestination() {
			header = headers.partial
		}
		t.describeSyncDisabled(w, header)
		return
	}
	if !t.ambiguous() {
		return
	}

	fmt.Fprintln(w, headers.ambiguous)

	// Said first, because it qualifies every "pushes to" below. The note is
	// still worth printing: the ambiguity it describes is what re-enabling
	// pushing would run into, and it is what checkpoint_remote pins.
	//
	// Deliberately silent about where checkpoints are READ from, even though
	// that is the live question when nothing is pushed: what this note lists
	// is PUSH URLs, and a fan-out remote's reads use its fetch URL instead
	// (the git-branch branch below says as much — "only the fetch URL is ever
	// reconciled").
	//
	// It points at `entire status` for that rather than answering it, and
	// without promising an answer: status names a read source where it can
	// establish one, and says so when it cannot — a failed election with a
	// configured checkpoint_remote resolves to neither.
	if t.pushDisabled {
		fmt.Fprintln(w, "  Automatic checkpoint pushing is disabled (push_sessions=false), so no")
		fmt.Fprintln(w, "  checkpoints are pushed anywhere right now. The destination below is where")
		fmt.Fprintln(w, "  they would go if you re-enabled it; for where they are read from today,")
		fmt.Fprintln(w, "  see `entire status`.")
	}

	t.describeFanout(w)
	if names := t.unpinnedNames(); t.explicitRemote == "" && len(names) > 1 {
		fmt.Fprintf(w, "  This repo has %d remotes (%s).\n", len(names), strings.Join(names, ", "))
		fmt.Fprintln(w, "    Checkpoints sync to a single elected remote — not to whichever one you")
		fmt.Fprintln(w, "    push to. A push to any other remote carries your code but no session")
		fmt.Fprintln(w, "    history. Run `entire status` to see the elected destination and how much")
		fmt.Fprintln(w, "    checkpoint data has not reached it.")
	}

	fmt.Fprintln(w, "  To pin one repository for checkpoints, set checkpoint_remote in")
	fmt.Fprintln(w, "  .entire/settings.json (or .entire/settings.local.json to keep it to this clone).")
}

// describeFanout is shared by doctor's complete topology report and enable's
// report of the selected checkpoint destination.
func (t remoteTopology) describeFanout(w io.Writer) {
	for _, d := range t.destinations {
		if !t.fanOutMatters(d) {
			continue
		}
		fmt.Fprintf(w, "  Remote %q pushes to %d URLs:\n", d.name, len(d.pushURLs))
		for i, u := range d.pushURLs {
			marker := "  "
			if i == 0 && t.primaryIsRefs {
				marker = "→ "
			}
			fmt.Fprintf(w, "    %s%s\n", marker, gitremote.RedactURLOrPath(u))
		}
		if t.primaryIsRefs {
			fmt.Fprintln(w, "    Checkpoints go to the first URL only; the others receive your code but")
			fmt.Fprintln(w, "    no session history. Clone that first repository to resume elsewhere.")
		} else {
			fmt.Fprintln(w, "    Checkpoints are pushed to every URL. If one rejects them or is")
			fmt.Fprintln(w, "    unreachable it is reported and left behind, and only the fetch URL is")
			fmt.Fprintln(w, "    ever reconciled — so those URLs can fall permanently out of date.")
		}
	}
}

// unpinnedNames lists the remotes whose checkpoint destination is not already
// pinned by a checkpoint_remote.
func (t remoteTopology) unpinnedNames() []string {
	var names []string
	for _, d := range t.destinations {
		if !d.pinned {
			names = append(names, d.name)
		}
	}
	return names
}

// checkpointNoteHeaders are the caller's headings for the verdicts this note
// reports. Disabled and partial sync are misconfigurations to fix, while an
// ambiguous destination is a working repo whose owner should know which remote
// gets the history.
type checkpointNoteHeaders struct {
	disabled  string
	partial   string
	ambiguous string
}

// doctorCheckpointNoteHeaders phrases the note as one of doctor's verdicts,
// matching the "<subject>: <STATE>" shape its other checks print.
//
// A var rather than a literal at the call site so the tests assert on the text
// a user actually sees: a copy in the test file stays green when the heading
// here changes, which is the one thing those tests exist to catch.
var doctorCheckpointNoteHeaders = checkpointNoteHeaders{
	disabled:  "Checkpoint sync: DISABLED",
	partial:   "Checkpoint sync: PARTIAL",
	ambiguous: "Checkpoint destination: REVIEW",
}

// printCheckpointDestinationNote explains where checkpoints go when this repo's
// remotes make that a choice, and says so when they go nowhere. Shared by
// `entire enable` — the moment a user is most likely to be looking, and the
// least surprising place to learn it — and by `entire doctor`, which reports it
// on demand. Silent on the ordinary repo, so it adds nothing to the common
// output.
func printCheckpointDestinationNote(ctx context.Context, w io.Writer, headers checkpointNoteHeaders) {
	inspectRemoteTopology(ctx).describeCheckpointDestination(w, headers)
}
