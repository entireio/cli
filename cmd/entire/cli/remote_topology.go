package cli

import (
	"context"
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
	// sync is where checkpoints actually go, from the same computation behind
	// `entire status` and `entire status --json` (status.go), so the three
	// surfaces cannot disagree about one election. Its Unpushed is always 0
	// here: the note needs the destination only, and must stay cheap.
	//
	// sync.Remote is empty when the election found nothing or failed, and the
	// note then names no destination — naming one the push side would not use
	// is the failure this note exists to prevent. sync.Err is a fail-closed
	// election, the one case where checkpoints go nowhere at all, which the
	// note must not stay silent about. An empty name with no error is the
	// third case: the resolver answers that way when its own `git config` read
	// fails transiently (cachedRemotesInConfigOrder returns nil rather than
	// caching the failure), so the note carries a tier-agnostic fallback too.
	sync checkpointSyncInfo
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

	// Share the status computation rather than re-deriving the destination, for
	// the same reason `pinned` asks the resolver: what is shown must be what
	// pushes actually use. A local re-derivation here is what let this note
	// name a remote while `entire status` named a dedicated store.
	//
	// A settings read failure degrades to a nil settings object — dedicated
	// mode simply goes undetected — rather than becoming an Err, which the note
	// renders as "Checkpoints are NOT syncing". A transient read failure is not
	// that, and this note must never obstruct enable or doctor.
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		logging.Debug(ctx, "remote topology: could not load settings",
			slog.String("error", err.Error()))
		s = nil
	}
	t.sync = resolveCheckpointSyncDestination(ctx, s)

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
		if d.fansOut() {
			return true
		}
		if !d.pinned {
			unpinned++
		}
	}
	return unpinned > 1
}

// describeCheckpointDestination writes an explanation of where checkpoints go,
// under the given header. Writes nothing when the destination is unambiguous.
func (t remoteTopology) describeCheckpointDestination(w io.Writer, header string) {
	if !t.ambiguous() {
		return
	}

	fmt.Fprintln(w, header)

	// Destination first: every claim below it is about how checkpoints reach
	// that destination, and the URL breakdown read as the whole answer when it
	// came first.
	if names := t.unpinnedNames(); len(names) > 1 {
		// Every remote, not just the unpinned ones. The count orients the
		// reader in their own repo, and omitting a pinned remote let the note
		// name a destination missing from its own list.
		all := make([]string, 0, len(t.destinations))
		for _, d := range t.destinations {
			all = append(all, d.name)
		}
		fmt.Fprintf(w, "  This repo has %d remotes (%s).\n", len(all), strings.Join(all, ", "))
		t.describeSyncRemote(w)
	}

	t.describeIgnoredStore(w)

	for _, d := range t.destinations {
		// Only the elected remote's URLs are a checkpoint destination. Another
		// remote's pushes are gated out entirely (checkpointSyncAllowedForRemote),
		// so "checkpoints go to the first URL only" would be false of it — as it
		// is of every remote when the election failed or resolved to nothing.
		if !d.fansOut() || t.dedicated() || d.name != t.sync.Remote {
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

	// The advice is "configure a checkpoint_remote" — already done in both
	// states where one exists, and off-target when a fail-closed election means
	// nothing is syncing at all.
	if t.dedicated() || t.sync.IgnoredStore != "" || t.sync.Err != "" {
		return
	}
	fmt.Fprintln(w, "  To pin one repository for checkpoints, set checkpoint_remote in")
	fmt.Fprintln(w, "  .entire/settings.json (or .entire/settings.local.json to keep it to this clone).")
}

// dedicated reports that a checkpoint_remote store is the destination, so none
// of this repo's remotes is where checkpoints land.
func (t remoteTopology) dedicated() bool { return t.sync.dedicated() }

// describeIgnoredStore reports a checkpoint_remote that is configured but not in
// effect. Everywhere else this is a logging.Warn inside remote.PushURL, so the
// setting is silently ignored and the fallback looks like an ordinary election.
//
// The cause is stated as a likelihood, not a fact: PushURL returns a bare
// enabled=false for the ownership rejection, an unparseable push URL and an
// unmappable protocol alike, so the note cannot tell which applied.
func (t remoteTopology) describeIgnoredStore(w io.Writer) {
	if t.sync.IgnoredStore == "" {
		return
	}
	fmt.Fprintf(w, "    A checkpoint_remote (%q) is configured but not in effect\n", t.sync.IgnoredStore)
	fmt.Fprintln(w, "    here — most often because it looks like it belongs to another owner.")
	fmt.Fprintf(w, "    Checkpoints go to %q instead; set it in .entire/settings.local.json\n", t.sync.Remote)
	fmt.Fprintln(w, "    if the store is yours.")
}

// describeSyncRemote names the one remote that carries checkpoints. It stays
// to three lines at most on purpose: capture already routes the fork topology
// correctly and announces itself when it does, a gated push says so at the
// time (strategy.hintGatedCheckpointSync), and `entire status` reports the
// destination on demand — so this is orientation at setup, not a warning about
// something the user must go and fix.
//
// Naming today's remote alone would be a promise the next push can break,
// hence the second clause on the tiers where the election is still open: a
// push whose target agrees with the branch's declared push destination elects
// that remote and carries the checkpoints with it, so "checkpoints go to
// origin, full stop" is exactly what must not be said here.
//
// The "no session history" line is the inverse promise, and belongs only to
// the tiers where the election is settled: with checkpoint_push_remote set or
// a capture already in force, captureEligible refuses every displacement, so
// no push can move the destination and every push to another remote really
// does leave the transcripts behind. On the open tiers the same sentence would
// be false for the electing push itself.
//
// The exception to the orientation-not-warning rule is a failed election: that
// is fail-closed, so checkpoints are going nowhere at all and the note has to
// say so.
//
// A dedicated checkpoint_remote store is answered before the tier switch, and
// deliberately drops the "no session history" clause the settled tiers carry:
// in dedicated mode checkpoints never ride a code push to a remote at all, so
// there is no "other remote" for that sentence to be about. It names the pinned
// remotes instead, because those are the pushes that reach the store — a repo
// where none of them is ever pushed strands its transcripts, and nothing else
// in this note would say so.
func (t remoteTopology) describeSyncRemote(w io.Writer) {
	// A dedicated store is not a tier of the election — status.go synthesizes
	// it — so it is answered before the switch, which stays typed on the
	// resolver's enum so `exhaustive` keeps turning a new tier into a decision
	// here.
	if t.dedicated() {
		fmt.Fprintf(w, "    Checkpoints go to the dedicated store %q (set by\n", t.sync.Remote)
		fmt.Fprintf(w, "    checkpoint_remote), not to any of these remotes. Pushes to %s\n",
			quoteNames(t.pinnedNames()))
		fmt.Fprintln(w, "    carry them there.")
		return
	}
	if t.sync.Remote == "" {
		if t.sync.Err != "" {
			// Same fact `entire status` reports as "Checkpoints NOT syncing":
			// the pre-push gate is skipping checkpoint sync entirely until the
			// setting is fixed.
			fmt.Fprintf(w, "    Checkpoints are NOT syncing: %s\n", t.sync.Err)
			return
		}
		// The election could not be read at all. Saying nothing here would
		// leave a multi-remote repo with a bare remote count and no hint that
		// the destination is one remote rather than all of them, so fall back
		// to the part that holds on every tier.
		fmt.Fprintln(w, "    Checkpoints sync to a single elected remote; `entire status` shows which.")
		return
	}
	switch strategy.CheckpointSyncRemoteSource(t.sync.Source) {
	case strategy.SyncRemoteSourceConfig:
		// The decision is already made; repeating the setting key as advice
		// would read as a warning that something still needs doing.
		fmt.Fprintf(w, "    Checkpoints sync to one remote — %q (set by checkpoint_push_remote).\n", t.sync.Remote)
		fmt.Fprintln(w, "    A push to any other remote carries your code but no session history.")
	case strategy.SyncRemoteSourceObserved:
		// Same fact `entire status` reports as "follows your branch's push
		// destination", shortened to hold one line at this indent.
		fmt.Fprintf(w, "    Checkpoints sync to one remote — %q, your branch's push destination.\n", t.sync.Remote)
		fmt.Fprintln(w, "    A push to any other remote carries your code but no session history.")
		fmt.Fprintln(w, "    Set strategy_options.checkpoint_push_remote to pin a different one.")
	case strategy.SyncRemoteSourceDefault, strategy.SyncRemoteSourceSole, strategy.SyncRemoteSourceFirst:
		// One wording for all three because the user-visible fact is the same:
		// nothing has chosen the destination yet, so a push can still move it.
		// Sole is all but unreachable from this caller, which writes the note
		// only when two or more remotes are unpinned while the resolver
		// reaches sole with exactly one remote configured — the two count
		// remotes with different git commands, so the arm stays shared rather
		// than assert an impossibility.
		//
		// "a different remote" rather than "your branch's own remote": capture
		// requires the push target to both agree with the branch's declared
		// push destination and differ from today's election, so on the common
		// setup where they already agree, no push moves anything.
		fmt.Fprintf(w, "    Checkpoints sync to one remote — right now %q. A push to a different\n", t.sync.Remote)
		fmt.Fprintln(w, "    remote of your own can move it; `entire status` shows where they go.")
		fmt.Fprintln(w, "    Set strategy_options.checkpoint_push_remote to pin one yourself.")
	}
}

// pinnedNames lists the remotes a checkpoint_remote is in effect for — which is
// exactly the set whose pushes reach the store, since the pre-push exemption
// (pushSettings.hasCheckpointURL) is the same remote.PushURL answer that sets
// `pinned`.
func (t remoteTopology) pinnedNames() []string {
	var names []string
	for _, d := range t.destinations {
		if d.pinned {
			names = append(names, d.name)
		}
	}
	return names
}

// quoteNames renders remote names for prose: `"a"`, `"a" and "b"`, `"a", "b" and "c"`.
func quoteNames(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, fmt.Sprintf("%q", n))
	}
	switch len(quoted) {
	case 0:
		return "no remote"
	case 1:
		return quoted[0]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
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

// printCheckpointDestinationNote explains where checkpoints go when this repo's
// remotes make that a choice. Shared by `entire enable` — the moment a user is
// most likely to be looking, and the least surprising place to learn it — and by
// `entire doctor`, which reports it on demand. Silent on the ordinary repo, so it
// adds nothing to the common output.
func printCheckpointDestinationNote(ctx context.Context, w io.Writer, header string) {
	inspectRemoteTopology(ctx).describeCheckpointDestination(w, header)
}
