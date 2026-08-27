package strategy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/go-git/go-git/v6"
)

// Entity deltas record WHICH code entities (functions, classes, types) changed
// between a session's base commit and the commit being checkpointed, so the
// entity-level view of a session travels with the checkpoint itself.
//
// GATE (read in scheduleEntityDeltas and again in the child):
// settings.IsEntityDeltasEnabled — the `entity_deltas` settings key with a
// tri-state ENTIRE_ENTITY_DELTAS override. Off by default.
//
// ASYNC BY CONSTRUCTION. Condensation runs on the post-commit hook path, and
// for agents that declare a session-end budget (Codex: 3s) it also runs inside
// that budget. A real producer run costs seconds and grows with the range, so
// it can never sit on that path. It doesn't:
//
//	condense ─▶ checkpoint written ─▶ spawn detached `entire __entity_deltas`
//	                                        │   (the hook returns HERE)
//	                                        ▼
//	                            run the producer (120s budget)
//	                            filter to the session's own files
//	                            SessionEntityDeltas backfill onto the checkpoint
//
// The hook's only added cost is writing a small job file and forking. The
// backfill is a different process writing a later commit onto an
// already-committed checkpoint, so every failure mode — producer absent,
// wedged, oversized, unparseable, or a store write that errors — is logged and
// dropped and CANNOT fail, alter, or delay checkpoint creation. That is
// structural rather than a promise: the scheduler returns nothing, so there is
// no value the condense path could act on and no failure it could inherit.
//
// The producer is an out-of-process binary looked up on $PATH under the CLI's
// kubectl-style plugin naming (`entire-<name>`, see cmd/entire/cli/plugin.go).

const (
	// entityDeltasSchemaVersion is the frozen schema version of the document.
	entityDeltasSchemaVersion = "1.0"

	// entityDeltasProducer names the tool the deltas came from, and — because
	// the CLI resolves plugins as `entire-<name>` on $PATH — is also the plugin
	// binary's name.
	entityDeltasProducer = "entire-graph"

	// entityDeltasWaitDelay bounds how long we wait for a killed child's pipes
	// to close, so a wedged grandchild cannot hold the backfill open past the
	// timeout. Mirrors the plugin runner's WaitDelay.
	entityDeltasWaitDelay = 2 * time.Second

	// EntityDeltasCommandName is the hidden CLI subcommand the detached
	// backfill child re-enters through. Exported so root.go can register it.
	EntityDeltasCommandName = "__entity_deltas"

	// entityDeltasJobVersion is the current job-file format version. Bump it
	// when a field's meaning changes; an older child then refuses the job
	// instead of misreading it.
	entityDeltasJobVersion = 1
)

// entityDeltasTimeout is the hard deadline for the producer subprocess. It is
// generous because nothing waits on it: the hook has already returned and the
// checkpoint already exists by the time this runs. A var, not a const, so tests
// can shorten the deadline instead of sleeping through it.
var entityDeltasTimeout = 120 * time.Second

// entityDeltasMaxOutputBytes caps the producer output we are willing to turn
// into a document. These documents are committed to the checkpoint branch and
// never garbage-collected, so an accidentally huge range (a 200-commit
// base..head was measured at 2.1MB) must be dropped rather than stored forever.
// A var, not a const, so tests can shrink it instead of generating 8MB.
var entityDeltasMaxOutputBytes = 8 << 20

// entityDeltasSpawn is the process-spawn seam used by scheduleEntityDeltas.
// Swapped in tests, which cannot fork the real child: execx.SpawnDetached is a
// no-op under `go test`, and a test binary would not understand the subcommand
// anyway. Production always uses spawnDetachedEntityDeltas.
var entityDeltasSpawn = spawnDetachedEntityDeltas

// errEntityDeltasOversized marks the one skip reason worth an Info line in the
// child: the producer emitted more than the cap. It is rare, it means a whole
// commit's deltas were dropped, and the fix (narrow the range) is the user's.
var errEntityDeltasOversized = errors.New("entity deltas producer output too large")

// entityDeltasDocument is the frozen on-disk schema of entity_deltas.json.
type entityDeltasDocument struct {
	SchemaVersion   string        `json:"schema_version"`
	Producer        string        `json:"producer"`
	ProducerVersion string        `json:"producer_version,omitempty"`
	Base            string        `json:"base"`
	Head            string        `json:"head"`
	ComputedAt      string        `json:"computed_at"`
	Entities        []entityDelta `json:"entities"`
}

// entityDelta is one changed code entity. Field names are frozen.
type entityDelta struct {
	Change       string `json:"change"` // added|removed|modified|renamed|moved
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	OldName      string `json:"old_name,omitempty"`
	Path         string `json:"path"`
	OldPath      string `json:"old_path,omitempty"`
	Signature    string `json:"signature,omitempty"`
	OldSignature string `json:"old_signature,omitempty"`
	StartLine    int    `json:"start_line"`
	Fingerprint  string `json:"fingerprint,omitempty"`
}

// graphDiffResult decodes the two fields of the producer's `diff --json`
// output that the frozen schema needs: `producer_version` and `files`. The
// producer's other top-level keys (base, head, schema_version) are
// intentionally left undecoded — this type ignores them, not just unknown
// future keys. See testdata/entity_deltas_producer_diff.json, captured from
// the real binary, so the two decoded field names cannot drift again.
type graphDiffResult struct {
	// ProducerVersion is the producer binary's own version. It is optional:
	// producers built without a stamped version omit it, and the document then
	// omits it too.
	ProducerVersion string            `json:"producer_version"`
	Files           []graphFileChange `json:"files"`
}

type graphFileChange struct {
	Path    string              `json:"path"`
	OldPath string              `json:"old_path"`
	Changes []graphEntityChange `json:"changes"`
}

type graphEntityChange struct {
	Type         string `json:"type"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	OldName      string `json:"old_name"`
	NewName      string `json:"new_name"`
	OldSignature string `json:"old_signature"`
	NewSignature string `json:"new_signature"`
	OldPath      string `json:"old_path"`
	NewPath      string `json:"new_path"`
	// BeforeStartLine is where the entity was before the change; the producer
	// emits it for anything that existed at base, and it is the ONLY line a
	// removal carries.
	BeforeStartLine int `json:"before_start_line"`
	AfterStartLine  int `json:"after_start_line"`
}

// entityDeltasJob is the unit of work handed to the detached child. It travels
// through a temp file rather than argv because the session's file list is
// unbounded and a single argv element is capped (128KiB on Linux); the child
// removes the file as soon as it has read it.
type entityDeltasJob struct {
	// V is the job-file format version. The writer and the reader are two
	// different builds of the CLI whenever an upgrade lands between the fork
	// and the exec, so the child checks it and refuses a format it does not
	// know rather than acting on fields it may be misreading. Absent (0) is the
	// unversioned original and is read as v1.
	V            int             `json:"v"`
	CheckpointID id.CheckpointID `json:"checkpoint_id"`
	SessionID    string          `json:"session_id"`
	RepoDir      string          `json:"repo_dir"`
	Base         string          `json:"base"`
	Head         string          `json:"head"`
	// Files is the session's own file set — WriteOptions.FilesTouched, i.e. the
	// post-filterFilesTouched list attribution scopes this session by. The
	// producer diffs the whole base..head range, so this is what separates this
	// session's entities from the other sessions' and the human's.
	Files []string `json:"files"`
}

// scheduleEntityDeltas hands this session's entity-delta computation to a
// detached child and returns. It is called AFTER the checkpoint has been
// written, and returns nothing: by construction there is no result the caller
// could act on and no failure it could inherit.
//
// base is the session base the caller also attributes against, so the deltas
// describe exactly the range attribution describes.
func scheduleEntityDeltas(ctx, logCtx context.Context, repo *git.Repository, o condenseOpts, checkpointID id.CheckpointID, sessionID, base string, files []string) {
	if !settings.IsEntityDeltasEnabled(ctx) {
		return
	}

	head := o.headCommitHash
	if head == "" {
		headRef, err := repo.Head()
		if err != nil {
			logging.Debug(logCtx, "entity deltas skipped: HEAD unavailable",
				slog.String("error", err.Error()))
			return
		}
		head = headRef.Hash().String()
	}
	if base == "" || head == "" || base == head {
		return
	}

	// A session that touched no files can own no entity in the range, so the
	// producer would be run only to have everything filtered back out.
	if len(files) == 0 {
		logging.Debug(logCtx, "entity deltas skipped: session touched no files",
			slog.String("session_id", sessionID))
		return
	}

	repoDir := o.repoDir
	if repoDir == "" {
		root, err := paths.WorktreeRoot(ctx)
		if err != nil {
			logging.Debug(logCtx, "entity deltas skipped: worktree root unavailable",
				slog.String("error", err.Error()))
			return
		}
		repoDir = root
	}

	// The one Info line. Every other skip is Debug, but the default log level is
	// Info: without this, turning the gate on with the producer missing looked
	// like the feature silently working forever.
	if _, err := exec.LookPath(entityDeltasProducer); err != nil {
		logging.Info(logCtx, "entity deltas enabled but the producer is unavailable; no deltas will be recorded",
			slog.String("producer", entityDeltasProducer),
			slog.String("error", err.Error()))
		return
	}

	jobPath, err := writeEntityDeltasJob(entityDeltasJob{
		V:            entityDeltasJobVersion,
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		RepoDir:      repoDir,
		Base:         base,
		Head:         head,
		Files:        files,
	})
	if err != nil {
		logging.Debug(logCtx, "entity deltas skipped: job file unwritable",
			slog.String("error", err.Error()))
		return
	}

	// The job file is the child's to consume, so it is removed here ONLY when
	// there is no child: a spawn that failed leaves a file nobody will ever
	// read, and the temp reaper is not a cleanup strategy we should rely on
	// when we already know the outcome.
	if err := entityDeltasSpawn(repoDir, jobPath); err != nil {
		_ = os.Remove(jobPath)
		logging.Debug(logCtx, "entity deltas skipped: could not spawn the backfill child",
			slog.String("error", err.Error()))
	}
}

// writeEntityDeltasJob serializes a job into a temp file for the detached
// child. On any failure here nothing is left behind, and a spawn that fails
// outright takes the file with it (see scheduleEntityDeltas); only a child that
// started and then died before reading leaves one small file behind, which is
// the OS temp reaper's problem and not worth blocking the hook over.
func writeEntityDeltasJob(job entityDeltasJob) (string, error) {
	payload, err := json.Marshal(job)
	if err != nil {
		return "", fmt.Errorf("encode entity deltas job: %w", err)
	}
	f, err := os.CreateTemp("", "entire-entity-deltas-*.json")
	if err != nil {
		return "", fmt.Errorf("create entity deltas job file: %w", err)
	}
	name := f.Name()
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("write entity deltas job file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("close entity deltas job file: %w", err)
	}
	return name, nil
}

// spawnDetachedEntityDeltas starts `entire __entity_deltas <job>` as a detached
// child so the producer run and the backfill commit add nothing to the
// post-commit hook. The child runs from the worktree root because the checkpoint
// store, settings, and logging all resolve from the working directory.
func spawnDetachedEntityDeltas(repoDir, jobPath string) error {
	//nolint:wrapcheck // the caller only distinguishes spawned from not-spawned
	return execx.SpawnDetachedErr(repoDir, EntityDeltasCommandName, jobPath)
}

// RunEntityDeltasBackfill executes one entity-delta job. It is the body of the
// hidden `__entity_deltas` command, invoked only from the detached child that
// scheduleEntityDeltas forked.
//
// It returns nothing, deliberately. The child has no terminal, its
// stdout/stderr are discarded, and nothing waits on its exit status — a
// non-zero exit would signal nothing to nobody — so every failure is recorded
// in the repo's log and swallowed (same reasoning as the detached
// trails-enablement refresh, which returns nil on its failure paths for exactly
// this reason).
func RunEntityDeltasBackfill(ctx context.Context, jobPath string) {
	job, err := readEntityDeltasJob(jobPath)
	if err != nil {
		logging.Debug(ctx, "entity deltas backfill skipped: job unreadable",
			slog.String("job_path", jobPath),
			slog.String("error", err.Error()))
		return
	}
	runEntityDeltasBackfill(ctx, job)
}

// readEntityDeltasJob loads and consumes the job file. The file is removed
// whether or not it decodes, so a malformed job cannot accumulate in temp.
func readEntityDeltasJob(jobPath string) (entityDeltasJob, error) {
	raw, err := os.ReadFile(jobPath) //nolint:gosec // path comes from this process's own scheduler
	// Remove before deciding: the job is single-use either way.
	_ = os.Remove(jobPath)
	if err != nil {
		return entityDeltasJob{}, fmt.Errorf("read entity deltas job: %w", err)
	}
	var job entityDeltasJob
	if err := json.Unmarshal(raw, &job); err != nil {
		return entityDeltasJob{}, fmt.Errorf("decode entity deltas job: %w", err)
	}
	return job, nil
}

// runEntityDeltasBackfill computes the deltas and attaches them to the already
// written checkpoint. Every exit is a log line and a return: the checkpoint is
// committed before this runs and must stay exactly as it is.
func runEntityDeltasBackfill(ctx context.Context, job entityDeltasJob) {
	// Re-read the gate in the child: settings may have been edited between the
	// fork and the exec, and this is the process that would do the writing.
	if !settings.IsEntityDeltasEnabled(ctx) {
		return
	}
	// An upgrade between the fork and the exec makes the writer and the reader
	// different builds. A version this build does not know means the fields
	// below may not mean what they used to, so refuse rather than write
	// something wrong into a committed checkpoint. Absent (0) is the original
	// unversioned job and reads as v1.
	if job.V != 0 && job.V != entityDeltasJobVersion {
		logging.Info(ctx, "entity deltas backfill skipped: unsupported job version",
			slog.Int("job_version", job.V),
			slog.Int("supported_version", entityDeltasJobVersion))
		return
	}
	if job.CheckpointID.IsEmpty() || job.SessionID == "" || job.RepoDir == "" {
		logging.Debug(ctx, "entity deltas backfill skipped: incomplete job")
		return
	}

	doc, err := computeEntityDeltas(ctx, job.RepoDir, job.Base, job.Head, entityDeltasFileSet(job.Files))
	if err != nil {
		logEntityDeltasSkip(ctx, job, err)
		return
	}

	document, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		logging.Debug(ctx, "entity deltas skipped: document unencodable",
			slog.String("error", err.Error()))
		return
	}
	document = append(document, '\n')

	repo, err := gitrepo.OpenPath(job.RepoDir)
	if err != nil {
		logging.Debug(ctx, "entity deltas skipped: repository unavailable",
			slog.String("repo_dir", job.RepoDir),
			slog.String("error", err.Error()))
		return
	}
	store, err := (&ManualCommitStrategy{}).getPersistentStore(ctx, repo)
	if err != nil {
		logging.Debug(ctx, "entity deltas skipped: checkpoint store unavailable",
			slog.String("error", err.Error()))
		return
	}

	// A commit that condenses N sessions forks N children, and on the
	// git-branch backend they all rewrite the same ref (on git-refs, the same
	// per-checkpoint ref). Without serializing, each would read the same base
	// tree and the last writer would silently drop the others' documents.
	// Nobody is waiting on this process, so blocking here is free.
	//
	// The pre-push OPF rewrite takes the SAME lock for its duration, so a
	// backfill can never land between that rewrite's tip read and its
	// compare-and-swap — which would abort the user's push. Parking here (and
	// dropping the document if the wait runs out) is the cheap side of that
	// trade: nobody is waiting on this process.
	release, err := acquireEntityDeltasLock(ctx, job.RepoDir)
	if err != nil {
		logging.Debug(ctx, "entity deltas skipped: could not serialize the backfill write",
			slog.String("error", err.Error()))
		return
	}
	defer release()

	if err := store.Write(ctx, cpkg.SessionEntityDeltas{
		CheckpointID: job.CheckpointID,
		SessionID:    job.SessionID,
		Document:     document,
	}); err != nil {
		// The checkpoint is already committed; this write only adds a file to
		// it. Losing that file is strictly better than any louder outcome.
		logging.Debug(ctx, "entity deltas backfill write failed",
			slog.String("checkpoint_id", job.CheckpointID.String()),
			slog.String("session_id", job.SessionID),
			slog.String("error", err.Error()))
		return
	}

	logging.Debug(ctx, "entity deltas recorded",
		slog.String("checkpoint_id", job.CheckpointID.String()),
		slog.String("session_id", job.SessionID),
		slog.String("base", job.Base),
		slog.String("head", job.Head),
		slog.Int("entities", len(doc.Entities)))
}

// logEntityDeltasSkip records why a job produced nothing. Everything is Debug
// except the oversize case, which drops a whole commit's deltas for a reason
// only the user can act on.
func logEntityDeltasSkip(ctx context.Context, job entityDeltasJob, err error) {
	attrs := []any{
		slog.String("base", job.Base),
		slog.String("head", job.Head),
		slog.String("error", err.Error()),
	}
	if errors.Is(err, errEntityDeltasOversized) {
		logging.Info(ctx, "entity deltas dropped: producer output exceeded the cap", attrs...)
		return
	}
	logging.Debug(ctx, "entity deltas skipped", attrs...)
}

// entityDeltasLockFile is the per-repo lock file (in the shared git dir, so
// every worktree of the repo contends on the same one) serializing
// entity-delta backfill writes.
const entityDeltasLockFile = "entire-entity-deltas.lock"

// entityDeltasLockWait bounds the wait for the backfill lock. It is generous
// because the holder is another backfill doing a small tree write (or a
// pre-push OPF rewrite the backfill must not land inside of), and it is
// bounded at all only so a pathological pile-up drops work instead of
// accumulating stuck children. A var, not a const, so tests can shrink it
// instead of sleeping through it.
var entityDeltasLockWait = 2 * time.Minute

// entityDeltasLockPath is the backfill lock's absolute path for a worktree.
//
// It resolves the shared git dir FROM THE WORKTREE PATH rather than from the
// process's working directory, because the two processes that contend here —
// the detached backfill child and the pre-push OPF rewrite — must agree on the
// path byte for byte or the lock excludes nobody.
func entityDeltasLockPath(ctx context.Context, worktreePath string) (string, error) {
	commonDir, err := gitdir.CommonDirForWorktree(ctx, worktreePath)
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	return filepath.Join(commonDir, entityDeltasLockFile), nil
}

// acquireEntityDeltasLock serializes backfill writes across the detached
// children of one commit (and across worktrees of one repo), and excludes them
// from the pre-push OPF rewrite's v1 rebuild — see acquireEntityDeltasLockFor.
func acquireEntityDeltasLock(ctx context.Context, worktreePath string) (func(), error) {
	return acquireEntityDeltasLockFor(ctx, worktreePath, entityDeltasLockWait)
}

// entityDeltasRewriteLockWait bounds how long the pre-push OPF rewrite waits
// for the backfill lock. Short, because a user is watching a push: the holder
// is a backfill child whose critical section is opening the repo and writing
// one small tree, so this is orders of magnitude past the real hold time and
// exists only so a wedged child cannot stall a push. Timing out is harmless —
// the rewrite proceeds unlocked, exactly as it did before. A var, not a const,
// for the same reason as entityDeltasLockWait.
var entityDeltasRewriteLockWait = 30 * time.Second

// lockOutEntityDeltasBackfills takes the entity-deltas backfill lock on repo's
// behalf, so no detached child can advance v1 while the caller holds it.
//
// Callers must treat a failure as advisory (log and continue): this keeps a
// backfill from turning into a push abort, but it is not what makes the write
// correct — the compare-and-swap on each side is.
func lockOutEntityDeltasBackfills(ctx context.Context, repo *git.Repository) (func(), error) {
	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("resolve worktree for the entity deltas lock: %w", err)
	}
	return acquireEntityDeltasLockFor(ctx, wt.Filesystem().Root(), entityDeltasRewriteLockWait)
}

// acquireEntityDeltasLockFor is acquireEntityDeltasLock with an explicit
// bound, so a caller that is on a user-visible path (the pre-push rewrite) can
// wait for a much shorter time than a caller nobody is waiting on.
func acquireEntityDeltasLockFor(ctx context.Context, worktreePath string, wait time.Duration) (func(), error) {
	path, err := entityDeltasLockPath(ctx, worktreePath)
	if err != nil {
		return nil, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	release, err := flock.AcquireContext(lockCtx, path)
	if err != nil {
		return nil, fmt.Errorf("acquire entity deltas lock: %w", err)
	}
	return release, nil
}

// entityDeltasFileSet normalizes a session's file list into a lookup set.
// Producer paths are always forward-slashed, so the session's are too.
func entityDeltasFileSet(files []string) map[string]struct{} {
	set := make(map[string]struct{}, len(files))
	for _, f := range files {
		if f == "" {
			continue
		}
		set[filepath.ToSlash(f)] = struct{}{}
	}
	return set
}

// computeEntityDeltas runs the producer over base..head in repoDir and maps its
// output onto the frozen schema, keeping only entities in sessionFiles. Every
// error path is the caller's silent skip.
func computeEntityDeltas(ctx context.Context, repoDir, base, head string, sessionFiles map[string]struct{}) (*entityDeltasDocument, error) {
	bin, err := exec.LookPath(entityDeltasProducer)
	if err != nil {
		return nil, fmt.Errorf("%s not on PATH: %w", entityDeltasProducer, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, entityDeltasTimeout)
	defer cancel()

	// The producer is normally reached as `entire graph diff …`; the plugin
	// dispatcher strips the plugin name before exec, so the binary's own argv
	// starts at the verb. --repo and the working directory are both set because
	// the producer resolves the repo from either.
	cmd := exec.CommandContext(runCtx, bin, "diff",
		"--base", base, "--head", head, "--repo", repoDir, "--json")
	cmd.Dir = repoDir
	cmd.WaitDelay = entityDeltasWaitDelay
	// The producer may fork workers of its own; without a process group the
	// deadline would kill only the producer and leave them running.
	killEntityDeltasProcessGroupOnCancel(cmd)

	stdout := &cappedBuffer{limit: entityDeltasMaxOutputBytes}
	stderr := &cappedBuffer{limit: entityDeltasMaxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s diff timed out after %s", entityDeltasProducer, entityDeltasTimeout)
		}
		return nil, fmt.Errorf("%s diff failed: %w (stderr: %s)", entityDeltasProducer, err, truncateForLog(string(stderr.Bytes())))
	}
	if stdout.overflowed {
		return nil, fmt.Errorf("%w: more than %d bytes; skipping rather than committing it to the checkpoint branch",
			errEntityDeltasOversized, entityDeltasMaxOutputBytes)
	}

	var result graphDiffResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("decode %s diff output: %w", entityDeltasProducer, err)
	}

	return &entityDeltasDocument{
		SchemaVersion:   entityDeltasSchemaVersion,
		Producer:        entityDeltasProducer,
		ProducerVersion: result.ProducerVersion,
		Base:            base,
		Head:            head,
		ComputedAt:      time.Now().UTC().Format(time.RFC3339),
		Entities:        entityDeltasFromDiff(result, sessionFiles),
	}, nil
}

// cappedBuffer collects at most limit bytes and records that more arrived.
// Writes past the limit are discarded instead of erroring, so the producer
// finishes normally and the oversize decision stays out of the
// process-failure error path.
type cappedBuffer struct {
	limit      int
	buf        bytes.Buffer
	overflowed bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) <= remaining {
			//nolint:wrapcheck // bytes.Buffer.Write never returns an error
			return b.buf.Write(p)
		}
		_, _ = b.buf.Write(p[:remaining])
	}
	b.overflowed = true
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }

// entityDeltasFromDiff flattens the producer's per-file change lists into the
// frozen flat entity list, keeping only entities that belong to the session's
// own files. The producer diffs the raw base..head range, which in a
// multi-session commit also contains the other sessions' and the human's work;
// scoping by the session's file set is exactly how attribution separates them.
// Never nil, so entity_deltas.json always carries an `entities` array — an empty
// one meaning "no entity-level change of this session's".
func entityDeltasFromDiff(result graphDiffResult, sessionFiles map[string]struct{}) []entityDelta {
	entities := make([]entityDelta, 0)
	for _, file := range result.Files {
		for _, change := range file.Changes {
			delta := entityDeltaFromChange(file, change)
			if !entityDeltaInSession(delta, sessionFiles) {
				continue
			}
			entities = append(entities, delta)
		}
	}
	return entities
}

// entityDeltaInSession reports whether a delta belongs to the session's files.
// Either end of a move counts: a symbol the session moved out of one of its
// files is the session's work even though it now lives elsewhere.
func entityDeltaInSession(delta entityDelta, sessionFiles map[string]struct{}) bool {
	if _, ok := sessionFiles[delta.Path]; ok {
		return true
	}
	if delta.OldPath == "" {
		return false
	}
	_, ok := sessionFiles[delta.OldPath]
	return ok
}

func entityDeltaFromChange(file graphFileChange, change graphEntityChange) entityDelta {
	name := change.NewName
	if name == "" {
		name = change.Name
	}
	path := change.NewPath
	if path == "" {
		path = file.Path
	}
	// The producer emits after_start_line only for entities that exist after the
	// change, so a removal carries only before_start_line. Falling back to it is
	// the difference between recording "line 0" and the line the entity actually
	// occupied when it was deleted.
	startLine := change.AfterStartLine
	if startLine == 0 {
		startLine = change.BeforeStartLine
	}

	delta := entityDelta{
		Change:       normalizeEntityChange(change.Type),
		Kind:         change.Kind,
		Name:         name,
		Path:         path,
		Signature:    change.NewSignature,
		OldSignature: change.OldSignature,
		StartLine:    startLine,
	}
	// old_name answers "what was this called before" — a property of the change
	// carrying a different previous name, NOT of the change type: the producer
	// also sets old_name/new_name on a `moved` change when one edit both moved
	// and renamed the symbol.
	if change.OldName != "" && change.OldName != delta.Name {
		delta.OldName = change.OldName
	}
	// old_path answers "where was this before". The entity's own old_path wins;
	// when the producer emits none, the FILE's old_path still answers it,
	// because renaming a file relocates every entity inside it.
	switch {
	case change.OldPath != "" && change.OldPath != delta.Path:
		delta.OldPath = change.OldPath
	case file.OldPath != "" && file.OldPath != delta.Path:
		delta.OldPath = file.OldPath
	}
	return delta
}

// normalizeEntityChange folds the producer's change types into the frozen
// enum (added|removed|modified|renamed|moved). The producer distinguishes
// finer-grained in-place edits (signature_changed, body_changed); the schema
// has a single `modified`, which is also the safe reading of any change type a
// future producer adds.
func normalizeEntityChange(changeType string) string {
	switch changeType {
	case "added", "removed", "renamed", "moved":
		return changeType
	default:
		return "modified"
	}
}

// truncateForLog bounds producer stderr so a chatty failure cannot flood the
// log.
func truncateForLog(s string) string {
	const maxLen = 256
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}
