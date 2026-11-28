// Background checkpoint upload (git-refs).
//
// The pre-push hook records the upload and starts one detached worker per
// repository (bgupload), which pushes the queue once the user's `git push` has
// exited (waitForUserPushes): pushed from the hook, the refs land just before
// git's own pack, and GitHub rejects a push arriving right after another one
// finishes. The worker acts only on what the hook decided in the foreground
// (checkpoint.UploadRequest).
package strategy

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/bgupload"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointremote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/redact"
)

const checkpointUploadComponent = "checkpoint-upload"

// CheckpointUploadForegroundEnv, when set to 1, keeps the whole upload inside
// the pre-push hook. Set by test harnesses whose temp remotes vanish when the
// test ends.
const CheckpointUploadForegroundEnv = "ENTIRE_CHECKPOINT_UPLOAD_FOREGROUND"

// foregroundUploadLockWait bounds how long a foreground flush waits for a
// running worker before leaving its refs queued. Var for tests.
var foregroundUploadLockWait = 30 * time.Second

// checkpointUploadDeliveryBudget bounds one worker pass's flush. Var for tests.
var checkpointUploadDeliveryBudget = 10 * time.Minute

// checkpointUploadMaxPasses bounds the passes for requests that arrive while
// the worker runs.
const checkpointUploadMaxPasses = 5

// ciEnvVars mark environments whose checkout is discarded when the job ends,
// taking a detached worker's queue with it: upload inline there.
var ciEnvVars = []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI", "BUILDKITE", "JENKINS_URL", "TF_BUILD"}

func inCI() bool {
	for _, name := range ciEnvVars {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

// backgroundUploadInTests lets a test opt into the hand-off; in-process tests
// otherwise upload inline.
var backgroundUploadInTests = false

// checkpointUploadSpawn is the spawn seam, swapped in tests.
var checkpointUploadSpawn = func(worktreeRoot string) bool { return bgupload.Spawn(worktreeRoot, checkpoint.UploadKind) }

type uploadWorkerKey struct{}

// withinUploadWorker marks ctx as the worker, which never hands off or spawns.
func withinUploadWorker(ctx context.Context) context.Context {
	return context.WithValue(ctx, uploadWorkerKey{}, true)
}

func inUploadWorker(ctx context.Context) bool {
	v, _ := ctx.Value(uploadWorkerKey{}).(bool) //nolint:errcheck // absent means false
	return v
}

type carriedOPFDecisionKey struct{}

func withCarriedOPFDecision(ctx context.Context, decision string) context.Context {
	return context.WithValue(ctx, carriedOPFDecisionKey{}, decision)
}

// carriedOPFDecision is the OPF decision a hand-off carried, or UploadOPFUnset.
func carriedOPFDecision(ctx context.Context) string {
	v, _ := ctx.Value(carriedOPFDecisionKey{}).(string) //nolint:errcheck // absent means unset
	return v
}

// backgroundCheckpointUploadEnabled reports whether this pre-push may hand its
// upload to the worker. CI checkouts are discarded with the job, so they upload
// inline.
func backgroundCheckpointUploadEnabled(ctx context.Context) bool {
	if inUploadWorker(ctx) || (testing.Testing() && !backgroundUploadInTests) ||
		os.Getenv(CheckpointUploadForegroundEnv) == "1" || inCI() {
		return false
	}
	s, err := settings.Load(ctx)
	return err != nil || !s.IsBackgroundCheckpointUploadDisabled()
}

// lockQueueForFlush takes the upload lock for a foreground flush, waiting up to
// foregroundUploadLockWait for a running worker. ok is false when it timed out.
// A lock that cannot be used at all must not block uploads: that returns ok
// with a no-op release.
func lockQueueForFlush(ctx context.Context, coord *checkpoint.UploadCoordinator) (release func(), ok bool) {
	if coord == nil {
		return func() {}, true
	}
	lockCtx, cancel := context.WithTimeout(ctx, foregroundUploadLockWait)
	defer cancel()
	release, err := coord.Lock(lockCtx)
	if err != nil {
		if lockCtx.Err() != nil && ctx.Err() == nil {
			return nil, false
		}
		logging.Debug(ctx, "checkpoint upload lock unavailable; uploading without it", slog.String("error", err.Error()))
		return func() {}, true
	}
	return release, true
}

// reportBackgroundUpload prints, once, what the last worker run could not.
func reportBackgroundUpload(ctx context.Context, coord *checkpoint.UploadCoordinator) {
	if coord == nil {
		return
	}
	if err := coord.Report(stderrWriter, "checkpoint upload"); err != nil {
		logging.Debug(ctx, "report checkpoint upload result failed", slog.String("error", err.Error()))
	}
}

// handOffCheckpointUpload records req and starts a worker. If the lock is held,
// the new worker exits at once and the holder starts one on release
// (releaseUploadLock).
func handOffCheckpointUpload(ctx context.Context, coord *checkpoint.UploadCoordinator, req checkpoint.UploadRequest) bool {
	if err := coord.Request(req); err != nil {
		logging.Warn(ctx, "record background checkpoint upload request failed", slog.String("error", err.Error()))
		return false
	}
	if !spawnCheckpointUploadWorker(ctx) {
		return false
	}
	logging.Info(logging.WithComponent(ctx, checkpointUploadComponent),
		"handed checkpoint upload to background worker", slog.String("remote", req.Remote))
	return true
}

func spawnCheckpointUploadWorker(ctx context.Context) bool {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Debug(ctx, "skipping background checkpoint upload spawn: no worktree root", slog.String("error", err.Error()))
		return false
	}
	return checkpointUploadSpawn(root)
}

// releaseUploadLock releases a flush's lock and starts a worker for a request
// recorded meanwhile: that push's own worker found the lock held and exited.
func releaseUploadLock(ctx context.Context, coord *checkpoint.UploadCoordinator, release func()) {
	release()
	if coord != nil && !inUploadWorker(ctx) && coord.HasRequest() && backgroundCheckpointUploadEnabled(ctx) {
		spawnCheckpointUploadWorker(ctx)
	}
}

// RunCheckpointUploadWorker is the detached worker's body. Each pass takes the
// pending request and uploads the queue under it; refs left queued wait for the
// next push. Failures are recorded and logged, never returned: nothing watches
// this process's exit code.
func RunCheckpointUploadWorker(ctx context.Context) {
	ctx = logging.WithComponent(ctx, checkpointUploadComponent)
	defer logging.Info(ctx, "checkpoint upload worker finished")
	spawnCtx := context.WithoutCancel(ctx) // not marked as the worker, so it may spawn the next
	// The hook's environment may pin GIT_DIR and friends to the spawning
	// worktree; resolve from the working directory instead.
	for _, name := range gitrepo.RepoOverrideEnvVars() {
		os.Unsetenv(name)
	}
	ctx = checkpointremote.WithNonInteractiveSSH(withinUploadWorker(ctx))
	ctx, cancel := context.WithTimeout(ctx, checkpoint.UploadWorkerDeadline)
	defer cancel()

	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Warn(ctx, "checkpoint upload worker: open repository failed", slog.String("error", err.Error()))
		return
	}
	defer repo.Close()
	coord, err := checkpoint.UploadCoordinatorForRepo(repo)
	if err != nil {
		logging.Warn(ctx, "checkpoint upload worker: resolve state failed", slog.String("error", err.Error()))
		return
	}
	if !uploadRedactionReady(ctx, coord) {
		return
	}
	leftover := coord.RunPasses(ctx, checkpointUploadMaxPasses, checkpoint.HasUploadRequest, func(ctx context.Context) {
		runCheckpointUploadPass(ctx, repo, coord)
	})
	if leftover && backgroundCheckpointUploadEnabled(spawnCtx) {
		spawnCheckpointUploadWorker(spawnCtx)
	}
}

// uploadRedactionReady configures redaction for a run, recording a failed run
// when it cannot: without it the OPF gate reads "off" and would ship unscanned
// content. EnsureRedactionConfigured survives a settings read failure (the hook
// path must), so that is checked separately.
func uploadRedactionReady(ctx context.Context, coord *checkpoint.UploadCoordinator) bool {
	_, settingsErr := settings.Load(ctx)
	if err := EnsureRedactionConfigured(ctx); err != nil || settingsErr != nil {
		now := time.Now()
		if startErr := coord.StartRun(now); startErr == nil {
			recordUploadFinish(ctx, coord, bgupload.Result{StartedAt: now, FinishedAt: time.Now(),
				Error: "settings could not be loaded, so nothing was uploaded"})
		}
		return false
	}
	return true
}

// uploadPassWait and uploadPassOPFGate are vars so a test can check their order.
var (
	uploadPassWait    = waitForUserPushes
	uploadPassOPFGate = opfGateForCheckpointRefs
)

// runCheckpointUploadPass waits for the user's pushes, then uploads the queue
// under the pending request and records the outcome. The request is taken after
// the wait, so a push recorded meanwhile merges its OPF decision into it.
func runCheckpointUploadPass(ctx context.Context, repo *git.Repository, coord *checkpoint.UploadCoordinator) {
	uploadPassWait(ctx, coord)
	taken, err := coord.TakeRequest()
	if err != nil || taken == nil {
		return
	}
	req := *taken
	result := bgupload.Result{StartedAt: time.Now()}
	if err := coord.StartRun(result.StartedAt); err != nil {
		logging.Warn(ctx, "record checkpoint upload start failed", slog.String("error", err.Error()))
	}
	finish := func(errLine string) {
		result.FinishedAt = time.Now()
		result.Error = errLine
		recordUploadFinish(ctx, coord, result)
	}

	// What the hook path would print has no terminal here: keep it for the
	// next push.
	var printed bytes.Buffer
	ctx = withNoticeWriter(ctx, &printed)

	ctx = withCarriedOPFDecision(ctx, effectiveOPFDecision(ctx, repo, req))
	ps := resolvePushSettings(ctx, req.Remote)
	if ps.pushDisabled {
		finish("")
		return
	}
	if !ps.hasCheckpointURL() && !checkpointSyncAllowedForRemote(ctx, ps.remote, req.PendingCapture) {
		finish("checkpoints no longer sync to " + ps.remote)
		return
	}
	// The hook decided OPF must run but this process sees it off: uploading
	// would ship what the user asked to have scanned.
	if req.OPFDecision == checkpoint.UploadOPFRun && !redact.OPFEnabled() {
		finish("the privacy filter could not run here, so nothing was uploaded")
		return
	}
	// After the wait: a commit made meanwhile queues checkpoints the filter
	// has not seen, and the flush drains the queue straight after this.
	if opfErr := uploadPassOPFGate(ctx, repo); opfErr != nil {
		logging.Warn(ctx, "background checkpoint upload withheld", slog.String("error", opfErr.Error()))
		finish("withheld before upload (see .entire/logs)")
		return
	}

	// A push made during a long upload is waited for between chunks, within
	// the budget.
	res, err := flushCheckpointRefsQueue(ctx, repo, ps, flushOptions{
		budget: checkpointUploadDeliveryBudget, boundBatch: true,
		beforeChunk: func(ctx context.Context) { waitForUserPushes(ctx, coord) }})
	if err != nil {
		logging.Warn(ctx, "background checkpoint upload: refs left queued", slog.String("error", err.Error()))
	}
	if res.pushed > 0 {
		if req.PendingCapture != "" {
			commitCapturedSyncRemote(ctx, req.PendingCapture)
		}
		warnIgnoredCheckpointRemote(ctx, ps)
	}
	result.Done, result.Remaining = res.pushed, res.remaining
	if res.remaining > 0 || err != nil {
		result.Detail = res.detail
	}
	result.Announcement = strings.TrimSpace(printed.String())
	finish(res.failureLine(err))
}

// effectiveOPFDecision is req's OPF decision, except that a skip covers only
// the queue it was given for: once other checkpoints are queued, the
// non-interactive rules decide (UploadOPFUnset).
func effectiveOPFDecision(ctx context.Context, repo *git.Repository, req checkpoint.UploadRequest) string {
	if req.OPFDecision != checkpoint.UploadOPFSkip {
		return req.OPFDecision
	}
	entries, err := queuedEntries(ctx, repo)
	if err != nil || checkpoint.QueueDigest(entries) != req.QueueDigest {
		return checkpoint.UploadOPFUnset
	}
	return req.OPFDecision
}

func recordUploadFinish(ctx context.Context, coord *checkpoint.UploadCoordinator, res bgupload.Result) {
	if err := coord.FinishRun(res); err != nil {
		logging.Warn(ctx, "record checkpoint upload result failed", slog.String("error", err.Error()))
	}
}
