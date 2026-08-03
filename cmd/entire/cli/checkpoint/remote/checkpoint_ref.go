package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	target, _, _ := checkpointFetchTarget(ctx)
	return target
}

// checkpointFetchTarget is CheckpointFetchTarget plus two facts about the
// returned target:
//   - authoritative: whether the target is certified as the host of checkpoint
//     refs (see fetchURLAuthoritative). The bare "origin" fallbacks are
//     non-authoritative: they exist so a fetch can still be attempted, not to
//     certify where checkpoint refs live.
//   - resolved: whether a concrete fetch URL/remote was resolved at all. It is
//     false ONLY when no checkpoint remote is configured (no origin remote and
//     no configured checkpoint_remote) and we fell back to the bare "origin"
//     name. That literal "origin" is not guaranteed to be a real git remote, so
//     a probe failure against it says only "there is nowhere to fetch from". If
//     a checkpoint_remote IS configured but its URL can't be derived this call,
//     resolved is true so a probe failure surfaces as a real error, not absence.
func checkpointFetchTarget(ctx context.Context) (target string, authoritative, resolved bool) {
	url, auth, err := fetchURLAuthoritative(ctx)
	if err == nil && url != "" {
		return url, auth, true
	}
	if errors.Is(err, errNoCheckpointRemoteConfigured) {
		// Genuinely no checkpoint remote configured: a probe failure against the
		// bare "origin" fallback proves only "nowhere to fetch from", so callers
		// classify it as checkpoint absence and fall back to the v1-branch store.
		return "origin", false, false
	}
	// A checkpoint remote IS configured but its URL could not be derived this
	// call (transient GetRemoteURL error, provider-host derivation failure). Mark
	// resolved so a probe failure surfaces as a real error rather than being
	// silently reclassified as checkpoint absence.
	return "origin", false, true
}

// errNoCheckpointRemoteConfigured marks the "no checkpoint remote configured at
// all" case (no origin remote and no configured checkpoint_remote) — the only
// situation in which a probe failure against the bare "origin" fallback is
// classified as checkpoint absence rather than surfaced as a real error.
var errNoCheckpointRemoteConfigured = errors.New("no checkpoint remote configured")

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
func FetchCheckpointRef(ctx context.Context, ref plumbing.ReferenceName) error {
	ctx, cancel := context.WithTimeout(ctx, readFetchTimeout)
	defer cancel()

	fetchTarget, authoritative, resolved := checkpointFetchTarget(ctx)

	out, err := LsRemoteInDir(ctx, "", fetchTarget, ref.String())
	if err != nil {
		if !resolved {
			// No checkpoint remote is configured or reachable — resolution fell
			// back to the bare "origin" name, which need not be a real git
			// remote (the local repo may have no origin at all). A probe failure
			// here proves only that there is nowhere to fetch from, not that the
			// checkpoint is missing on a real remote. Classify as absence so the
			// git-refs store maps it to ErrCheckpointNotFound and write routing
			// falls back to the v1-branch store, mirroring the branch-existence
			// path (BranchExistsOnRemote treats an ls-remote failure as "not
			// found"). A genuine transport failure against a *resolved* remote
			// still propagates below, so real offline/network loss is never
			// silently swallowed as absence.
			return fmt.Errorf("probe checkpoint ref %s: no checkpoint remote configured: %w", ref, plumbing.ErrReferenceNotFound)
		}
		// Redact: fetchTarget can be a remote URL with embedded credentials
		// (CI origin URLs), and this error is logged and shown to users.
		return fmt.Errorf("probe checkpoint ref %s on %s: %w", ref, RedactURL(fetchTarget), err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		if !authoritative {
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
