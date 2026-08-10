package remote

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/go-git/go-git/v6/plumbing"
)

// WriteProbeFetchBudget bounds the on-demand ref fetch performed by a
// BACKFILL's absence probe. Write paths run inside git hooks (post-commit,
// stop-time finalize) where a dead network must not stall the user's
// workflow for the read path's full fetch window; combined with the
// per-store failure memo in the git-refs store, a loop over N checkpoints
// pays a dead network once, briefly.
const WriteProbeFetchBudget = 15 * time.Second

// readFetchTimeout bounds the interactive read-path fetch (unchanged from
// the historical FetchCheckpointRef behavior).
const readFetchTimeout = 2 * time.Minute

// CheckpointFetchTarget returns the git remote (URL or name) that checkpoint
// data is fetched from. It prefers the effective URL resolved by FetchURL,
// which is the source of truth for checkpoint fetch location. If URL
// resolution fails, it falls back to the origin remote name so callers can
// still attempt a fetch.
func CheckpointFetchTarget(ctx context.Context) string {
	target, _ := checkpointFetchTarget(ctx)
	return target
}

// checkpointFetchTarget is CheckpointFetchTarget plus whether the target is
// authoritative for checkpoint refs (see fetchURLAuthoritative). The bare
// "origin" fallbacks are non-authoritative: they exist so a fetch can still
// be attempted, not to certify where checkpoint refs live.
func checkpointFetchTarget(ctx context.Context) (string, bool) {
	url, authoritative, err := fetchURLAuthoritative(ctx)
	if err == nil && url != "" {
		return url, authoritative
	}
	return "origin", false
}

// FetchCheckpointRef fetches a single per-checkpoint ref
// (refs/entire/checkpoints/<shard>/<id>) from the checkpoint remote into the
// local ref of the same name, so the git-refs store can resolve a checkpoint
// written on another machine.
//
// Contract — absence is distinguishable from failure:
//   - The remote genuinely lacking the ref returns an error wrapping
//     plumbing.ErrReferenceNotFound (probed via ls-remote before fetching,
//     because `git fetch` of a missing refspec fails indistinguishably from a
//     transport error). Store probes classify this as "checkpoint not found",
//     which write routing may legitimately act on.
//   - Any transport-level failure (probe or fetch) is surfaced as a real
//     error, never mapped to absence — a false "absent" would misdirect a
//     backfill onto another backend instead of retrying.
//   - A repository with no git remotes at all and no checkpoint_remote
//     configured also returns an error wrapping plumbing.ErrReferenceNotFound
//     without probing: there is no remote that could host the ref.
func FetchCheckpointRef(ctx context.Context, ref plumbing.ReferenceName) error {
	ctx, cancel := context.WithTimeout(ctx, readFetchTimeout)
	defer cancel()

	fetchTarget, authoritative := checkpointFetchTarget(ctx)

	// A fully local repository — no git remotes at all and no
	// checkpoint_remote configured — has no remote that could host checkpoint
	// refs, so the ref's local absence is the final verdict. Without this,
	// the origin-name fallback below probes a remote git cannot resolve
	// ("'origin' does not appear to be a git repository", exit 128) and a
	// remoteless repo is misreported as a transport outage. Absence is only
	// ever classified on positive evidence; each escape below keeps today's
	// hard-failure probe:
	//   - dead caller context: every subprocess fails for reasons that say
	//     nothing about the repository
	//   - unreadable settings: a configured checkpoint remote cannot be
	//     ruled out
	//   - checkpoint_remote key present in any form, valid or malformed
	//     (see settings.HasCheckpointRemoteKey for why key presence, not
	//     GetCheckpointRemote, is the signal)
	//   - any remote listed, or the listing itself failing: checkpoint refs
	//     are pushed to whatever remote the pre-push hook fires for, so a
	//     repo with only non-origin remotes is NOT remoteless
	// The skipped-guard cases surface through the ordinary probe paths: a
	// missing origin fails the ls-remote probe as a transport error, and an
	// origin that merely lacks the ref hits the fallback-emptiness refusal
	// below — neither classifies as absence.
	if fetchTarget == originRemote && !authoritative && ctx.Err() == nil {
		s, loadErr := settings.Load(ctx)
		switch {
		case loadErr != nil:
			logging.Warn(ctx, "checkpoint probe: settings unreadable; cannot rule out a configured checkpoint remote, probing anyway",
				slog.String("error", loadErr.Error()))
		case !s.HasCheckpointRemoteKey() && repoHasNoRemotes(ctx):
			logging.Debug(ctx, "checkpoint probe: repository has no git remotes; classifying ref as absent",
				slog.String("ref", ref.String()))
			return fmt.Errorf("checkpoint ref %s: repository has no git remotes to fetch from: %w", ref, plumbing.ErrReferenceNotFound)
		}
	}

	return probeAndFetchCheckpointRef(ctx, probeFetchSpec{target: fetchTarget, authoritative: authoritative}, ref)
}

// FetchCheckpointRefInto fetches a single per-checkpoint ref from an explicitly
// named remote URL (rather than the current repo's resolved checkpoint remote)
// into the repository at dir, under the local ref of the same name. The URL is
// treated as authoritative for checkpoint refs: an empty ls-remote probe
// returns an error wrapping plumbing.ErrReferenceNotFound, distinguishable
// from transport failures (see FetchCheckpointRef's contract). Used by
// cross-repo explain, which fetches a checkpoint ref from another repo's
// Entire mirror into a throwaway repo — never into the cwd repo, whose
// checkpoint namespace must not absorb foreign checkpoints. The fetch is
// always unfiltered: the target repo is discarded after the read, so it can
// never lazy-fetch blobs elided by --filter.
func FetchCheckpointRefInto(ctx context.Context, dir, url string, ref plumbing.ReferenceName) error {
	ctx, cancel := context.WithTimeout(ctx, readFetchTimeout)
	defer cancel()
	return probeAndFetchCheckpointRef(ctx, probeFetchSpec{target: url, dir: dir, authoritative: true, noFilter: true}, ref)
}

// probeFetchSpec bundles probeAndFetchCheckpointRef's non-ref inputs: the
// remote to dial, the repository directory to operate in (empty = CWD), the
// authoritativeness of the remote, and whether to force an unfiltered fetch.
type probeFetchSpec struct {
	target        string
	dir           string
	authoritative bool
	noFilter      bool
}

// probeAndFetchCheckpointRef is the shared probe+fetch core: ls-remote probe
// (absence detection), then a single-refspec fetch with NoTags. authoritative
// controls whether an empty probe classifies as absence
// (plumbing.ErrReferenceNotFound) or is refused (see FetchCheckpointRef).
func probeAndFetchCheckpointRef(ctx context.Context, spec probeFetchSpec, ref plumbing.ReferenceName) error {
	fetchTarget := spec.target
	out, err := LsRemoteInDir(ctx, spec.dir, fetchTarget, ref.String())
	if err != nil {
		// Redact: fetchTarget can be a remote URL with embedded credentials
		// (CI origin URLs), and this error is logged and shown to users.
		return fmt.Errorf("probe checkpoint ref %s on %s: %w", ref, RedactURL(fetchTarget), err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		if !spec.authoritative {
			// The probe hit an origin FALLBACK while a checkpoint_remote is
			// configured (or undeterminable) — a remote that may simply never
			// host the configured checkpoint refs. Emptiness there proves
			// nothing; classifying it as absence would silently drop backfills
			// for checkpoints that exist on the real checkpoint remote.
			return fmt.Errorf("checkpoint ref %s not visible on fallback remote %s, and the configured checkpoint remote could not be resolved; refusing to treat this as absence", ref, RedactURL(fetchTarget))
		}
		return fmt.Errorf("checkpoint ref %s not found on %s: %w", ref, RedactURL(fetchTarget), plumbing.ErrReferenceNotFound)
	}

	refSpec := "+" + ref.String() + ":" + ref.String()
	if fetchOut, err := Fetch(ctx, FetchOptions{
		Remote:   fetchTarget,
		RefSpecs: []string{refSpec},
		NoTags:   true,
		NoFilter: spec.noFilter,
		Dir:      spec.dir,
	}); err != nil {
		// Fold git's own output into the error (redacted): a bare
		// "exit status 128" is undebuggable in hook Warn logs.
		msg := strings.TrimSpace(string(fetchOut))
		msg = strings.ReplaceAll(msg, fetchTarget, RedactURL(fetchTarget))
		if msg != "" {
			return fmt.Errorf("fetch checkpoint ref %s from %s: %s: %w", ref, RedactURL(fetchTarget), msg, err)
		}
		return fmt.Errorf("fetch checkpoint ref %s from %s: %w", ref, RedactURL(fetchTarget), err)
	}
	return nil
}

// repoHasNoRemotes reports whether the repository at the current directory
// definitively has no git remotes configured. Only a successful, empty
// `git remote` listing counts as proof; any error (dead context, missing git
// binary, not a repository) returns false so the caller falls through to the
// probe instead of classifying absence off an undifferentiated failure.
func repoHasNoRemotes(ctx context.Context) bool {
	out, err := exec.CommandContext(ctx, "git", "remote").Output()
	return err == nil && len(bytes.TrimSpace(out)) == 0
}

// HookCheckpointRefFetcher returns the write-probe fetcher for git-hook
// contexts (post-commit attribution, stop-time transcript finalize): the
// bounded budget plus BatchMode SSH, so a passphrase-protected key can never
// prompt — or invisibly hang — inside a hook the user's git command is
// waiting on.
func HookCheckpointRefFetcher() func(context.Context, plumbing.ReferenceName) error {
	bounded := BoundedCheckpointRefFetcher(WriteProbeFetchBudget)
	return func(ctx context.Context, ref plumbing.ReferenceName) error {
		return bounded(WithNonInteractiveSSH(ctx), ref)
	}
}

// BoundedCheckpointRefFetcher returns a RefFetchFunc-shaped fetcher whose
// per-call budget is capped at d, for wiring into write-path checkpoint
// stores (see WriteProbeFetchBudget).
func BoundedCheckpointRefFetcher(d time.Duration) func(context.Context, plumbing.ReferenceName) error {
	return func(ctx context.Context, ref plumbing.ReferenceName) error {
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return FetchCheckpointRef(ctx, ref)
	}
}
