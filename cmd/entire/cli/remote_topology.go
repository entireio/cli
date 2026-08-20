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
	// syncRemote is the elected checkpoint sync remote — the one remote whose
	// pushes carry checkpoints — and syncSource says which tier elected it.
	// Both are empty when the election found nothing or failed, and the note
	// then says nothing about where checkpoints go: naming a destination the
	// push side would not use is the failure this note exists to prevent.
	syncRemote string
	syncSource strategy.CheckpointSyncRemoteSource
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

	// Ask the resolver rather than reading checkpoint_push_remote out of
	// settings, for the same reason `pinned` does: the destination shown must
	// be the one pushes actually use, never a re-derivation that can disagree.
	if elected, err := strategy.ResolveCheckpointSyncRemote(ctx); err == nil {
		t.syncRemote, t.syncSource = elected.Name, elected.Source
	} else {
		logging.Debug(ctx, "remote topology: could not resolve the checkpoint sync remote",
			slog.String("error", err.Error()))
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

	for _, d := range t.destinations {
		if !d.fansOut() {
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

	if names := t.unpinnedNames(); len(names) > 1 {
		fmt.Fprintf(w, "  This repo has %d remotes (%s).\n", len(names), strings.Join(names, ", "))
		t.describeSyncRemote(w)
	}

	fmt.Fprintln(w, "  To pin one repository for checkpoints, set checkpoint_remote in")
	fmt.Fprintln(w, "  .entire/settings.json (or .entire/settings.local.json to keep it to this clone).")
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
func (t remoteTopology) describeSyncRemote(w io.Writer) {
	if t.syncRemote == "" {
		return
	}
	switch t.syncSource {
	case strategy.SyncRemoteSourceConfig:
		// The decision is already made; repeating the setting key would read
		// as a warning that something still needs doing.
		fmt.Fprintf(w, "    Checkpoints sync to one remote — %q (set by checkpoint_push_remote).\n", t.syncRemote)
	case strategy.SyncRemoteSourceObserved:
		// Same fact `entire status` reports as "follows your branch's push
		// destination", shortened to hold one line at this indent.
		fmt.Fprintf(w, "    Checkpoints sync to one remote — %q, your branch's push destination.\n", t.syncRemote)
		fmt.Fprintln(w, "    Set strategy_options.checkpoint_push_remote to pin a different one.")
	default:
		fmt.Fprintf(w, "    Checkpoints sync to one remote — right now %q, and your first push to\n", t.syncRemote)
		fmt.Fprintln(w, "    your branch's own remote takes it over. `entire status` shows where.")
		fmt.Fprintln(w, "    Set strategy_options.checkpoint_push_remote to pin one yourself.")
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
