package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
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

	// One `git remote -v` for the whole function, and every claim below derived
	// from it. Asking git per remote meant asking the same two questions N+1
	// times per command and getting answers that could disagree with each other
	// — the note naming a store while claiming no remote reached it.
	snap, err := gitremote.LoadSnapshot(ctx, repoRoot)
	if err != nil {
		logging.Debug(ctx, "remote topology: could not read remotes", slog.String("error", err.Error()))
		return t
	}

	// A settings read failure degrades to a nil settings object — the store
	// simply goes undetected — rather than becoming an Err, which the note
	// renders as "Checkpoints are NOT syncing". A transient read failure is not
	// that, and this note must never obstruct enable or doctor.
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		logging.Debug(ctx, "remote topology: could not load settings",
			slog.String("error", err.Error()))
		s = nil
	}
	probe := newCheckpointStoreProbe(ctx, s, snap)

	for _, name := range snap.Names() {
		urls, _ := snap.Get(name)
		t.destinations = append(t.destinations, remoteDestination{
			name:     name,
			pushURLs: urls.Push,
			pinned:   probe.inEffectFor(ctx, name),
		})
	}

	if cpCfg, cfgErr := settings.LoadCheckpointsConfig(ctx); cfgErr == nil {
		t.primaryIsRefs = checkpoint.PrimaryIsRefs(cpCfg)
	}

	// Share the status computation rather than re-deriving the destination: what
	// is shown must be what pushes actually use. A local re-derivation here is
	// what let this note name a remote while `entire status` named a dedicated
	// store. The probe carries the same snapshot the loop above used, so the
	// destination and every remote's pinned flag cannot disagree.
	t.sync = resolveCheckpointSyncDestination(ctx, probe)

	return t
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
//
// The body is composed before the header is written, and a body that came out
// empty suppresses the header with it. Deciding the two separately is what
// printed `Checkpoint destination: REVIEW` and nothing else: `ambiguous()` said
// the repo needed explaining while every explaining branch was gated on
// something narrower. Composing first makes the header a consequence of having
// something to say rather than a second opinion about it.
func (t remoteTopology) describeCheckpointDestination(w io.Writer, header string) {
	if !t.ambiguous() {
		return
	}
	var body strings.Builder
	t.writeDestinationBody(&body)
	if body.Len() == 0 {
		return
	}
	fmt.Fprintln(w, header)
	fmt.Fprint(w, body.String())
}

// writeDestinationBody writes the explanation itself, destination first: every
// claim after it is about how checkpoints reach that destination, and the URL
// breakdown read as the whole answer when it came first.
func (t remoteTopology) writeDestinationBody(b *strings.Builder) {
	if len(t.unpinnedNames()) > 1 {
		// Every remote, not just the unpinned ones. The count orients the
		// reader in their own repo, and omitting a pinned remote let the note
		// name a destination missing from its own list.
		fmt.Fprintf(b, "  This repo has %d remotes (%s).\n", len(t.destinations), strings.Join(t.allNames(), ", "))
	}
	t.describeDestination(b)
	t.describeIgnoredStore(b)
	t.describeFanOut(b)

	// The advice is "configure a checkpoint_remote" — already taken in both
	// states where one exists, and off-target when a fail-closed election means
	// nothing is syncing at all. It also never stands alone: on its own it would
	// turn the note on for repos that have nothing else to report.
	if b.Len() == 0 || t.sync.storeConfigured() || t.sync.Err != "" {
		return
	}
	fmt.Fprintln(b, "  To pin one repository for checkpoints, set checkpoint_remote in")
	fmt.Fprintln(b, "  .entire/settings.json (or .entire/settings.local.json to keep it to this clone).")
}

// describeDestination names where checkpoints land. Every branch here holds
// whatever the repo's remote count is, which is why none of them sits behind the
// several-remotes gate: a fail-closed election and a dedicated store are facts
// about the configuration, and gating them on "more than one remote" is what
// left a broken checkpoint_push_remote as the state that said least.
//
// Only the tier wording is genuinely about choosing among several remotes, so
// only it stays gated. The note as a whole still sits behind ambiguous(),
// though: a single-remote, single-URL repo prints nothing even in these states,
// and `entire status` is the surface that always reports them.
func (t remoteTopology) describeDestination(b *strings.Builder) {
	switch {
	case t.sync.Err != "":
		// Same fact `entire status` reports as "Checkpoints NOT syncing": the
		// pre-push gate is skipping checkpoint sync entirely until the setting
		// is fixed. The one branch here that is a warning, not orientation.
		fmt.Fprintf(b, "  Checkpoints are NOT syncing: %s\n", t.sync.Err)
	case t.sync.dedicated():
		t.describeDedicatedStore(b)
	case t.sync.Remote == "":
		// The election could not be read at all. Saying nothing would leave a
		// multi-remote repo with a bare remote count and no hint that the
		// destination is one remote rather than all of them, so fall back to
		// the part that holds on every tier.
		fmt.Fprintln(b, "  Checkpoints sync to a single elected remote; `entire status` shows which.")
	default:
		if len(t.unpinnedNames()) > 1 {
			t.describeElectedTier(b)
		}
	}
}

// describeDedicatedStore explains a checkpoint_remote that is in effect: the
// destination is a repository of its own, and the pushes that reach it are the
// ones the store applies to.
//
// It drops the "no session history" clause the settled tiers carry: in this mode
// checkpoints never ride a code push to a remote at all, so there is no "other
// remote" for that sentence to be about. Naming the carriers matters instead —
// a repo that never pushes one of them strands its transcripts, and nothing else
// in this note would say so.
func (t remoteTopology) describeDedicatedStore(b *strings.Builder) {
	fmt.Fprintf(b, "  Checkpoints go to the dedicated store %q (set by\n", t.sync.Remote)
	fmt.Fprintln(b, "  checkpoint_remote), not to any of these remotes.")
	// The store is in effect because it resolved for the elected remote, and
	// that resolution and this list come from one snapshot — so there is
	// normally a name here. The exception is narrow enough to be worth a
	// sentence rather than a claim: the resolution can still take a branch that
	// opens the repository, and a failure there flips one answer and not the
	// other. Saying no remote carries them would be the one false thing to say,
	// since a push is reaching the store.
	if carriers := t.pinnedNames(); len(carriers) > 0 {
		fmt.Fprintf(b, "  Pushes to %s carry them there.\n", quoteNames(carriers))
		return
	}
	fmt.Fprintln(b, "  `entire status` shows whether your pushes are reaching it.")
}

// describeElectedTier words which remote carries checkpoints and how firmly.
// It stays to three lines at most on purpose: capture already routes the fork
// topology correctly and announces itself when it does, a gated push says so at
// the time (strategy.hintGatedCheckpointSync), and `entire status` reports the
// destination on demand — so this is orientation at setup, not a warning about
// something the user must go and fix.
//
// Naming today's remote alone would be a promise the next push can break, hence
// the second clause on the tiers where the election is still open: a push whose
// target agrees with the branch's declared push destination elects that remote
// and carries the checkpoints with it, so "checkpoints go to origin, full stop"
// is exactly what must not be said here.
//
// The "no session history" line is the inverse promise, and belongs only to the
// tiers where the election is settled: with checkpoint_push_remote set or a
// capture already in force, captureEligible refuses every displacement, so no
// push can move the destination and every push to another remote really does
// leave the transcripts behind. On the open tiers the same sentence would be
// false for the electing push itself.
func (t remoteTopology) describeElectedTier(b *strings.Builder) {
	switch strategy.CheckpointSyncRemoteSource(t.sync.Source) {
	case strategy.SyncRemoteSourceConfig:
		// The decision is already made; repeating the setting key as advice
		// would read as a warning that something still needs doing.
		fmt.Fprintf(b, "    Checkpoints sync to one remote — %q (set by checkpoint_push_remote).\n", t.sync.Remote)
		fmt.Fprintln(b, "    A push to any other remote carries your code but no session history.")
	case strategy.SyncRemoteSourceObserved:
		// Same fact `entire status` reports as "follows your branch's push
		// destination", shortened to hold one line at this indent.
		fmt.Fprintf(b, "    Checkpoints sync to one remote — %q, your branch's push destination.\n", t.sync.Remote)
		fmt.Fprintln(b, "    A push to any other remote carries your code but no session history.")
		fmt.Fprintln(b, "    Set strategy_options.checkpoint_push_remote to pin a different one.")
	case strategy.SyncRemoteSourceDefault, strategy.SyncRemoteSourceSole, strategy.SyncRemoteSourceFirst:
		// One wording for all three because the user-visible fact is the same:
		// nothing has chosen the destination yet, so a push can still move it.
		// Sole is all but unreachable from this caller, which words the tier
		// only when two or more remotes are unpinned while the resolver reaches
		// sole with exactly one remote configured — and the two count remotes
		// with different git reads, so the arm stays shared rather than assert
		// an impossibility.
		//
		// "a different remote" rather than "your branch's own remote": capture
		// requires the push target to both agree with the branch's declared
		// push destination and differ from today's election, so on the common
		// setup where they already agree, no push moves anything.
		fmt.Fprintf(b, "    Checkpoints sync to one remote — right now %q. A push to a different\n", t.sync.Remote)
		fmt.Fprintln(b, "    remote of your own can move it; `entire status` shows where they go.")
		fmt.Fprintln(b, "    Set strategy_options.checkpoint_push_remote to pin one yourself.")
	}
}

// describeIgnoredStore reports a checkpoint_remote that is configured but not in
// effect. Everywhere else this is a logging.Warn inside the URL resolution, so
// the setting is silently ignored and the fallback looks like an ordinary
// election.
//
// The cause is stated as a likelihood, not a fact: the resolution reports a bare
// "not in effect" for the ownership rejection, an unparseable push URL and an
// unmappable protocol alike, so the note cannot tell which applied.
func (t remoteTopology) describeIgnoredStore(b *strings.Builder) {
	// IgnoredStore is only ever set alongside an elected remote, which is what
	// the fallback destination named below is. The Remote check guards that
	// invariant here as well: naming %q of an empty string would render
	// `Checkpoints go to "" instead`.
	if t.sync.IgnoredStore == "" || t.sync.Remote == "" {
		return
	}
	fmt.Fprintf(b, "  A checkpoint_remote (%q) is configured but not in\n", t.sync.IgnoredStore)
	fmt.Fprintln(b, "  effect here — most often because it looks like it belongs to another owner.")
	fmt.Fprintf(b, "  Checkpoints go to %q instead; set it in .entire/settings.local.json\n", t.sync.Remote)
	fmt.Fprintln(b, "  if the store is yours.")
}

// describeFanOut breaks down the push URLs of the one remote whose pushes carry
// checkpoints.
//
// Only that remote's URLs are a checkpoint destination: a push to any other is
// gated out entirely (strategy.checkpointSyncAllowedForRemote), so "checkpoints
// go to the first URL only" would be false of it — as it is of every remote when
// the election failed or resolved to nothing, and of all of them when a
// dedicated store bypasses the remotes altogether.
func (t remoteTopology) describeFanOut(b *strings.Builder) {
	d := t.electedDestination()
	if d == nil || !d.fansOut() || t.sync.dedicated() {
		return
	}
	fmt.Fprintf(b, "  Remote %q pushes to %d URLs:\n", d.name, len(d.pushURLs))
	for i, u := range d.pushURLs {
		marker := "  "
		if i == 0 && t.primaryIsRefs {
			marker = "→ "
		}
		fmt.Fprintf(b, "    %s%s\n", marker, gitremote.RedactURLOrPath(u))
	}
	if t.primaryIsRefs {
		fmt.Fprintln(b, "    Checkpoints go to the first URL only; the others receive your code but")
		fmt.Fprintln(b, "    no session history. Clone that first repository to resume elsewhere.")
	} else {
		fmt.Fprintln(b, "    Checkpoints are pushed to every URL. If one rejects them or is")
		fmt.Fprintln(b, "    unreachable it is reported and left behind, and only the fetch URL is")
		fmt.Fprintln(b, "    ever reconciled — so those URLs can fall permanently out of date.")
	}
}

// electedDestination returns the remote whose pushes carry checkpoints, or nil
// when the election named nothing, failed, or named something that is not one of
// this repo's remotes — a dedicated store's org/repo slug, or a remote the
// election can see and `git remote -v` cannot.
func (t remoteTopology) electedDestination() *remoteDestination {
	if t.sync.Remote == "" {
		return nil
	}
	for i, d := range t.destinations {
		if d.name == t.sync.Remote {
			return &t.destinations[i]
		}
	}
	return nil
}

// allNames lists every configured remote, in the order the snapshot sorted them.
func (t remoteTopology) allNames() []string {
	names := make([]string, 0, len(t.destinations))
	for _, d := range t.destinations {
		names = append(names, d.name)
	}
	return names
}

// pinnedNames lists the remotes a checkpoint_remote is in effect for — exactly
// the set whose pushes reach the store, since the pre-push exemption
// (pushSettings.hasCheckpointURL) is the same answer that sets `pinned`.
func (t remoteTopology) pinnedNames() []string {
	var names []string
	for _, d := range t.destinations {
		if d.pinned {
			names = append(names, d.name)
		}
	}
	return names
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

// quoteNames renders remote names for prose: `"a"`, `"a" and "b"`, `"a", "b" and "c"`.
//
// Deliberately not shared with formatTokenClassList (checkpoint_tokens.go),
// which does the same joining for a different command: unifying them is a
// worthwhile cleanup, but it renames a function in an unrelated file, so it
// belongs in its own change rather than this one. Note the two already disagree
// on the Oxford comma.
//
// Empty in, empty out — a caller with no names to print must drop the sentence
// rather than print one about nothing.
func quoteNames(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, fmt.Sprintf("%q", n))
	}
	switch len(quoted) {
	case 0:
		return ""
	case 1:
		return quoted[0]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
	}
}

// printCheckpointDestinationNote explains where checkpoints go when this repo's
// remotes make that a choice. Shared by `entire enable` — the moment a user is
// most likely to be looking, and the least surprising place to learn it — and by
// `entire doctor`, which reports it on demand. Silent on the ordinary repo, so it
// adds nothing to the common output.
func printCheckpointDestinationNote(ctx context.Context, w io.Writer, header string) {
	inspectRemoteTopology(ctx).describeCheckpointDestination(w, header)
}
