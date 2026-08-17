package remote

import (
	"bytes"
	"context"
	"errors"
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

// readFetchTimeout bounds one complete interactive read-chain attempt.
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
// authoritative for checkpoint refs (see fetchURLAuthoritative). A resolved
// origin is also non-authoritative when checkpoint_push_remote elects another
// remote: it may still be probed as a fallback, but cannot certify absence.
func checkpointFetchTarget(ctx context.Context) (string, bool) {
	target, authoritative := checkpointFetchTargetFrom(ctx, "")
	if !authoritative {
		return target, false
	}

	s, err := settings.Load(ctx)
	if err != nil {
		return target, false
	}
	if s.HasCheckpointRemoteKey() {
		return target, s.GetCheckpointRemote() != nil
	}

	pushRemote := s.GetCheckpointPushRemote()
	return target, pushRemote == "" || pushRemote == originRemote
}

func checkpointFetchTargetFrom(ctx context.Context, leadRemote string) (string, bool) {
	url, authoritative, err := fetchURLAuthoritative(ctx, FetchURLOptions{LeadReadRemote: leadRemote})
	if err == nil && url != "" {
		return url, authoritative
	}
	if leadRemote != "" {
		return leadRemote, false
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

	return probeAndFetchCheckpointRef(ctx, ref, fetchTarget, authoritative)
}

// FetchCheckpointRefFrom fetches a checkpoint ref from an ordered read chain.
// Candidates advance only after positive absence. A transport failure stops
// the chain because hydrating a legacy ref into the canonical local ref while
// the elected remote is unavailable could make stale data shadow the elected
// copy. A configured checkpoint_remote keeps the existing single-target
// behavior.
//
// The readFetchTimeout bounds the complete chain, not each candidate. When
// electionErr is non-nil, the fail-open origin candidate may still satisfy a
// read, but its emptiness cannot certify global absence.
func FetchCheckpointRefFrom(ctx context.Context, ref plumbing.ReferenceName, readRemotes []string, electionErr error) error {
	s, loadErr := settings.Load(ctx)
	if loadErr != nil || s.HasCheckpointRemoteKey() {
		return FetchCheckpointRef(ctx, ref)
	}

	chainCtx, cancel := context.WithTimeout(ctx, readFetchTimeout)
	defer cancel()

	if len(readRemotes) == 0 {
		if electionErr != nil {
			return fmt.Errorf("checkpoint ref %s: checkpoint remote election failed; no authoritative read candidate is available: %w", ref, electionErr)
		}
		if chainCtx.Err() == nil && !s.HasCheckpointRemoteKey() && repoHasNoRemotes(chainCtx) {
			logging.Debug(chainCtx, "checkpoint probe: repository has no git remotes; classifying ref as absent",
				slog.String("ref", ref.String()))
			return fmt.Errorf("checkpoint ref %s: repository has no git remotes to fetch from: %w", ref, plumbing.ErrReferenceNotFound)
		}
		return fmt.Errorf("checkpoint ref %s: no checkpoint read candidates available and the repository is not provably remoteless; refusing to treat this as absence", ref)
	}

	var firstErr error
	for i, remoteName := range readRemotes {
		target, authoritative := checkpointFetchTargetFrom(chainCtx, remoteName)
		var err error
		if !authoritative {
			err = fmt.Errorf("resolve checkpoint read candidate %q: resolved target %s is not authoritative", remoteName, RedactURLOrPath(target))
		} else {
			err = probeAndFetchCheckpointRef(chainCtx, ref, target, true)
		}
		if err == nil {
			return nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return err
		}
		if i+1 < len(readRemotes) {
			logging.Debug(ctx, "checkpoint ref fetch: read candidate failed; trying next candidate",
				slog.String("ref", ref.String()),
				slog.String("candidate", remoteName),
				slog.String("error", err.Error()))
		}
	}
	if electionErr != nil && errors.Is(firstErr, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("checkpoint ref %s: checkpoint remote election failed; origin fallback cannot certify absence: %w", ref, electionErr)
	}
	return firstErr
}

func probeAndFetchCheckpointRef(ctx context.Context, ref plumbing.ReferenceName, fetchTarget string, authoritative bool) error {
	out, err := LsRemoteInDir(ctx, "", fetchTarget, ref.String())
	if err != nil {
		// Redact: fetchTarget can be a remote URL with embedded credentials
		// (CI origin URLs), and this error is logged and shown to users.
		return fmt.Errorf("probe checkpoint ref %s on %s: %w", ref, RedactURLOrPath(fetchTarget), err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		if !authoritative {
			// The probe hit an origin FALLBACK while a checkpoint_remote is
			// configured (or undeterminable) — a remote that may simply never
			// host the configured checkpoint refs. Emptiness there proves
			// nothing; classifying it as absence would silently drop backfills
			// for checkpoints that exist on the real checkpoint remote.
			return fmt.Errorf("checkpoint ref %s not visible on fallback remote %s, and the configured checkpoint remote could not be resolved; refusing to treat this as absence", ref, RedactURLOrPath(fetchTarget))
		}
		return fmt.Errorf("checkpoint ref %s not found on %s: %w", ref, RedactURLOrPath(fetchTarget), plumbing.ErrReferenceNotFound)
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
		safeTarget := RedactURLOrPath(fetchTarget)
		msg = strings.ReplaceAll(msg, fetchTarget, safeTarget)
		if msg != "" {
			return fmt.Errorf("fetch checkpoint ref %s from %s: %s: %w", ref, safeTarget, msg, err)
		}
		return fmt.Errorf("fetch checkpoint ref %s from %s: %w", ref, safeTarget, err)
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
//
// This remains single-target because write-side probes must not broaden their
// destination beyond the remote selected by the push flow.
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
