// Background checkpoint upload (git-refs).
//
// The pre-push hook uploads queued checkpoint refs inline for a short budget
// (checkpointInlineUploadBudget). A queue that does not fit — a backlog, a slow
// uplink — is handed to one detached `entire __checkpoint_upload` worker per
// repository, so the user's `git push` returns in seconds and the remaining
// checkpoints follow. An ordinary push finishes inside the inline budget and
// behaves exactly as before: checkpoints are on the remote when the push returns.
//
// The worker acts only on what the pushing hook decided in the foreground, where
// the user can still be asked: the OPF decision and the pending sync-remote
// capture travel in the request (checkpoint.UploadRequest). Its stdio is the
// null device, so what it would have printed is recorded instead
// (checkpoint.UploadResult) and shown by the next push and by `entire status`.
package strategy

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointremote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/redact"
)

const checkpointUploadComponent = "checkpoint-upload"

// CheckpointUploadForegroundEnv, when set to 1, keeps the whole upload inside
// the pre-push hook. Set by callers that report on the hook's delivery
// (`entire trail create`) and by test harnesses whose temp remotes vanish when
// the test ends.
const CheckpointUploadForegroundEnv = "ENTIRE_CHECKPOINT_UPLOAD_FOREGROUND"

// checkpointInlineUploadBudget is how long the pre-push hook uploads before it
// hands the rest to the worker. Long enough that an ordinary push never hands
// off — its checkpoints are on the remote when the push returns, which
// entire.io, `entire trail create`, and short-lived environments rely on — and
// short enough that a backlog no longer holds the user's push. Var for tests.
var checkpointInlineUploadBudget = 5 * time.Second

// foregroundUploadLockWait bounds how long a foreground flush (hand-off
// disabled) waits for a running worker before leaving its refs queued. Var for
// tests.
var foregroundUploadLockWait = 30 * time.Second

const (
	// checkpointUploadDeliveryBudget bounds one worker pass's flush.
	checkpointUploadDeliveryBudget = 10 * time.Minute
	// checkpointUploadWorkerDeadline bounds a worker's whole run, and is how
	// long a run record without a finish time counts as still running.
	checkpointUploadWorkerDeadline = 30 * time.Minute
	// checkpointUploadMaxPasses bounds the passes a worker makes for requests
	// that arrive while it runs.
	checkpointUploadMaxPasses = 5
)

// ciEnvVars mark environments whose machine or checkout is discarded when the
// job ends, taking a detached worker's queue with it: upload inline there.
var ciEnvVars = []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI", "BUILDKITE", "JENKINS_URL", "TF_BUILD"}

// backgroundUploadInTests lets a test opt into the hand-off; in-process tests
// otherwise upload inline, as before.
var backgroundUploadInTests = false

// checkpointUploadChunkTimeout bounds one chunk push inside a worker pass, so a
// stalled upload is moved to the back of the queue instead of holding every
// chunk behind it for the whole delivery budget. Var for tests.
var checkpointUploadChunkTimeout = max(time.Minute, checkpointUploadDeliveryBudget/8)

// checkpointUploadSpawn is the process-spawn seam, swapped in tests:
// execx.SpawnDetached is a no-op under `go test`.
var checkpointUploadSpawn = func(worktreeRoot string) {
	execx.SpawnDetached(worktreeRoot, "__checkpoint_upload")
}

type uploadWorkerKey struct{}

// withinUploadWorker marks ctx as the background worker, which uploads inline
// and never hands off or spawns another worker.
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

// backgroundCheckpointUploadEnabled reports whether this pre-push may hand the
// rest of its upload to the background worker.
func backgroundCheckpointUploadEnabled(ctx context.Context) bool {
	if inUploadWorker(ctx) {
		return false
	}
	if testing.Testing() && !backgroundUploadInTests {
		return false
	}
	if os.Getenv(CheckpointUploadForegroundEnv) == "1" {
		return false
	}
	for _, name := range ciEnvVars {
		if os.Getenv(name) != "" {
			return false
		}
	}
	if s, err := settings.Load(ctx); err == nil && s.IsBackgroundCheckpointUploadDisabled() {
		return false
	}
	return true
}

// lockQueueForFlush takes the upload lock for a pre-push flush, so it never
// pushes the same refs as a running worker. With hand-off enabled it only tries:
// a running worker already owns the queue, and the caller records a request for
// it instead. Otherwise it waits up to foregroundUploadLockWait. ok is false
// when the lock was not taken; a coordinator that cannot be used at all must not
// block uploads, so that returns ok with a no-op release.
func lockQueueForFlush(ctx context.Context, coord *checkpoint.UploadCoordinator, background bool) (release func(), ok bool) {
	if coord == nil {
		return func() {}, true
	}
	if background {
		release, ok, err := coord.TryLockWorker(ctx)
		if err != nil {
			logging.Debug(ctx, "checkpoint upload lock unavailable; uploading without it",
				slog.String("error", err.Error()))
			return func() {}, true
		}
		return release, ok
	}
	lockCtx, cancel := context.WithTimeout(ctx, foregroundUploadLockWait)
	defer cancel()
	release, err := coord.LockWorker(lockCtx)
	if err != nil {
		if lockCtx.Err() != nil && ctx.Err() == nil {
			return nil, false
		}
		logging.Debug(ctx, "checkpoint upload lock unavailable; uploading without it",
			slog.String("error", err.Error()))
		return func() {}, true
	}
	return release, true
}

// reportBackgroundUpload prints, once, what the last worker run could not: a
// failure that left refs queued, and any line it would have printed.
func reportBackgroundUpload(ctx context.Context, coord *checkpoint.UploadCoordinator) {
	if coord == nil {
		return
	}
	st, err := coord.State()
	if err != nil || st.Last == nil || st.Last.Running(time.Now(), checkpointUploadWorkerDeadline) {
		return
	}
	if st.Last.Announcement == "" && st.Last.Error == "" {
		return
	}
	if st.Last.Announcement != "" {
		fmt.Fprintln(stderrWriter, st.Last.Announcement)
	}
	if st.Last.Error != "" {
		fmt.Fprintf(stderrWriter, "[entire] Last background checkpoint upload stopped: %s.\n", st.Last.Error)
	}
	if clearErr := coord.ClearReported(st.Last.StartedAt); clearErr != nil {
		logging.Debug(ctx, "clear checkpoint upload result failed", slog.String("error", clearErr.Error()))
	}
}

// handOffCheckpointUpload records req for the worker and starts one. The caller
// must have released the upload lock, or the new worker exits at once — which
// is safe: whoever holds the lock starts a worker for the request on release
// (releaseUploadLock).
func handOffCheckpointUpload(ctx context.Context, coord *checkpoint.UploadCoordinator, req checkpoint.UploadRequest) bool {
	req.RequestedAt = time.Now()
	if err := coord.Request(req); err != nil {
		logging.Warn(ctx, "record background checkpoint upload request failed",
			slog.String("error", err.Error()))
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
		logging.Debug(ctx, "skipping background checkpoint upload spawn: no worktree root",
			slog.String("error", err.Error()))
		return false
	}
	checkpointUploadSpawn(root)
	return true
}

// releaseUploadLock releases a flush's hold on the upload lock and, if a push
// recorded a request while it was held, starts a worker for it. A push that
// found the lock held relied on its holder to get the request done; a worker
// it spawned exited at once, since the lock was taken.
func releaseUploadLock(ctx context.Context, coord *checkpoint.UploadCoordinator, release func()) {
	release()
	if coord != nil && !inUploadWorker(ctx) && coord.HasRequest() && backgroundCheckpointUploadEnabled(ctx) {
		spawnCheckpointUploadWorker(ctx)
	}
}

// RunCheckpointUploadWorker is the body of the detached
// `entire __checkpoint_upload` worker. Each pass takes the pending request and
// uploads the queue under it; another pass runs only for a request that arrived
// meanwhile, and none after a pass that left refs queued — the next push
// retries those. Best-effort: failures are recorded and logged, never
// returned, because nothing watches this process's exit code.
func RunCheckpointUploadWorker(ctx context.Context) {
	ctx = logging.WithComponent(ctx, checkpointUploadComponent)
	defer logging.Info(ctx, "checkpoint upload worker finished")
	// A detached child inherits the hook's environment, which may pin GIT_DIR
	// and friends to the spawning worktree; resolve from the working directory
	// instead, like any other CWD-based command.
	unsetRepoOverrideEnv()
	ctx = checkpointremote.WithNonInteractiveSSH(withinUploadWorker(ctx))
	ctx, cancel := context.WithTimeout(ctx, checkpointUploadWorkerDeadline)
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
	// OPFEnabled reads process-global config only this sets. Without it the
	// gate reads "OPF off" and ships unscanned content, so a worker that cannot
	// configure redaction uploads nothing. EnsureRedactionConfigured survives a
	// settings read failure (the hook path must), so check that separately.
	_, settingsErr := settings.Load(ctx)
	if err := EnsureRedactionConfigured(ctx); err != nil || settingsErr != nil {
		now := time.Now()
		if startErr := coord.StartRun(now); startErr == nil {
			recordUploadFinish(ctx, coord, checkpoint.UploadResult{StartedAt: now, FinishedAt: time.Now(),
				Error: "settings could not be loaded, so nothing was uploaded"})
		}
		return
	}

	// A pass that leaves refs queued ends the run (the next push retries them)
	// unless a push asked for more meanwhile; passes are bounded either way.
	for range checkpointUploadMaxPasses {
		release, ok, lockErr := coord.TryLockWorker(ctx)
		if lockErr != nil || !ok {
			return // The holder starts a worker for any request on release.
		}
		req, takeErr := coord.TakeRequest()
		if takeErr != nil || req == nil {
			release()
			return
		}
		runCheckpointUploadPass(ctx, repo, coord, *req)
		release()
		if !coord.HasRequest() {
			return
		}
	}
}

// runCheckpointUploadPass uploads the queue for req and records the outcome.
func runCheckpointUploadPass(ctx context.Context, repo *git.Repository, coord *checkpoint.UploadCoordinator, req checkpoint.UploadRequest) {
	result := checkpoint.UploadResult{StartedAt: time.Now()}
	if err := coord.StartRun(result.StartedAt); err != nil {
		logging.Warn(ctx, "record checkpoint upload start failed", slog.String("error", err.Error()))
	}
	finish := func(errLine string) {
		result.FinishedAt = time.Now()
		result.Error = errLine
		recordUploadFinish(ctx, coord, result)
	}

	// Lines the hook path writes to the user (the capture announcement, the
	// ignored-checkpoint_remote warning) have no terminal here: collect them
	// for the next push to show.
	var printed bytes.Buffer
	restore := stderrWriter
	stderrWriter = &printed
	defer func() { stderrWriter = restore }()

	ctx = withCarriedOPFDecision(ctx, req.OPFDecision)
	ps := resolvePushSettings(ctx, req.Remote)
	if ps.pushDisabled {
		finish("")
		return
	}
	if !ps.hasCheckpointURL() && !checkpointSyncAllowedForRemote(ctx, ps.remote, req.PendingCapture) {
		finish("checkpoints no longer sync to " + ps.remote)
		return
	}
	// The hook decided OPF must run, but this process sees it off (another
	// worktree's settings, or a scanner that failed to configure): uploading
	// now would ship exactly what the user asked to have scanned.
	if req.OPFDecision == checkpoint.UploadOPFRun && !redact.OPFEnabled() {
		finish("the privacy filter could not run here, so nothing was uploaded")
		return
	}
	if opfErr := opfGateForCheckpointRefs(ctx, repo); opfErr != nil {
		logging.Warn(ctx, "background checkpoint upload withheld", slog.String("error", opfErr.Error()))
		finish("withheld before upload (see .entire/logs)")
		return
	}

	deliverCtx, cancel := context.WithTimeout(ctx, checkpointUploadDeliveryBudget)
	defer cancel()
	res, err := flushCheckpointRefsQueue(deliverCtx, repo, ps, flushOptions{
		budget: checkpointUploadDeliveryBudget, chunkTimeout: checkpointUploadChunkTimeout})
	if err != nil {
		logging.Warn(ctx, "background checkpoint upload: refs left queued", slog.String("error", err.Error()))
	}
	if res.pushed > 0 {
		if req.PendingCapture != "" {
			commitCapturedSyncRemote(ctx, req.PendingCapture)
		}
		warnIgnoredCheckpointRemote(ctx, ps)
	}
	result.Pushed, result.Remaining = res.pushed, res.remaining
	result.Announcement = strings.TrimSpace(printed.String())
	finish(res.failureLine(err))
}

func recordUploadFinish(ctx context.Context, coord *checkpoint.UploadCoordinator, res checkpoint.UploadResult) {
	if err := coord.FinishRun(res); err != nil {
		logging.Warn(ctx, "record checkpoint upload result failed", slog.String("error", err.Error()))
	}
}

// unsetRepoOverrideEnv removes the git repo-selector variables
// gitrepo.EnvWithoutRepoOverrides filters, from this process's environment.
func unsetRepoOverrideEnv() {
	keep := make(map[string]bool)
	for _, kv := range gitrepo.EnvWithoutRepoOverrides() {
		keep[kv] = true
	}
	for _, kv := range os.Environ() {
		if keep[kv] {
			continue
		}
		if name, _, ok := strings.Cut(kv, "="); ok {
			os.Unsetenv(name)
		}
	}
}

// CheckpointUploadStatus reports, for `entire status`, whether a background
// checkpoint upload is running and why the last one stopped short ("" when it
// did not). Best-effort: any error reads as no background upload.
func CheckpointUploadStatus(ctx context.Context) (running bool, lastError string) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return false, ""
	}
	defer repo.Close()
	coord, err := checkpoint.UploadCoordinatorForRepo(repo)
	if err != nil {
		return false, ""
	}
	st, err := coord.State()
	if err != nil || st.Last == nil {
		return false, ""
	}
	running = st.Last.Running(time.Now(), checkpointUploadWorkerDeadline)
	// A worker that was killed leaves a record that still reads as running,
	// but the kernel freed its lock: a lock we can take means nobody uploads.
	if running {
		if release, ok, lockErr := coord.TryLockWorker(ctx); lockErr == nil && ok {
			release()
			running = false
		}
	}
	if running {
		return true, ""
	}
	return false, st.Last.Error
}
