package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointremote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
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

	printPushSummary(ctx, remote, ps.pushTarget())

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

	if _, err := flushCheckpointRefsQueue(ctx, repo, ps.pushTarget()); err != nil {
		// Fail-soft: a checkpoint-ref push failure must never block the user's
		// git push. The refs stay queued for the next pre-push.
		logging.Warn(ctx, "git-refs pre-push: checkpoint ref push failed; refs left queued",
			slog.String("error", err.Error()))
	}

	cleanupPushedShadowBranches(ctx)
	return nil
}

// PushQueuedCheckpointRefs pushes any queued checkpoint refs to the configured
// checkpoint remote, surfacing errors (unlike the fail-soft pre-push path); the
// caller owns the repo. It returns the number of refs pushed and whether
// pushing is disabled in settings — a distinct signal from pushed==0 with
// pushing enabled (an empty queue), so callers can report the two accurately.
// Like the pre-push paths, a checkpoint policy that blocks pushing errors with
// the refs left queued. Currently used by the checkpoint migration command's
// opt-in "push now".
func PushQueuedCheckpointRefs(ctx context.Context, repo *git.Repository, remote string) (pushed int, pushDisabled bool, err error) {
	ps := resolvePushSettings(ctx, remote)
	if ps.pushDisabled {
		return 0, true, nil
	}
	syncCheckpointPolicyForPrePush(ctx, repo, ps)
	if !checkpointPolicyAllowsGitHook(ctx, repo) {
		return 0, false, errors.New("checkpoint policy does not allow pushing checkpoint refs; refs stay queued")
	}
	pushed, err = flushCheckpointRefsQueue(ctx, repo, ps.pushTarget())
	// Clean up even on a partial/failed flush: a diverged batch can push some
	// refs and still return an error, and the shadow branches for the refs that
	// *did* land must still be cleaned up — parity with the pre-push path, which
	// always runs cleanup after flush regardless of its error.
	cleanupPushedShadowBranches(ctx)
	return pushed, false, err
}

// flushCheckpointRefsQueue drains the push-discovery queue and batch-pushes the
// recorded refs fast-forward-only, recovering a diverged ref by fetch+replay and
// removing from the queue only the refs that land. It returns the number pushed.
//
// Shared by the git-refs pre-push path (which logs and ignores the error to
// never block the user's push) and the migration command's opt-in push (which
// surfaces it). Stale entries — refs no longer present locally — are pruned so
// they don't block the queue forever.
func flushCheckpointRefsQueue(ctx context.Context, repo *git.Repository, pushTarget string) (int, error) {
	queue, err := checkpoint.PushQueueForRepo(ctx, repo)
	if err != nil {
		return 0, fmt.Errorf("resolve push queue: %w", err)
	}
	queued, err := queue.Drain()
	if err != nil {
		return 0, fmt.Errorf("drain push queue: %w", err)
	}
	if len(queued) == 0 {
		return 0, nil
	}

	pushCtx, pushSpan := perf.Start(ctx, "push_checkpoint_refs")
	defer pushSpan.End()

	existing, stale := partitionLocalRefs(repo, queued)
	if len(stale) > 0 {
		if err := queue.Remove(stale); err != nil {
			logging.Warn(ctx, "git-refs push: prune stale queue entries failed",
				slog.String("error", err.Error()))
		}
	}
	if len(existing) == 0 {
		return 0, nil
	}

	// Progress: pushing many refs over the network can take tens of seconds, so
	// surface it (matching the v1 path's "[entire] Pushing ..." line) instead of
	// leaving the user's git push apparently hung. Written to stderr, which git
	// shows during the pre-push hook.
	displayTarget := displayPushTarget(pushTarget)
	fmt.Fprintf(os.Stderr, "[entire] Pushing %d checkpoint ref(s) to %s...", len(existing), displayTarget)
	stop := startProgressDots(os.Stderr)

	// Fast path: push all refs in one round-trip (fast-forward-only). If every
	// ref was up to date or fast-forwarded, we're done.
	if err := batchPushRefs(pushCtx, pushTarget, existing); err == nil {
		stop(" done")
		if removeErr := queue.Remove(existing); removeErr != nil {
			logging.Warn(ctx, "git-refs push: clear pushed refs from queue failed",
				slog.String("error", removeErr.Error()))
		}
		return len(existing), nil
	}
	stop("")

	// At least one ref was rejected — typically a non-fast-forward divergence
	// (the same checkpoint re-written on another machine). Retry per ref with
	// fetch+replay recovery, and remove from the queue only the refs that land
	// (a genuine cherry-pick conflict leaves that ref queued for a later push,
	// never force-overwriting the remote).
	fmt.Fprintf(os.Stderr, "[entire] Some checkpoint refs diverged; syncing %d ref(s) individually...", len(existing))
	stop = startProgressDots(os.Stderr)
	pushed := make([]plumbing.ReferenceName, 0, len(existing))
	var firstErr error
	for _, ref := range existing {
		if err := pushCheckpointRefWithRecovery(pushCtx, pushTarget, ref); err != nil {
			logging.Warn(ctx, "git-refs push: checkpoint ref push/sync failed; left queued, not overwritten",
				slog.String("ref", ref.String()), slog.String("error", err.Error()))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		pushed = append(pushed, ref)
	}
	stop(fmt.Sprintf(" pushed %d of %d", len(pushed), len(existing)))
	if err := queue.Remove(pushed); err != nil {
		logging.Warn(ctx, "git-refs push: clear pushed refs from queue failed",
			slog.String("error", err.Error()))
	}
	if firstErr != nil {
		return len(pushed), fmt.Errorf("%d of %d checkpoint refs failed to push: %w",
			len(existing)-len(pushed), len(existing), firstErr)
	}
	return len(pushed), nil
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

// printPushSummary writes a session-level summary of pending checkpoint commits.
// Silent on any failure — never blocks the push.
func printPushSummary(ctx context.Context, remoteName, target string) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}

	branchName := paths.MetadataBranchName
	logFormat := "%H|%s|%(trailers:key=" + trailers.SessionTrailerKey + ",valueonly,separator=%x20)|%aI"

	var logOutput string
	isNewBranch := false

	if !checkpointremote.IsURL(target) {
		rangeSpec := "refs/remotes/" + remoteName + "/" + branchName + "..refs/heads/" + branchName
		firstOut, firstErr := runPushSummaryGitLog(ctx, repoRoot, logFormat, rangeSpec)
		if firstErr == nil && strings.TrimSpace(firstOut) != "" {
			logOutput = firstOut
		} else {
			fallbackOut, fallbackErr := runPushSummaryGitLog(ctx, repoRoot, logFormat, "refs/heads/"+branchName)
			if fallbackErr == nil && strings.TrimSpace(fallbackOut) != "" {
				logOutput = fallbackOut
				isNewBranch = true
			}
		}
	} else {
		fallbackOut, fallbackErr := runPushSummaryGitLog(ctx, repoRoot, logFormat, "refs/heads/"+branchName)
		if fallbackErr == nil {
			logOutput = fallbackOut
			isNewBranch = strings.TrimSpace(logOutput) != ""
		}
	}
	if strings.TrimSpace(logOutput) == "" {
		return
	}

	summaries := parsePushSummaryFromLog(logOutput)
	if len(summaries) == 0 {
		return
	}

	totalCommits := len(strings.Split(strings.TrimSpace(logOutput), "\n"))
	lines := formatSessionTree(summaries, formatSessionTreeOpts{
		TotalCommits: totalCommits,
		IsNewBranch:  isNewBranch,
	})
	w := pushProgressOutput()
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
}

func runPushSummaryGitLog(ctx context.Context, repoRoot, format, rangeSpec string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "log", "--format="+format, rangeSpec)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git log: %w", err)
	}
	return string(out), nil
}
