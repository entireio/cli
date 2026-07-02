package strategy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/perf"
	"github.com/entireio/cli/redact"
)

// errOPFAbortedByUser is returned when the user chose Abort (or pressed
// Ctrl-C) at the OPF prompt. PrePush returns it verbatim; the hook
// command propagates the non-zero exit code so git push aborts.
var errOPFAbortedByUser = errors.New("OPF prompt aborted by user; push cancelled")

var opfPrePushProgressWriter io.Writer = os.Stderr

// PrePush is called by the git pre-push hook before pushing to a remote.
// It pushes each ref in refs.Push alongside the user's push.
//
// If a checkpoint_remote is configured in settings, checkpoint branches/refs
// are pushed to the derived URL instead of the user's push remote.
//
// Configuration options (stored in .entire/settings.json under strategy_options):
//   - push_sessions: false to disable automatic pushing of checkpoints
//   - checkpoint_remote: {"provider": "github", "repo": "org/repo"} to push to a separate repo
func (s *ManualCommitStrategy) PrePush(ctx context.Context, remote string) error {
	// Load settings once for remote resolution and push_sessions check.
	// Spanned because checkpoint-remote resolution can perform a one-time
	// network fetch of the metadata branch (fetchMetadataBranchIfMissing),
	// which is otherwise invisible in the pre-push trace.
	resolveCtx, resolveSpan := perf.Start(ctx, "resolve_push_settings")
	ps := resolvePushSettings(resolveCtx, remote)
	resolveSpan.End()

	if ps.pushDisabled {
		return nil
	}

	// git-refs primary: push the per-checkpoint refs recorded in the push queue
	// instead of the single v1 branch. (A configured git-branch mirror's v1 ref
	// is not pushed here yet — mirror push for downgrade safety is a later step.)
	if cpCfg, _ := settings.LoadCheckpointsConfig(ctx); checkpoint.PrimaryIsRefs(cpCfg) { //nolint:errcheck // fail-soft: a bad checkpoints block already surfaces via Open; default to no refs push
		return s.prePushCheckpointRefs(ctx, ps)
	}

	refs := checkpoint.ResolveRefs(ctx)
	repo, repoErr := OpenRepository(ctx)
	if repoErr != nil {
		logging.Warn(ctx, "checkpoint policy pre-push: failed to open repository; allowing checkpoint push",
			slog.String("error", repoErr.Error()),
		)
	} else {
		defer repo.Close()
		syncCheckpointPolicyForPrePush(ctx, repo, ps)
		if !checkpointPolicyAllowsGitHook(ctx, repo) {
			// Policy failures should skip checkpoint pushes, not abort the user's push.
			return nil
		}
	}

	// OPF pre-push rewrite: if OPF is configured, resolve the user's
	// decision (env > settings > prompt > non-TTY auto-run), then
	// re-redact unpushed v1 commits with the 8-layer pipeline before
	// pushing. Skipped entirely when OPF is off, so the common-case
	// fast path is unchanged.
	if redact.OPFEnabled() {
		cfg, _ := settings.Load(ctx) //nolint:errcheck // Load already failed at hook init; fall back to nil
		var opfCfg *settings.OPFSettings
		if cfg != nil && cfg.Redaction != nil {
			opfCfg = cfg.Redaction.OpenAIPrivacyFilter
		}
		decision, decisionErr := resolveOPFDecisionForPrePush(ctx, opfCfg, opfPrePushProgressWriter)
		if decisionErr != nil {
			logging.Warn(ctx, "OPF pre-push decision failed; aborting push",
				slog.String("error", decisionErr.Error()),
			)
			return decisionErr
		}
		switch decision {
		case OPFAbort:
			return errOPFAbortedByUser
		case OPFSkip:
			// User opted out for this push (or settings/env say
			// "never"). Push 7-layer content as-is.
			logging.Info(ctx, "OPF skipped for this push (user choice or settings)")
		case OPFRun:
			_, opfSpan := perf.Start(ctx, "opf_pre_push_rewrite")
			if repoErr != nil {
				opfSpan.RecordError(repoErr)
				opfSpan.End()
				logging.Warn(ctx, "OPF pre-push: failed to open repo; aborting push",
					slog.String("error", repoErr.Error()),
				)
				return repoErr
			}
			if _, rewriteErr := RewriteUnpushedV1WithOPF(ctx, repo, ps.pushTarget()); rewriteErr != nil {
				opfSpan.RecordError(rewriteErr)
				opfSpan.End()
				logging.Warn(ctx, "OPF pre-push rewrite failed; aborting push",
					slog.String("error", rewriteErr.Error()),
				)
				return rewriteErr
			}
			opfSpan.End()
		}
	}

	// Thread the span's context into the push so the network push and any
	// fetch+rebase recovery nest beneath it as child steps in the perf trace.
	pushCtx, pushCheckpointsSpan := perf.Start(ctx, "push_checkpoint_refs")
	for _, ref := range refs.Push {
		if err := pushRefIfNeeded(pushCtx, ps.pushTarget(), ref); err != nil {
			pushCheckpointsSpan.RecordError(err)
			pushCheckpointsSpan.End()
			return err
		}
	}
	pushCheckpointsSpan.End()

	cleanupPushedShadowBranches(ctx)
	return nil
}

// prePushCheckpointRefs drains the per-checkpoint push queue and batch-pushes the
// recorded refs fast-forward-only (git-refs primary; never a force push — a
// diverged ref is recovered via fetch+replay). Transient push failures are logged and
// swallowed — like the v1 path, they must not block the user's git push — and the
// refs stay queued for the next pre-push. OPF is not applied (it is descoped for
// the git-refs store for now).
//
// It honors the checkpoint policy exactly like the v1 path: the policy gates on
// checkpoint *format* compatibility (diverged from the remote, or an unsupported
// local format), which is independent of the storage backend, so a blocked
// policy skips the ref push (leaving refs queued) rather than pushing.
func (s *ManualCommitStrategy) prePushCheckpointRefs(ctx context.Context, ps pushSettings) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Warn(ctx, "git-refs pre-push: open repo failed; skipping checkpoint push",
			slog.String("error", err.Error()))
		return nil
	}
	defer repo.Close()

	// Refresh the checkpoint policy from the remote, then skip the ref push
	// (leaving refs queued) if the policy is diverged or the local format is
	// unsupported — same gate the v1 path uses.
	syncCheckpointPolicyForPrePush(ctx, repo, ps)
	if !checkpointPolicyAllowsGitHook(ctx, repo) {
		return nil
	}

	queue, err := checkpoint.PushQueueForRepo(ctx, repo)
	if err != nil {
		logging.Warn(ctx, "git-refs pre-push: resolve push queue failed; skipping checkpoint push",
			slog.String("error", err.Error()))
		return nil
	}
	queued, err := queue.Drain()
	if err != nil {
		logging.Warn(ctx, "git-refs pre-push: drain push queue failed; skipping checkpoint push",
			slog.String("error", err.Error()))
		return nil
	}
	if len(queued) == 0 {
		return nil
	}

	// Drop stale entries (refs no longer present locally) so they don't block
	// the queue forever, then push what remains.
	existing, stale := partitionLocalRefs(repo, queued)
	if len(stale) > 0 {
		if err := queue.Remove(stale); err != nil {
			logging.Warn(ctx, "git-refs pre-push: prune stale queue entries failed",
				slog.String("error", err.Error()))
		}
	}
	if len(existing) == 0 {
		return nil
	}

	pushCtx, pushSpan := perf.Start(ctx, "push_checkpoint_refs")
	defer pushSpan.End()

	// Fast path: push all refs in one round-trip (fast-forward-only). If every
	// ref was up to date or fast-forwarded, we're done.
	if err := batchPushRefs(pushCtx, ps.pushTarget(), existing); err == nil {
		if removeErr := queue.Remove(existing); removeErr != nil {
			logging.Warn(ctx, "git-refs pre-push: clear pushed refs from queue failed",
				slog.String("error", removeErr.Error()))
		}
		cleanupPushedShadowBranches(ctx)
		return nil
	}

	// At least one ref was rejected — typically a non-fast-forward divergence
	// (the same checkpoint re-written on another machine). Retry per ref with
	// fetch+replay recovery, and remove from the queue only the refs that land
	// (a genuine cherry-pick conflict leaves that ref queued for a later push,
	// never force-overwriting the remote).
	pushed := make([]plumbing.ReferenceName, 0, len(existing))
	for _, ref := range existing {
		if err := pushCheckpointRefWithRecovery(pushCtx, ps.pushTarget(), ref); err != nil {
			logging.Warn(ctx, "git-refs pre-push: checkpoint ref push/sync failed; left queued, not overwritten",
				slog.String("ref", ref.String()), slog.String("error", err.Error()))
			continue
		}
		pushed = append(pushed, ref)
	}
	if err := queue.Remove(pushed); err != nil {
		logging.Warn(ctx, "git-refs pre-push: clear pushed refs from queue failed",
			slog.String("error", err.Error()))
	}

	cleanupPushedShadowBranches(ctx)
	return nil
}

// cleanupPushedShadowBranches runs post-push shadow-branch cleanup. Failures are
// non-fatal — shadow branches just accumulate until `entire clean` or the next
// successful push.
func cleanupPushedShadowBranches(ctx context.Context) {
	if deleted, cleanupErr := CleanupPushedShadowBranches(ctx); cleanupErr != nil {
		logging.Warn(ctx, "post-push shadow branch cleanup failed",
			slog.String("error", cleanupErr.Error()),
		)
	} else if deleted > 0 {
		logging.Info(ctx, "cleaned up vestigial shadow branches",
			slog.Int("count", deleted),
		)
	}
}
