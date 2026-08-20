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
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/perf"
	"github.com/entireio/cli/redact"
)

// errOPFAbortedByUser is returned when the user chose Abort (or pressed
// Ctrl-C) at the OPF prompt. PrePush returns it verbatim; the hook
// command propagates the non-zero exit code so git push aborts.
var errOPFAbortedByUser = errors.New("OPF prompt aborted by user; push cancelled")

var opfPrePushProgressWriter io.Writer = os.Stderr

// resolveTrustDecisionFn is a test seam over the trust prompt (same pattern
// as stderrWriter).
var resolveTrustDecisionFn = resolveTrustDecisionForPrePush

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
	return s.prePush(ctx, remote, false)
}

// PrePushFromGitHook handles a push initiated by Git's pre-push hook. Unlike
// direct callers, it protects an empty user remote from receiving checkpoint
// metadata before the user's first normal branch is published.
func (s *ManualCommitStrategy) PrePushFromGitHook(ctx context.Context, remote string) error {
	return s.prePush(ctx, remote, true)
}

func (s *ManualCommitStrategy) prePush(ctx context.Context, remote string, protectFirstUserBranch bool) error {
	// This runs inside the user's `git push` pre-push hook. Every checkpoint
	// git subprocess spawned here (metadata fetch, policy sync, checkpoint
	// push and its recovery fetch) must fail fast rather than block on an
	// interactive SSH passphrase prompt — there is no way to answer it here and
	// it would hang the user's push. Foreground commands do not set this.
	//
	// BatchMode=yes suppresses passphrase/PIN prompts (including FIDO2
	// verify-required PIN entry). Touch-only security keys still work because
	// user-presence touch is not a terminal read. Users who need a PIN prompt
	// in this path should load the key into ssh-agent, or set an explicit
	// BatchMode=no via GIT_SSH_COMMAND / core.sshCommand (respected by the
	// non-interactive SSH helper).
	ctx = checkpointremote.WithNonInteractiveSSH(ctx)

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

	// Single-remote gate (ENT-1451): checkpoint data syncs only to the
	// elected checkpoint sync remote. A dedicated checkpoint_remote URL is
	// exempt — it is a dedicated metadata store addressed directly, not a
	// remote selected by this push. The gate must stay BELOW
	// resolvePushSettings: hasCheckpointURL is only known after resolution,
	// so hoisting the gate above it would break the exemption.
	//
	// Capture is two-phase, straddling this whole function. The proposal runs
	// before the gate so that the push which elects a remote by evidence (push
	// target agrees with the branch's declared push destination) is also the
	// first push to carry checkpoints there; the election is persisted only at
	// the delivery points below, once those checkpoints actually arrived.
	// Everything in between can still stop delivery, and the election is
	// permanent, so intent is not enough to move it. The hint below then speaks
	// only for what capture left gated — see hintGatedCheckpointSync.
	var pendingCapture string
	if !ps.hasCheckpointURL() {
		if pendingCaptureCheckpointSyncRemote(ctx, ps.remote) {
			pendingCapture = ps.remote
		}
		if !checkpointSyncAllowedForRemote(ctx, ps.remote, pendingCapture) {
			hintGatedCheckpointSync(ctx, ps.remote)
			return nil
		}
	}

	// Egress trust gate, above both backend branches: a globally enrolled repo
	// syncs only after the user trusts it once. A hold pairs with exactly one
	// stderr explanation and never fails the user's own push.
	if !settings.CheckpointEgressAllowed(ctx) {
		decision, decisionErr := resolveTrustDecisionFn(ctx, stderrWriter)
		if decisionErr != nil {
			logging.Warn(ctx, "trust pre-push prompt failed; holding checkpoint sync",
				slog.String("error", decisionErr.Error()),
			)
		}
		if decisionErr != nil || decision != TrustGranted {
			fmt.Fprintln(stderrWriter, "Entire: checkpoint sync held — this repo isn't trusted yet.\nSessions are captured locally. Run `entire trust` to sync them to your checkpoint sync remote.")
			return nil
		}
		// Granted at the prompt: re-evaluate the gate, never reuse a boolean.
		if !settings.CheckpointEgressAllowed(ctx) {
			fmt.Fprintln(stderrWriter, "Warning: trust was saved but the gate still holds; checkpoint sync skipped for this push.")
			return nil
		}
	}

	// git-refs primary: push the per-checkpoint refs recorded in the push queue
	// instead of the single v1 branch. Those refs live under refs/entire/, not
	// refs/heads/, so a forge can never pick them as a repository's default
	// branch — the empty-remote guard below is unnecessary for this backend.
	// (A configured git-branch mirror's v1 ref is not pushed here yet — mirror
	// push for downgrade safety is a later step.)
	if ps.primaryIsRefs {
		return s.prePushCheckpointRefs(ctx, ps, pendingCapture)
	}

	// git-branch primary: entire/checkpoints/v1 is a real refs/heads branch, so
	// on an otherwise-empty remote a forge like GitHub would select it as the
	// default. Defer publication until the user's own branch exists there.
	deferAutomaticCheckpointPush := protectFirstUserBranch && deferCheckpointPushOnEmptyRemote(ctx, ps)

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
	// re-redact unpushed v1 commits with OPF (producing the OPF-applied,
	// 9-layer pipeline) before pushing. Skipped entirely when OPF is off,
	// so the common-case fast path is unchanged.
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
			// "never"). Push regex-only (8-layer) content as-is.
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

	if deferAutomaticCheckpointPush {
		// Do this only after OPF has had a chance to rewrite v1: the outer
		// user push may explicitly include the metadata branch.
		logging.Info(ctx, "automatic checkpoint push deferred until the remote has a branch",
			slog.String("remote", ps.remote),
		)
		return nil
	}

	// Thread the span's context into the push so the network push and any
	// fetch+rebase recovery nest beneath it as child steps in the perf trace.
	pushCtx, pushCheckpointsSpan := perf.Start(ctx, "push_checkpoint_refs")
	// Capture needs checkpoint data CONFIRMED on this remote, so count what
	// landed rather than trusting the absence of an error: every ref that was due
	// has to deliver, and at least one has to actually do so. Delivery must come
	// from pushRefIfNeeded's delivered return and NOT from err, which is
	// fail-soft and nil even when the remote refused the ref.
	deliveredCount, anyFailed := 0, false
	for _, ref := range refs.Push {
		delivered, err := pushRefIfNeeded(pushCtx, ps.pushTarget(), ref)
		if err != nil {
			pushCheckpointsSpan.RecordError(err)
			pushCheckpointsSpan.End()
			return err
		}
		if delivered {
			deliveredCount++
		} else {
			anyFailed = true
		}
	}
	pushCheckpointsSpan.End()

	// Delivered: the election may now follow the push that carried it. A push that
	// carried nothing — an empty ref set, or a v1 ref that does not exist locally
	// yet — leaves the election alone. It is safe either way (nothing is stranded
	// when there was nothing to strand), but capturing would announce a move that
	// moved no data, which is the class of claim this whole path exists to stop
	// making. The next push that carries a checkpoint captures instead.
	if pendingCapture != "" && deliveredCount > 0 && !anyFailed {
		commitCapturedSyncRemote(ctx, pendingCapture)
	}

	cleanupPushedShadowBranches(ctx)
	return nil
}

// deferCheckpointPushOnEmptyRemote reports whether publication of the git-branch
// v1 metadata should be held back because the push remote may be brand new.
//
// Hosting providers such as GitHub make the first branch pushed to an empty
// repository its default, so the pre-push hook must not publish
// entire/checkpoints/v1 ahead of the user's own first branch. The check is
// purely local: if a remote-tracking ref for this remote already exists
// (refs/remotes/<remote>/*), the remote has been fetched from or pushed to
// before and therefore already has at least one branch, so publishing cannot
// make our metadata the default. Otherwise defer — git records a
// remote-tracking ref after the first successful push, so the deferred metadata
// publishes on the next push.
//
// It deliberately performs no ls-remote/fetch. A network round trip on the
// pre-push path can trigger an SSH security-key touch prompt (and doing so per
// push URL would multiply those prompts), which is a poor pre-push UX. This is
// also why it uses only the remote git handed the hook rather than resolving
// every configured push URL.
//
// A separate checkpoint remote is exempt: it is a dedicated metadata store, not
// the repository the user pushes to.
func deferCheckpointPushOnEmptyRemote(ctx context.Context, ps pushSettings) bool {
	if ps.hasCheckpointURL() {
		return false
	}

	// The hazard only arises for a configured remote (the `git remote add
	// origin …` then first-push flow). Pushing straight to a bare URL hands that
	// URL to the hook as the remote arg, and git never records a
	// refs/remotes/<url>/* tracking ref for it — so a tracking-ref check would
	// defer the metadata forever. Publish for a non-configured (URL) target
	// rather than strand it; the first-branch scenario always uses a named
	// remote.
	if !isConfiguredRemote(ctx, ps.remote) {
		return false
	}

	// Known limitation, accepted for the no-network design: a tracking ref left
	// over from before a remote was deleted and recreated empty under the same
	// URL reads as "established", so v1 would publish to the now-empty remote.
	// Detecting that requires asking the remote — the network round trip we
	// deliberately avoid here. The scenario is rare and its default branch is
	// recoverable by resetting it on the forge.
	return !remoteHasTrackingRefs(ctx, ps.remote)
}

// isConfiguredRemote reports whether name is a configured git remote, as
// opposed to a bare URL that git passes through verbatim when a push targets a
// URL directly. Local and best-effort (reads config, no network); any error is
// treated as "not a configured remote".
func isConfiguredRemote(ctx context.Context, name string) bool {
	if name == "" {
		return false
	}
	return cachedIsConfiguredRemote(ctx, name, func() (bool, error) {
		cmd := exec.CommandContext(ctx, "git", "remote", "get-url", name)
		if worktreeRoot, ok := settings.WorktreeRoot(ctx); ok {
			cmd.Dir = worktreeRoot
		}
		err := cmd.Run()
		if err == nil {
			return true, nil
		}
		// git ran and said no: a real answer worth caching. git failing to run at
		// all says nothing about the remote, and memoizing that false would
		// fail-close checkpoint_push_remote for the rest of the process.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("probe remote %q: %w", name, err)
	})
}

// remoteHasTrackingRefs reports whether any refs/remotes/<remote>/* ref exists
// locally. Its presence means the remote has been fetched from or pushed to
// before and so already has at least one branch. Local-only and best-effort:
// any error is treated as "no tracking refs" so the caller fails safe (defers).
func remoteHasTrackingRefs(ctx context.Context, remote string) bool {
	if remote == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "for-each-ref", "--count=1", "refs/remotes/"+remote+"/")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
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
func (s *ManualCommitStrategy) prePushCheckpointRefs(ctx context.Context, ps pushSettings, pendingCapture string) error {
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

	if flushed, err := flushCheckpointRefsQueue(ctx, repo, ps); err == nil {
		// Delivered, and only if something actually was: an empty queue pushed
		// nothing, so it must not move the election or announce that it had.
		if pendingCapture != "" && flushed > 0 {
			commitCapturedSyncRemote(ctx, pendingCapture)
		}
	} else {
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
	// Second egress entry point (bypasses prePush): same trust gate, no
	// prompt; refs stay queued for after `entire trust`.
	if !settings.CheckpointEgressAllowed(ctx) {
		return 0, false, errors.New("checkpoint sync is held — this repo isn't trusted yet; refs stay queued — run `entire trust` first")
	}
	syncCheckpointPolicyForPrePush(ctx, repo, ps)
	if !checkpointPolicyAllowsGitHook(ctx, repo) {
		return 0, false, errors.New("checkpoint policy does not allow pushing checkpoint refs; refs stay queued")
	}
	pushed, err = flushCheckpointRefsQueue(ctx, repo, ps)
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
func flushCheckpointRefsQueue(ctx context.Context, repo *git.Repository, ps pushSettings) (int, error) {
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

	// Resolved here, not by the caller: it spawns `git remote get-url` and its
	// result is unused unless refs are actually pushed, so an ordinary push with
	// an empty queue must not pay for it — nor print the multi-URL warning.
	dest := resolveRefsPushDestination(pushCtx, ps)
	dest.warnIgnoredPushURLs(pushCtx)

	// Progress: pushing many refs over the network can take tens of seconds, so
	// surface it (matching the v1 path's "[entire] Pushing ..." line) instead of
	// leaving the user's git push apparently hung. Written to stderr, which git
	// shows during the pre-push hook.
	fmt.Fprintf(os.Stderr, "[entire] Pushing %d checkpoint ref(s) to %s...", len(existing), dest.display())
	stop := startProgressDots(os.Stderr)

	// Fast path: push all refs in one round-trip (fast-forward-only). If every
	// ref was up to date or fast-forwarded, we're done.
	batchErr := batchPushRefs(pushCtx, dest.target, existing)
	if batchErr == nil {
		stop(" done")
		if removeErr := queue.Remove(existing); removeErr != nil {
			logging.Warn(ctx, "git-refs push: clear pushed refs from queue failed",
				slog.String("error", removeErr.Error()))
		}
		return len(existing), nil
	}
	stop("")

	// Non-interactive SSH auth failures cannot be fixed by per-ref
	// fetch+replay. Surface the same actionable hint as the v1 doPushRef path
	// (issue #1523) instead of only logging to .entire/logs/.
	if nonInteractiveSSHAuthFailure(pushCtx, batchErr) {
		fmt.Fprintf(os.Stderr, "[entire] Warning: couldn't push checkpoint refs: %v\n", batchErr)
		printNonInteractiveSSHAuthHint()
		if dest.checkpointRemote {
			printCheckpointRemoteHint(dest.target)
		}
		return 0, batchErr
	}

	// At least one ref was rejected — typically a non-fast-forward divergence
	// (the same checkpoint re-written on another machine). Retry per ref with
	// fetch+replay recovery, and remove from the queue only the refs that land
	// (a genuine cherry-pick conflict leaves that ref queued for a later push,
	// never force-overwriting the remote).
	// Deliberately names no cause: the batch fails on divergence, but just as
	// often on an unreachable or unauthorized destination. Telling a user with a
	// dead remote that their refs "diverged" — or were "rejected", which equally
	// implies the remote answered — sends them after the wrong problem.
	fmt.Fprintf(os.Stderr, "[entire] Checkpoint ref push failed; retrying %d ref(s) individually...", len(existing))
	stop = startProgressDots(os.Stderr)
	pushed := make([]plumbing.ReferenceName, 0, len(existing))
	var firstErr error
	for _, ref := range existing {
		if err := pushCheckpointRefWithRecovery(pushCtx, dest.target, ref); err != nil {
			logging.Warn(ctx, "git-refs push: checkpoint ref push/sync failed; left queued, not overwritten",
				slog.String("ref", ref.String()), slog.String("error", err.Error()))
			if nonInteractiveSSHAuthFailure(pushCtx, err) {
				printNonInteractiveSSHAuthHint()
			}
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
