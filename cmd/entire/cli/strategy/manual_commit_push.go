package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
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

// ErrOPFAbortedByUser is returned when the user chose Abort (or pressed
// Ctrl-C) at the OPF prompt. PrePush returns it verbatim; the hook
// command propagates the non-zero exit code so git push aborts.
//
// Exported because a user-initiated abort is a decline, not a failure, and
// only the calling command knows how to say so: `doctor migrate-checkpoints`
// matches it to report the same clean "refs stay queued" its own declined
// prompt does, rather than an error.
var ErrOPFAbortedByUser = errors.New("OPF prompt aborted by user; push cancelled")

// ErrCheckpointPushHeldByPolicy is returned by PushQueuedCheckpointRefs when
// the clone's checkpoint push policy is manual: the refs stay queued for
// `entire checkpoint push`, which is not the same as an empty queue.
var ErrCheckpointPushHeldByPolicy = errors.New("checkpoint push policy is manual; refs stay queued")

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

	// Before push_sessions and the sync-remote gate: those decide what Entire
	// sends, while this is about what the user's own push sends, to any remote.
	if err := CheckOuterPushHeldCheckpoints(ctx); err != nil {
		return err
	}

	// Load settings once for remote resolution and push_sessions check.
	// Spanned because checkpoint-remote resolution can perform a one-time
	// network fetch of the metadata branch (fetchMetadataBranchIfMissing),
	// which is otherwise invisible in the pre-push trace.
	resolveCtx, resolveSpan := perf.Start(ctx, "resolve_push_settings")
	ps := resolvePushSettings(resolveCtx, remote)
	resolveSpan.End()

	if skipsCheckpointPush(ps) {
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

	// git-refs primary: push the per-checkpoint refs recorded in the push queue
	// instead of the single v1 branch. Those refs live under refs/entire/, not
	// refs/heads/, so a forge can never pick them as a repository's default
	// branch — the empty-remote guard below is unnecessary for this backend.
	// (A configured git-branch mirror's v1 ref is not pushed here yet — mirror
	// push for downgrade safety is a later step.)
	if ps.primaryIsRefs {
		return s.prePushCheckpointRefs(ctx, ps, pendingCapture, protectFirstUserBranch)
	}

	// git-branch primary: entire/checkpoints/v1 is a real refs/heads branch, so
	// on an otherwise-empty remote a forge like GitHub would select it as the
	// default. Defer publication until the user's own branch exists there.
	deferAutomaticCheckpointPush := protectFirstUserBranch && deferCheckpointPushOnEmptyRemote(ctx, ps)

	// OPF pre-push rewrite: if OPF is configured, resolve the user's
	// decision (env > settings > prompt > non-TTY auto-run), then
	// re-redact unpushed v1 commits with OPF (producing the OPF-applied,
	// 9-layer pipeline) before pushing. Skipped entirely when OPF is off,
	// so the common-case fast path is unchanged.
	if redact.OPFEnabled() {
		decision, decisionErr := opfPrePushDecision(ctx)
		if decisionErr != nil {
			logging.Warn(ctx, "OPF pre-push decision failed; aborting push",
				slog.String("error", decisionErr.Error()),
			)
			return decisionErr
		}
		switch decision {
		case OPFAbort:
			return ErrOPFAbortedByUser
		case OPFSkip:
			// User opted out for this push (or settings/env say
			// "never"). Push regex-only (8-layer) content as-is.
			logging.Info(ctx, "OPF skipped for this push (user choice or settings)")
		case OPFRun:
			// The open is its own span: opf_pre_push_rewrite names the rewrite
			// and nothing else, so its timings stay comparable with every trace
			// recorded while the repository was opened further up this function.
			// The open is not free — on a reftable repo gitrepo routes reference
			// reads back through the git CLI.
			_, openSpan := perf.Start(ctx, "open_repository")
			repo, repoErr := OpenRepository(ctx)
			if repoErr != nil {
				openSpan.RecordError(repoErr)
				openSpan.End()
				logging.Warn(ctx, "OPF pre-push: failed to open repo; aborting push",
					slog.String("error", repoErr.Error()),
				)
				return repoErr
			}
			openSpan.End()
			defer repo.Close()
			_, opfSpan := perf.Start(ctx, "opf_pre_push_rewrite")
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

	pushed, err := pushCheckpointBranchRefs(ctx, ps)
	if err != nil {
		return err
	}
	deliveredCount := len(pushed.delivered)

	// Delivered: the election may now follow the push that carried it. A push that
	// carried nothing — an empty ref set, or a v1 ref that does not exist locally
	// yet — leaves the election alone. It is safe either way (nothing is stranded
	// when there was nothing to strand), but capturing would announce a move that
	// moved no data, which is the class of claim this whole path exists to stop
	// making. The next push that carries a checkpoint captures instead.
	if pendingCapture != "" && deliveredCount > 0 && !pushed.anyFailed {
		commitCapturedSyncRemote(ctx, pendingCapture)
	}
	// After capture, so the election this compares against is final. A dedicated
	// checkpoint_remote URL is not a remote name and has no tracking refs.
	if !ps.hasCheckpointURL() {
		for _, ref := range pushed.delivered {
			advanceSyncRemoteTrackingRef(ctx, ps.remote, ref)
		}
	}
	// Only a push that carried checkpoints can say "they are going somewhere
	// other than where you said"; the gate and every early return above carry
	// none, and hintGatedCheckpointSync speaks for the gated case.
	if deliveredCount > 0 {
		warnIgnoredCheckpointRemote(ctx, ps)
	}
	// Last, because git sends the user's push the moment this returns, and it
	// must not land right on top of the one that just finished (see
	// settleBeforeUserPush). A dedicated checkpoint_remote waits too: nothing
	// stops it naming the repository the user is pushing to.
	if pushed.moved {
		settleBeforeUserPush(ctx, ps.remote)
	}
	return nil
}

// checkpointBranchPush is what pushCheckpointBranchRefs left behind.
type checkpointBranchPush struct {
	// delivered lists the refs now on the target.
	delivered []plumbing.ReferenceName
	// anyFailed reports a ref that was due and did not deliver.
	anyFailed bool
	// moved reports that a push moved a ref on the target rather than finding
	// it there already.
	moved bool
}

// pushCheckpointBranchRefs pushes the git-branch backend's checkpoint refs to
// ps.pushTarget().
//
// Capture needs checkpoint data CONFIRMED on this remote, so it counts what
// landed rather than trusting the absence of an error: every ref that was due
// has to deliver, and at least one has to actually do so. Delivery must come
// from pushRefIfNeeded's delivered return and NOT from err, which is fail-soft
// and nil even when the remote refused the ref.
func pushCheckpointBranchRefs(ctx context.Context, ps pushSettings) (checkpointBranchPush, error) {
	// Thread the span's context into the push so the network push and any
	// fetch+rebase recovery nest beneath it as child steps in the perf trace.
	pushCtx, span := perf.Start(ctx, "push_checkpoint_refs")
	defer span.End()
	var result checkpointBranchPush
	for _, ref := range checkpoint.ResolveRefs(ctx).Push {
		delivered, moved, err := pushRefIfNeeded(pushCtx, ps.pushTarget(), ref)
		if err != nil {
			span.RecordError(err)
			return result, err
		}
		if delivered {
			result.delivered = append(result.delivered, ref)
		} else {
			result.anyFailed = true
		}
		result.moved = result.moved || moved
	}
	return result, nil
}

// opfPrePushDecision resolves the user's OPF decision for this push (env >
// settings > prompt > non-TTY auto-run). Shared by both checkpoint backends so
// the precedence cannot drift between them.
func opfPrePushDecision(ctx context.Context) (OPFDecision, error) {
	cfg, _ := settings.Load(ctx) //nolint:errcheck // Load already failed at hook init; fall back to nil
	var opfCfg *settings.OPFSettings
	if cfg != nil && cfg.Redaction != nil {
		opfCfg = cfg.Redaction.OpenAIPrivacyFilter //nolint:staticcheck // OPF is deprecated but still honored until it is removed
	}
	return resolveOPFDecisionForPrePush(ctx, opfCfg, opfPrePushProgressWriter)
}

// opfGateForCheckpointRefs resolves the OPF decision and, when it says run,
// rewrites the queued checkpoint refs. A nil error means the flush may proceed;
// a non-nil one means it must be withheld, leaving the refs queued rather than
// shipping 8-layer content the user did not opt out of. OPF being disabled is a
// nil error, so every flush can call this unconditionally.
//
// Every path that flushes the queue must pass through here, because skipping it
// is not the same as an OPFSkip: a skip is a decision the user made for this
// push and it ships untagged content deliberately, while a missing gate ships
// exactly the same bytes having never asked.
//
// Reporting belongs to the caller: the pre-push hook warns and carries on,
// because a checkpoint failure must never block the user's own git push, while
// an explicitly requested push surfaces the error instead.
//
// Precondition, and the one way to make this gate a silent no-op: OPFEnabled
// reads process-global config that only EnsureRedactionConfigured sets, so a
// caller that reaches here without it having run reads "OPF off" and flushes
// everything unscanned. Every caller must ensure it — the hook path via
// withHookSession (hooks_git_cmd.go), the migration push via pushMigratedRefs
// (doctor_migrate.go).
//
// This gate does not call it itself: on the hook path its scanner-config error
// is deliberately logged and survived rather than propagated, so raising it
// here would withhold pushes on a condition that path documents as
// non-fatal.
func opfGateForCheckpointRefs(ctx context.Context, repo *git.Repository) error {
	if !redact.OPFEnabled() {
		return nil
	}
	decision, err := opfPrePushDecision(ctx)
	if err != nil {
		return err
	}
	switch decision {
	case OPFAbort:
		return ErrOPFAbortedByUser
	case OPFSkip:
		// Explicit opt-out for this push: flush the 8-layer content as-is,
		// untagged — same as the v1 path.
		logging.Info(ctx, "OPF skipped for this push (user choice or settings)")
	case OPFRun:
		_, opfSpan := perf.Start(ctx, "opf_pre_push_rewrite_refs")
		defer opfSpan.End()
		if rewriteErr := RewriteQueuedCheckpointRefsWithOPF(ctx, repo); rewriteErr != nil {
			opfSpan.RecordError(rewriteErr)
			return rewriteErr
		}
	}
	return nil
}

// warnOPFCheckpointRefsWithheld reports a withheld flush on both channels: the
// log for diagnosis, and the user's terminal so a silently un-synced checkpoint
// is never the first they hear of it.
//
// Names no cause, for the reason PushQueuedCheckpointRefs does not either: the
// gate withholds on an unresolvable decision or a failed scan, but also on a
// ref update that lost a race after a scan that ran fine. Saying "OPF did not
// run" on the pre-push channel — the one nearly every withheld flush comes
// through — would send those users after the wrong problem. The wrapped error
// says which it was.
func warnOPFCheckpointRefsWithheld(ctx context.Context, err error) {
	logging.Warn(ctx, "checkpoint ref push withheld; refs left queued",
		slog.String("error", err.Error()),
	)
	fmt.Fprintf(stderrWriter,
		"[entire] Your checkpoint refs were not pushed and stay queued for the next push: %v\n", err)
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
// refs stay queued for the next pre-push. When OPF is enabled the queued refs
// are re-redacted with it first (RewriteQueuedCheckpointRefsWithOPF).
// fromGitHook is true for a user's git push, the only push that may ask the
// first-push policy question.
func (s *ManualCommitStrategy) prePushCheckpointRefs(ctx context.Context, ps pushSettings, pendingCapture string, fromGitHook bool) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Warn(ctx, "git-refs pre-push: open repo failed; skipping checkpoint push",
			slog.String("error", err.Error()))
		return nil
	}
	defer repo.Close()

	if fromGitHook {
		answer, saved := askCheckpointPushPolicyOnFirstPush(ctx, repo)
		if answer == checkpointPushAnswerSuperseded {
			// The check before the question ran while the policy was unset; the
			// policy stored meanwhile decides this push.
			if err := CheckOuterPushHeldCheckpoints(ctx); err != nil {
				return err
			}
		}
		if answer == checkpointPushAnswerManual {
			// CheckOuterPushHeldCheckpoints and the old-hook warning ran before
			// the question, while the policy was unset, so both run again. The
			// question is asked only while checkpoints wait, so the warning
			// needs no count.
			if err := refuseOuterPushAfterManualAnswer(ctx); err != nil {
				return err
			}
			warnPrePushRefsUnknown(ctx, stderrWriter)
			if !saved {
				return nil
			}
		}
	}

	// Before OPF, so a manual clone's queue is never scanned on git push:
	// nothing in it is about to upload.
	if !announceCheckpointPushPolicy(ctx, repo) {
		return nil
	}

	// OPF backend divergence: both paths fail closed, but this one does it
	// without blocking the user. The v1 path aborts the user's git push; here a
	// checkpoint-ref failure must never do that (see this function's doc), so
	// failing closed means withholding the flush — nothing un-OPF'd ships, the
	// refs stay queued, and the user's push proceeds.
	if opfErr := opfGateForCheckpointRefs(ctx, repo); opfErr != nil {
		warnOPFCheckpointRefsWithheld(ctx, opfErr)
		return nil
	}

	flushed, err := flushCheckpointRefsQueue(ctx, repo, ps, batchWithinFlushBudget)
	if err != nil {
		// Fail-soft: a checkpoint-ref push failure must never block the user's
		// git push. The refs stay queued for the next pre-push.
		logging.Warn(ctx, "git-refs pre-push: checkpoint ref push failed; refs left queued",
			slog.String("error", err.Error()))
	}
	// Delivered, and only if something actually was — counted, not inferred
	// from the error: a chunked flush can land chunks and still fail later
	// ones, and those checkpoints reached the remote all the same. An empty
	// queue pushed nothing, so it must not move the election or announce that
	// it had, nor warn that checkpoints were misdirected (see
	// warnIgnoredCheckpointRemote).
	if flushed > 0 {
		if pendingCapture != "" {
			commitCapturedSyncRemote(ctx, pendingCapture)
		}
		warnIgnoredCheckpointRemote(ctx, ps)
	}
	return nil
}

// PushQueuedCheckpointRefs pushes any queued checkpoint refs to the configured
// checkpoint remote, surfacing errors (unlike the fail-soft pre-push path); the
// caller owns the repo. It returns the number of refs pushed and whether
// pushing is disabled in settings — a distinct signal from pushed==0 with
// pushing enabled (an empty queue), so callers can report the two accurately.
// Like the pre-push paths, an OPF rewrite that cannot run when OPF is enabled
// errors with the refs left queued, and a manual policy returns
// ErrCheckpointPushHeldByPolicy. Currently used by the checkpoint migration
// command's opt-in "push now".
func PushQueuedCheckpointRefs(ctx context.Context, repo *git.Repository, remote string) (pushed int, pushDisabled bool, err error) {
	ps := resolvePushSettings(ctx, remote)
	if ps.pushDisabled {
		return 0, true, nil
	}
	if automatic, policyErr := automaticCheckpointDelivery(repo); policyErr != nil {
		return 0, false, fmt.Errorf("checkpoint refs stay queued: %w", policyErr)
	} else if !automatic {
		return 0, false, ErrCheckpointPushHeldByPolicy
	}
	if ps.withheld != nil {
		return 0, false, fmt.Errorf("checkpoint refs stay queued: %w", ps.withheld)
	}
	if opfErr := opfGateForCheckpointRefs(ctx, repo); opfErr != nil {
		// Names no cause, matching flushCheckpointRefsQueue's retry message
		// below: the gate fails on an unresolvable decision or a failed scan,
		// but also on a ref update that lost a race after a scan that ran
		// fine, so "OPF did not run" would sometimes send the user after the
		// wrong problem. The wrapped error says which it was.
		return 0, false, fmt.Errorf("checkpoint refs stay queued: %w", opfErr)
	}
	pushed, err = flushCheckpointRefsQueue(ctx, repo, ps, batchUnbounded)
	return pushed, false, err
}

// checkpointRefPartition sorts drained queue entries for one upload. Only
// ready refs are sent. Waiting and changed refs stay queued and are not
// failures: they are left out of retries, rotation and the pushed-of count.
type checkpointRefPartition struct {
	ready []checkpointRefPush
	// waiting were not selected for this upload.
	waiting []checkpoint.PushQueueEntry
	// changed were selected with a pinned commit that is no longer the tip;
	// each carries the current tip.
	changed    []checkpointRefPush
	stale      []checkpoint.PushQueueEntry
	unreadable []checkpoint.PushQueueEntry
	// readErrs holds each unreadable ref's lookup error.
	readErrs map[plumbing.ReferenceName]error
}

// partitionCheckpointRefPushes captures each queued ref's current immutable
// tip. The queue token remains attached solely for exact-generation cleanup
// after delivery. A ref whose lookup fails for any reason but absence is
// unreadable: it has no tip to pin, so it stays queued for the next push rather
// than being pruned over a flaky read. Stale entries are reported whether or
// not they are selected: pruning them sends nothing.
func partitionCheckpointRefPushes(
	repo *git.Repository,
	entries []checkpoint.PushQueueEntry,
	selection checkpointRefSelection,
) checkpointRefPartition {
	part := checkpointRefPartition{
		ready:    make([]checkpointRefPush, 0, len(entries)),
		readErrs: make(map[plumbing.ReferenceName]error),
	}
	for _, entry := range entries {
		// A ref the selection leaves out waits untouched: a push of named refs
		// does not read, prune or report the others.
		pin, selected := selection.pin(entry.Ref)
		if !selected {
			part.waiting = append(part.waiting, entry)
			continue
		}
		ref, err := repo.Reference(entry.Ref, true)
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			part.stale = append(part.stale, entry)
			continue
		}
		if err != nil {
			part.unreadable = append(part.unreadable, entry)
			part.readErrs[entry.Ref] = fmt.Errorf("read checkpoint ref: %w", err)
			continue
		}
		candidate := checkpointRefPush{token: entry, name: entry.Ref, hash: ref.Hash()}
		switch {
		case !pin.IsZero() && pin != candidate.hash:
			part.changed = append(part.changed, candidate)
		default:
			part.ready = append(part.ready, candidate)
		}
	}
	return part
}

// warnUnreadableCheckpointRefs names one ref: a ref that stays unreadable is
// reported on every push, and the count alone gives the user nothing to look for.
func warnUnreadableCheckpointRefs(ctx context.Context, unreadable []checkpoint.PushQueueEntry) {
	names := make([]string, 0, len(unreadable))
	for _, entry := range unreadable {
		names = append(names, entry.Ref.String())
	}
	logging.Warn(ctx, "git-refs push: checkpoint refs could not be read; left queued",
		slog.Int("unreadable", len(unreadable)), slog.Any("refs", names))
	fmt.Fprintf(os.Stderr, "[entire] %d checkpoint ref(s) could not be read and stay queued for the next push (for example %s).\n",
		len(unreadable), names[0])
}

func checkpointRefPushTokens(refs []checkpointRefPush) []checkpoint.PushQueueEntry {
	entries := make([]checkpoint.PushQueueEntry, 0, len(refs))
	for _, ref := range refs {
		entries = append(entries, ref.token)
	}
	return entries
}

// flushCheckpointRefsQueue drains the push-discovery queue and batch-pushes the
// recorded refs fast-forward-only, recovering a diverged ref by fetch+replay and
// removing from the queue only the refs that land. It returns the number pushed.
//
// Shared by the git-refs pre-push path (which logs and ignores the error to
// never block the user's push) and the migration command's opt-in push (which
// surfaces it). bound says whether the batch push runs under
// checkpointFlushBudget too (see checkpointBatchBound). Stale
// entries — refs no longer present locally — are pruned so they don't block the
// queue forever.
//
// This is automatic delivery, so it sends nothing unless the clone's policy is
// automatic. The callers check first to skip OPF; the check is repeated here so
// a new caller cannot upload under manual by forgetting it.
func flushCheckpointRefsQueue(ctx context.Context, repo *git.Repository, ps pushSettings, bound checkpointBatchBound) (int, error) {
	automatic, err := automaticCheckpointDelivery(repo)
	if err != nil {
		return 0, err
	}
	if !automatic {
		return 0, nil
	}
	result, err := flushCheckpointRefsWith(ctx, repo, ps, checkpointRefSelection{}, bound)
	return len(result.pushed), err
}

// checkpointBatchBound says whether a flush's batch push shares
// checkpointFlushBudget with its per-ref fallback.
type checkpointBatchBound bool

const (
	// batchWithinFlushBudget is the pre-push path: the whole flush, batch
	// included, must not hold the user's git push past the budget.
	batchWithinFlushBudget checkpointBatchBound = true
	// batchUnbounded is the explicit pushes (migration, `entire checkpoint
	// push`): the user asked for the upload and is waiting on it, so the batch
	// runs to the end and the per-ref fallback gets a full budget after it.
	batchUnbounded checkpointBatchBound = false
)

// checkpointFlushResult reports what one flush did with the refs it selected.
type checkpointFlushResult struct {
	pushed  []checkpointRefPush
	changed []checkpointRefPush
	failed  map[plumbing.ReferenceName]error
}

// markUnsettled records cause for every ref of existing the flush neither
// pushed, saw superseded, nor already failed: they stay queued.
func (r *checkpointFlushResult) markUnsettled(existing []checkpointRefPush, cause error) {
	settled := make(map[plumbing.ReferenceName]struct{}, len(r.pushed)+len(r.changed))
	for _, ref := range r.pushed {
		settled[ref.name] = struct{}{}
	}
	for _, ref := range r.changed {
		settled[ref.name] = struct{}{}
	}
	for _, ref := range existing {
		if _, ok := settled[ref.name]; ok {
			continue
		}
		if _, ok := r.failed[ref.name]; !ok {
			r.failed[ref.name] = cause
		}
	}
}

// flushCheckpointRefs is flushCheckpointRefsQueue for any selection, with
// the outcome of every selected ref. It does not read the policy: an explicit
// selection is the user naming what to send. The batch is unbounded: the user
// asked for this upload and is waiting on it.
func flushCheckpointRefs(ctx context.Context, repo *git.Repository, ps pushSettings, selection checkpointRefSelection) (checkpointFlushResult, error) {
	return flushCheckpointRefsWith(ctx, repo, ps, selection, batchUnbounded)
}

// flushCheckpointRefsWith is flushCheckpointRefs with the batch push bounded
// as bound says.
func flushCheckpointRefsWith(
	ctx context.Context,
	repo *git.Repository,
	ps pushSettings,
	selection checkpointRefSelection,
	bound checkpointBatchBound,
) (checkpointFlushResult, error) {
	// Callers check first; repeated so a new caller cannot hand a withheld
	// store's checkpoints to the push remote.
	if ps.withheld != nil {
		return checkpointFlushResult{}, ps.withheld
	}
	result := checkpointFlushResult{failed: make(map[plumbing.ReferenceName]error)}
	queue, err := checkpoint.PushQueueForRepo(ctx, repo)
	if err != nil {
		return result, fmt.Errorf("resolve push queue: %w", err)
	}
	queued, err := queue.DrainEntries()
	if err != nil {
		return result, fmt.Errorf("drain push queue: %w", err)
	}
	if len(queued) == 0 {
		return result, nil
	}

	pushCtx, pushSpan := perf.Start(ctx, "push_checkpoint_refs")
	defer pushSpan.End()

	part := partitionCheckpointRefPushes(repo, queued, selection)
	result.changed = part.changed
	if len(part.stale) > 0 {
		if err := queue.RemoveEntries(part.stale); err != nil {
			logging.Warn(ctx, "git-refs push: prune stale queue entries failed",
				slog.String("error", err.Error()))
		}
	}
	if len(part.unreadable) > 0 {
		warnUnreadableCheckpointRefs(ctx, part.unreadable)
		maps.Copy(result.failed, part.readErrs)
	}
	existing := part.ready
	if len(existing) == 0 {
		return result, nil
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

	// One budget for the whole flush, opened before the batch: a backlog upload
	// is as able to hang the user's push as a walk of per-ref retries. Bounded;
	// see checkpointFlushBudget and maxConsecutiveRefPushFailures. An unbounded
	// batch opens it after the batch instead, so its per-ref fallback is not left
	// with a budget the batch already spent.
	flushCtx, cancelFlush := context.WithTimeout(pushCtx, checkpointFlushBudget)
	defer cancelFlush()
	batchCtx := flushCtx
	if bound == batchUnbounded {
		batchCtx = pushCtx
	}

	// Fast path: push the refs a chunk per round-trip (fast-forward-only).
	// The remembered size only fits a chunk to the flush budget, so an
	// unbounded batch uses the full size and leaves the hint alone.
	chunkSize := checkpointRefPushChunkSize
	if bound == batchWithinFlushBudget {
		chunkSize = queue.ChunkSizeHint(checkpointRefPushChunkSize)
	}
	batch := pushRefChunks(batchCtx, dest.target, existing, chunkSize, func(landed []checkpointRefPush) {
		result.pushed = append(result.pushed, landed...)
		if removeErr := queue.RemoveEntries(checkpointRefPushTokens(landed)); removeErr != nil {
			logging.Warn(ctx, "git-refs push: clear pushed refs from queue failed",
				slog.String("error", removeErr.Error()))
		}
	})
	// The budget, not an interruption, cut the batch.
	budgetCut := bound == batchWithinFlushBudget &&
		errors.Is(flushCtx.Err(), context.DeadlineExceeded) && pushCtx.Err() == nil
	if bound == batchWithinFlushBudget {
		adaptChunkSize(ctx, queue, chunkSize, batch, budgetCut)
	}
	if budgetCut && len(batch.failed) > 0 && !batch.sshAuthFailed && batch.unreachable == "" {
		// The budget ran out mid-chunk: the per-ref fallback would only fail
		// its one guaranteed attempt on the spent budget. Stop as if between
		// chunks, and send the cut chunk to the back of the queue.
		stopOnBudgetCut(ctx, flushCtx, queue, batch, len(existing), stop)
		result.markUnsettled(existing, fmt.Errorf("not attempted: %s", flushAbortReason(flushCtx, 0)))
		return result, nil
	}
	if bound == batchUnbounded {
		var cancelFallback context.CancelFunc
		flushCtx, cancelFallback = context.WithTimeout(pushCtx, checkpointFlushBudget)
		defer cancelFallback()
	}
	if done, err := settleBatchOnly(ctx, batch, len(existing), dest, stop); done {
		cause := err
		if cause == nil {
			cause = fmt.Errorf("not attempted: %s", batch.stopReason)
		}
		result.markUnsettled(existing, cause)
		return result, err
	}

	// At least one chunk failed — typically a non-fast-forward divergence (the
	// same checkpoint re-written on another machine). Retry its refs one at a
	// time with fetch+replay recovery, and remove from the queue only the refs
	// that land (a genuine cherry-pick conflict leaves that ref queued for a
	// later push, never force-overwriting the remote).
	// Deliberately names no cause: the batch fails on divergence, but just as
	// often on an unreachable or unauthorized destination. Telling a user with a
	// dead remote that their refs "diverged" — or were "rejected", which equally
	// implies the remote answered — sends them after the wrong problem.
	fmt.Fprintf(os.Stderr, "[entire] Checkpoint ref push failed; retrying %d ref(s) individually...", len(batch.failed))
	stop = startProgressDots(os.Stderr)
	retry := retryRefsIndividually(ctx, flushCtx, repo, dest.target, batch.failed, len(existing))
	return finishIndividualRetry(ctx, pushCtx, queue, existing, batch, retry, stop, result)
}

// stopOnBudgetCut reports and settles a batch the flush budget cut mid-chunk.
func stopOnBudgetCut(ctx, flushCtx context.Context, queue *checkpoint.PushQueue, batch chunkPushResult, queued int, stop func(string)) {
	stop(fmt.Sprintf(" pushed %d of %d", batch.landed, queued))
	fmt.Fprintf(os.Stderr, "[entire] Stopped pushing: %s; %d checkpoint ref(s) stay queued for the next push.\n",
		flushAbortReason(flushCtx, 0), queued-batch.landed)
	logging.Warn(ctx, "git-refs push: batch push cut by the flush budget; remaining refs stay queued",
		slog.Int("pushed", batch.landed), slog.Int("queued", queued))
	// An earlier chunk may have been refused outright before the clock ran
	// out. The fallback that would show why does not run on a spent budget,
	// so show the remote's reason here: a ref blocked for its content would
	// otherwise ride along with every budget cut without ever saying so.
	if reason := checkpointRefRejectionReason(batch.firstErr); reason != "" {
		fmt.Fprintf(os.Stderr, "[entire] Warning: the remote declined a checkpoint ref batch (showing one rejection):\n%s\n", reason)
	}
	// Fair scheduling, as after a fallback abort: a slow remote cuts the
	// budget at the same place every push, so the cut chunk goes to the
	// back rather than holding the head of the queue.
	if err := queue.Rotate(checkpointRefPushTokens(batch.failed)); err != nil {
		logging.Warn(ctx, "git-refs push: rotate cut chunk to queue back failed",
			slog.String("error", err.Error()))
	}
}

// adaptChunkSize updates the remembered chunk size after a batch. It is
// quartered when the budget cut a chunk in flight and nothing landed — a link
// too slow to land one chunk per budget would otherwise never move the head of
// the queue, and each such cut costs a whole budget, hence the steep step. A
// cut after chunks landed is a backlog draining as intended, not a chunk too
// large, so the size is kept. It doubles back toward checkpointRefPushChunkSize
// after any batch in which every chunk it attempted landed, even if the budget
// or a stop left later chunks untried: a draining backlog may never empty the
// queue in one flush, and one transient stall must not pin the size low.
func adaptChunkSize(ctx context.Context, queue *checkpoint.PushQueue, size int, batch chunkPushResult, budgetCut bool) {
	next := size
	switch {
	case batch.landed == 0 && budgetCut && len(batch.failed) > 0:
		next = max(1, size/4)
	case len(batch.failed) == 0 && batch.landed > 0:
		next = min(checkpointRefPushChunkSize, size*2)
	}
	if next == size {
		return
	}
	if err := queue.SetChunkSizeHint(next); err != nil {
		logging.Debug(ctx, "git-refs push: remember chunk size failed", slog.String("error", err.Error()))
	}
}

// settleBatchOnly finishes a flush whose batch phase decided its outcome on its
// own — everything landed, the budget or an interruption stopped it with
// nothing failed, or the destination refused the SSH key or the connection —
// printing that outcome. done is false when failed chunks remain for the
// per-ref fallback, which then owns the progress line.
func settleBatchOnly(ctx context.Context, batch chunkPushResult, queued int, dest refsPushDestination, stop func(string)) (done bool, err error) {
	if len(batch.failed) == 0 && len(batch.untried) == 0 {
		stop(" done")
		return true, nil
	}
	if len(batch.failed) == 0 {
		// Every attempted chunk landed, but the budget ran out first. Nothing
		// failed, so this is progress rather than an error.
		stop(fmt.Sprintf(" pushed %d of %d", batch.landed, queued))
		fmt.Fprintf(os.Stderr, "[entire] Stopped pushing: %s; %d checkpoint ref(s) stay queued for the next push.\n",
			batch.stopReason, len(batch.untried))
		logging.Warn(ctx, "git-refs push: batch push stopped early; remaining refs stay queued",
			slog.String("reason", batch.stopReason), slog.Int("pushed", batch.landed),
			slog.Int("queued", queued))
		return true, nil
	}
	if batch.unreachable != "" {
		stop(" failed")
	} else {
		stop("")
	}
	if reportUnpushableDestination(dest, batch, queued) {
		return true, batch.firstErr
	}
	return false, nil
}

// reportUnpushableDestination prints the hint for a batch that failed because
// the destination refused the SSH key or could not be reached, and reports
// whether it was one of those: the per-ref fallback cannot fix either.
func reportUnpushableDestination(dest refsPushDestination, batch chunkPushResult, queued int) bool {
	// Non-interactive SSH auth failures cannot be fixed by per-ref
	// fetch+replay. Surface the same actionable hint as the v1 doPushRef path
	// (issue #1523) instead of only logging to .entire/logs/.
	// Deliberately does not print the batch error: it carries git's own output,
	// and this runs inside the user's `git push`. The hint below is the
	// actionable part; the full error reaches .entire/logs via the caller, which
	// logs the error this branch returns.
	if batch.sshAuthFailed {
		fmt.Fprintln(os.Stderr, "[entire] Warning: couldn't push checkpoint refs (SSH authentication failed).")
		printNonInteractiveSSHAuthHint()
		if dest.checkpointRemote {
			printCheckpointRemoteHint(dest.target)
		}
		return true
	}

	// Nothing landed and git could not even connect: the per-ref fallback would
	// fail the same way, one connect timeout per ref. Unlike the branch above,
	// name the cause: it is one line from ssh or curl (credentials redacted, see
	// remote.UnreachableRemoteLine), and without it the user sees a slow push
	// fail with no reason. No rotation: no ref failed on its own account.
	if batch.unreachable != "" {
		fmt.Fprintf(os.Stderr, "[entire] Couldn't reach %s: %s\n", dest.display(), batch.unreachable)
		fmt.Fprintf(os.Stderr, "[entire] %d checkpoint ref(s) stay queued for the next push.\n", queued)
		if dest.checkpointRemote {
			printCheckpointRemoteHint(dest.target)
		}
		return true
	}
	return false
}

// queuedRefWarning returns the line to print for a ref that stays queued, or ""
// when the failure needs no warning.
func queuedRefWarning(pushErr error, target string, ref plumbing.ReferenceName) string {
	if reason := checkpointRefRejectionReason(pushErr); reason != "" {
		return fmt.Sprintf("[entire] Warning: checkpoint ref %s remains queued (showing one rejection):\n%s", ref, reason)
	}
	if errors.Is(pushErr, errShallowHidesMergeBase) {
		// Divergence alone stays quiet, but this one never clears on its own:
		// every push would fetch and refuse again.
		return fmt.Sprintf("[entire] Warning: checkpoint ref %s remains queued: %v (from %s)", ref, errShallowHidesMergeBase, displayPushTarget(target))
	}
	return ""
}

// finishIndividualRetry reports and settles a flush after its per-ref fallback.
func finishIndividualRetry(
	ctx, pushCtx context.Context,
	queue *checkpoint.PushQueue,
	existing []checkpointRefPush,
	batch chunkPushResult,
	retry refRetryResult,
	stop func(string),
	result checkpointFlushResult,
) (checkpointFlushResult, error) {
	result.pushed = append(result.pushed, retry.pushed...)
	result.changed = append(result.changed, retry.changed...)
	maps.Copy(result.failed, retry.errs)
	stop(fmt.Sprintf(" pushed %d of %d", len(result.pushed), len(existing)))
	// The fallback reached every failed ref, but the batch stopped before
	// trying the rest of the queue: report that stop instead.
	abortReason := retry.abortReason
	if abortReason == "" && len(batch.untried) > 0 {
		abortReason = batch.stopReason
	}
	// Printed before the rejection warning so the more specific reason lands
	// closest to the prompt.
	if abortReason != "" {
		// Everything this flush did not land stays queued, not just the refs it
		// never reached: only landed refs are removed, so the ones that were
		// attempted and failed are still there too. Superseded refs stay queued
		// under their newer generation, but nothing failed to deliver them.
		fmt.Fprintf(os.Stderr, "[entire] Stopped retrying: %s; %d checkpoint ref(s) stay queued for the next push.\n",
			abortReason, len(existing)-len(result.pushed)-len(retry.changed))
	}
	// One actionable example per flush, after the progress line. Label it as
	// such: other queued refs may have different causes (all are logged).
	// Preserve Git's line breaks rather than printing the single-line log error.
	// Do not print the batch error too, or diagnose speculative recovery as
	// divergence.
	if retry.rejectionWarning != "" {
		fmt.Fprintln(os.Stderr, retry.rejectionWarning)
	}
	if err := queue.RemoveEntries(checkpointRefPushTokens(retry.pushed)); err != nil {
		logging.Warn(ctx, "git-refs push: clear pushed refs from queue failed",
			slog.String("error", err.Error()))
	}
	// A bounded flush always stops at the same place unless the queue moves: it
	// is drained in first-seen order, so a prefix that always fails is retried
	// in that same order on every push and the refs behind it are never reached.
	// Rotating this flush's failures to the back makes the "stay queued for the
	// next push" promise true for the refs that were skipped.
	//
	// Deliberately also on an exhausted budget, not only on the failure cap.
	// Rotation is fair scheduling, not a verdict on a ref: it drops nothing and
	// only changes order, so it does not matter that a ref cut mid-flight by the
	// deadline failed for reasons of its own. What matters is that a slow remote
	// expires the budget at roughly the same position on every push, which
	// starves the tail of the queue exactly the way a failing prefix does.
	// Rotating only after the failure cap would leave that case unfixed.
	//
	// Not after an interruption, though: there the whole flush is abandoned
	// rather than bounded, nothing was fairly "skipped", and the process is on
	// its way out — reordering the queue then is churn at best.
	if abortReason != "" && pushCtx.Err() == nil && len(retry.failed) > 0 {
		if err := queue.Rotate(checkpointRefPushTokens(retry.failed)); err != nil {
			logging.Warn(ctx, "git-refs push: rotate failed refs to queue back failed",
				slog.String("error", err.Error()))
		}
	}
	if abortReason != "" {
		result.markUnsettled(existing, fmt.Errorf("not attempted: %s", abortReason))
	}
	if retry.firstErr != nil {
		// Counts attempts, not the queue: refs skipped by an early abort were
		// never tried, and reporting them as failures would overstate what the
		// remote actually refused. Superseded refs were attempted but did not
		// fail, so only the failures are counted.
		return result, fmt.Errorf("%d of %d attempted checkpoint refs failed to push: %w",
			len(retry.failed), retry.attempted, retry.firstErr)
	}
	return result, nil
}

// refRetryResult is what the per-ref fallback of a flush left behind.
type refRetryResult struct {
	pushed []checkpointRefPush
	// changed were superseded during recovery; each carries the current tip.
	changed          []checkpointRefPush
	failed           []checkpointRefPush // attempted and failed; unreached refs are in neither
	errs             map[plumbing.ReferenceName]error
	attempted        int
	firstErr         error
	abortReason      string
	rejectionWarning string
}

// retryRefsIndividually pushes refs one at a time with fetch+replay recovery
// under flushCtx, stopping early per flushAbortReason. ctx is used for logging.
func retryRefsIndividually(ctx, flushCtx context.Context, repo *git.Repository, target string, refs []checkpointRefPush, queued int) refRetryResult {
	res := refRetryResult{errs: make(map[plumbing.ReferenceName]error)}
	consecutiveFailures := 0
	for i, ref := range refs {
		// Before the attempt but never before the first: a flush always tries at
		// least one ref, and only aborts while refs are left to skip.
		if i > 0 {
			if res.abortReason = flushAbortReason(flushCtx, consecutiveFailures); res.abortReason != "" {
				logging.Warn(ctx, "git-refs push: individual retry stopped early; remaining refs stay queued",
					slog.String("reason", res.abortReason), slog.Int("attempted", i),
					slog.Int("queued", queued))
				break
			}
		}
		res.attempted++
		landed, err := pushCheckpointRefWithRecovery(flushCtx, repo, target, ref)
		if err == nil {
			consecutiveFailures = 0
			res.pushed = append(res.pushed, landed)
			continue
		}
		if errors.Is(err, errCheckpointRefSuperseded) {
			// A session wrote this ref during recovery. Its newer generation stays
			// queued for the next push, so this neither counts toward the stop nor
			// is reported as a failure.
			logging.Info(ctx, "git-refs push: checkpoint ref changed during recovery; newer generation stays queued",
				slog.String("ref", ref.name.String()))
			// Report the generation now waiting, so a caller has a commit to pin.
			if current, readErr := repo.Reference(ref.name, true); readErr == nil {
				res.changed = append(res.changed, checkpointRefPush{token: ref.token, name: ref.name, hash: current.Hash()})
			} else {
				res.errs[ref.name] = err
			}
			continue
		}
		consecutiveFailures++
		res.recordFailure(ctx, flushCtx, target, ref, err)
	}
	return res
}

// recordFailure notes one ref the fallback could not push.
func (res *refRetryResult) recordFailure(ctx, flushCtx context.Context, target string, ref checkpointRefPush, err error) {
	logging.Warn(ctx, "git-refs push: checkpoint ref push/sync failed; left queued, not overwritten",
		slog.String("ref", ref.name.String()), slog.String("error", err.Error()))
	if nonInteractiveSSHAuthFailure(flushCtx, err) {
		printNonInteractiveSSHAuthHint()
	}
	if res.rejectionWarning == "" {
		res.rejectionWarning = queuedRefWarning(err, target, ref.name)
	}
	if res.firstErr == nil {
		res.firstErr = err
	}
	res.failed = append(res.failed, ref)
	res.errs[ref.name] = err
}
