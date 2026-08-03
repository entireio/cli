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
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointremote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
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

	// git-refs primary: push the per-checkpoint refs recorded in the push queue
	// instead of the single v1 branch. Those refs live under refs/entire/, not
	// refs/heads/, so a forge can never pick them as a repository's default
	// branch — the empty-remote guard below is unnecessary for this backend.
	// (A configured git-branch mirror's v1 ref is not pushed here yet — mirror
	// push for downgrade safety is a later step.)
	if cpCfg, _ := settings.LoadCheckpointsConfig(ctx); checkpoint.PrimaryIsRefs(cpCfg) { //nolint:errcheck // fail-soft: a bad checkpoints block already surfaces via Open; default to no refs push
		return s.prePushCheckpointRefs(ctx, ps)
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

	// Session-tree summary is progress, not an error/hint — skip the git-log
	// work entirely on non-TTY stderr (agents/CI) instead of computing it only
	// to have pushProgressOutput() discard it.
	if interactive.ShouldStyle(os.Stderr) {
		printPushSummary(ctx, remote, ps.pushTarget())
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
	return exec.CommandContext(ctx, "git", "remote", "get-url", name).Run() == nil
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
	// surface it via the threshold-gated reporter instead of leaving the user's
	// git push apparently hung. Styled-gated (interactive.ShouldStyle — respects
	// NO_COLOR and TERM=cygwin, not merely "stderr is a terminal") and
	// threshold-delayed: agents and CI (non-TTY stderr) see zero progress bytes.
	rep := newPushReporter(ctx, os.Stderr, interactive.ShouldStyle(os.Stderr), time.Second)
	rep.phase(fmt.Sprintf("syncing %d checkpoint(s)", len(existing)))

	// Stream git's own --progress output live into the reporter's detail line
	// (counting/compressing/writing) as the batch push runs, instead of only
	// showing it after the push completes.
	streamer := &gitProgressStreamer{
		onEvent: func(event *gitProgressEvent) {
			if detail := formatPushProgressDetail(event); detail != "" {
				rep.setDetail(detail)
			}
		},
	}

	// Fast path: push all refs in one round-trip (fast-forward-only). If every
	// ref was up to date or fast-forwarded, we're done.
	batchErr := batchPushRefs(pushCtx, pushTarget, existing, streamer)
	if batchErr == nil {
		rep.finish(fmt.Sprintf("pushed %d checkpoint(s)", len(existing)))
		if removeErr := queue.Remove(existing); removeErr != nil {
			logging.Warn(ctx, "git-refs push: clear pushed refs from queue failed",
				slog.String("error", removeErr.Error()))
		}
		return len(existing), nil
	}

	// Non-interactive SSH auth failures cannot be fixed by per-ref
	// fetch+replay. Surface the same actionable hint as the v1 doPushRef path
	// (issue #1523) instead of only logging to .entire/logs/.
	if nonInteractiveSSHAuthFailure(pushCtx, batchErr) {
		rep.finish("")
		fmt.Fprintf(os.Stderr, "[entire] Warning: couldn't push checkpoint refs: %v\n", batchErr)
		printNonInteractiveSSHAuthHint()
		printCheckpointRemoteHint(pushTarget)
		return 0, batchErr
	}

	// At least one ref was rejected — typically a non-fast-forward divergence
	// (the same checkpoint re-written on another machine). Retry per ref with
	// fetch+replay recovery, and remove from the queue only the refs that land
	// (a genuine cherry-pick conflict leaves that ref queued for a later push,
	// never force-overwriting the remote).
	rep.setDetail("") // clear the batch attempt's stale transfer detail
	pushed := make([]plumbing.ReferenceName, 0, len(existing))
	var firstErr error
	for i, ref := range existing {
		// Recovery is per-ref — fetch the remote's version of the checkpoint,
		// replay the local commit(s) on top, then push — so it runs one sync
		// cycle per ref and can take a while. Surface an advancing counter.
		rep.phase(fmt.Sprintf("syncing checkpoint %d/%d with remote", i+1, len(existing)))
		if err := pushCheckpointRefWithRecovery(pushCtx, pushTarget, ref); err != nil {
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
	rep.finish(fmt.Sprintf("pushed %d of %d checkpoint(s)", len(pushed), len(existing)))
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

// pushSummaryLogFormat is the `git log --format=` string runPushSummaryGitLog
// uses to gather commits for the session-tree summary; parsePushSummaryFromLog
// parses its output. Deliberately omits `valueonly` on the trailers
// placeholder: parsePushSummaryFromLog's sessionTrailerRE regex matches
// against the "Entire-Session: <id>" prefixed form, so stripping the key
// prefix here would bucket every commit under "unknown". Kept as a
// package-level const (baked into runPushSummaryGitLog rather than threaded
// as a parameter) so tests can exercise the exact production format against a
// real git log — see TestPrintPushSummaryLogFormat_TrailerGroupsUnderSessionID.
const pushSummaryLogFormat = "%H|%s|%(trailers:key=" + trailers.SessionTrailerKey + ",separator=%x20)|%aI"

// printPushSummary writes a session-level summary of pending checkpoint commits.
// Silent on any failure — never blocks the push.
func printPushSummary(ctx context.Context, remoteName, target string) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}

	branchName := paths.MetadataBranchName

	var logOutput string
	isNewBranch := false

	if !checkpointremote.IsURL(target) {
		rangeSpec := "refs/remotes/" + remoteName + "/" + branchName + "..refs/heads/" + branchName
		firstOut, firstErr := runPushSummaryGitLog(ctx, repoRoot, rangeSpec)
		if firstErr == nil && strings.TrimSpace(firstOut) != "" {
			logOutput = firstOut
		} else {
			fallbackOut, fallbackErr := runPushSummaryGitLog(ctx, repoRoot, "refs/heads/"+branchName)
			if fallbackErr == nil && strings.TrimSpace(fallbackOut) != "" {
				logOutput = fallbackOut
				isNewBranch = true
			}
		}
	} else {
		fallbackOut, fallbackErr := runPushSummaryGitLog(ctx, repoRoot, "refs/heads/"+branchName)
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
	w := pushProgressOutput()
	// Pass the same writer the tree is printed to so formatSessionTree styles
	// against the real target instead of io.Discard — otherwise ANSI styling
	// never applies even when w is a real, style-capable terminal.
	lines := formatSessionTree(summaries, formatSessionTreeOpts{
		TotalCommits: totalCommits,
		IsNewBranch:  isNewBranch,
		Writer:       w,
	})
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
}

func runPushSummaryGitLog(ctx context.Context, repoRoot, rangeSpec string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "log", "--format="+pushSummaryLogFormat, rangeSpec)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git log: %w", err)
	}
	return string(out), nil
}
