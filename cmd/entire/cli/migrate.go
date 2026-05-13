package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	agenttypes "github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/lockfile"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/transcript/compact"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/perf"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage"
	"github.com/spf13/cobra"
)

func newMigrateCmd() *cobra.Command {
	var checkpointsFlag string
	var forceFlag bool
	var dryRunFlag bool

	cmd := &cobra.Command{
		Use:    "migrate",
		Short:  "Migrate Entire data to newer formats",
		Long:   `Migrate Entire data to newer formats. Currently supports migrating v1 checkpoints to v2.`,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if checkpointsFlag == "" {
				return cmd.Help()
			}
			if checkpointsFlag != "v2" {
				return fmt.Errorf("unsupported checkpoints version: %q (only \"v2\" is supported)", checkpointsFlag)
			}
			if dryRunFlag && forceFlag {
				return errors.New("--dry-run and --force cannot be combined")
			}

			ctx := cmd.Context()

			if _, err := paths.WorktreeRoot(ctx); err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run from within a git repository.")
				return NewSilentError(errors.New("not a git repository"))
			}

			release, err := acquireCommandLock(ctx, cmd, "entire-migrate.lock", "migrate")
			if err != nil {
				return err
			}

			logging.SetLogLevelGetter(GetLogLevel)
			if initErr := logging.Init(ctx, ""); initErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not initialize logging: %v\n", initErr)
			} else {
				defer logging.Close()
			}
			defer release()

			if dryRunFlag {
				return runMigrateCheckpointsV2DryRun(ctx, cmd)
			}

			return runMigrateCheckpointsV2(ctx, cmd, forceFlag)
		},
	}

	cmd.Flags().StringVar(&checkpointsFlag, "checkpoints", "", "Target checkpoint format version (e.g., \"v2\")")
	cmd.Flags().BoolVar(&forceFlag, "force", false, "Force re-migration of all checkpoints, overwriting existing v2 data")
	cmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "List v1 checkpoints not yet in v2 without migrating")

	return cmd
}

// acquireCommandLock takes <git-common-dir>/<lockFile> as a per-command
// exclusive lock. On contention it prints a message to stderr and returns a
// SilentError. Other setup failures return regular errors so main.go prints
// them. Defer release() after logging.Init so a release error can still be
// warned (LIFO defer order).
func acquireCommandLock(ctx context.Context, cmd *cobra.Command, lockFile, opName string) (release func(), err error) {
	commonDir, err := strategy.GetGitCommonDir(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		return nil, fmt.Errorf("resolve git common dir: %w", err)
	}
	lockPath := filepath.Join(commonDir, lockFile)

	lk, err := lockfile.Acquire(lockPath)
	if err != nil {
		if errors.Is(err, lockfile.ErrLocked) {
			cmd.SilenceUsage = true
			pidStr := "unknown"
			if holder := lockfile.ReadHolderPID(lockPath); holder > 0 {
				pidStr = strconv.Itoa(holder)
			}
			fmt.Fprintf(cmd.ErrOrStderr(),
				"another `entire %s` is already running (PID %s, lock at %s); refusing to start a second instance\n",
				opName, pidStr, lockPath)
			return nil, NewSilentError(fmt.Errorf("%s already in progress", opName))
		}
		cmd.SilenceUsage = true
		return nil, fmt.Errorf("acquire %s lock: %w", opName, err)
	}

	return func() {
		if relErr := lk.Release(); relErr != nil {
			logging.Warn(ctx, "failed to release command lock",
				slog.String("op", opName),
				slog.String("error", relErr.Error()))
		}
	}, nil
}

type migrateResult struct {
	total                    int
	migrated                 int
	skipped                  int
	failed                   int
	missingSessions          int
	compactTranscriptSkipped int
}

func runMigrateCheckpointsV2(ctx context.Context, cmd *cobra.Command, force bool) error {
	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run from within a git repository.")
		return NewSilentError(err)
	}

	v1Store := checkpoint.NewGitStore(repo)
	v2Store := checkpoint.NewV2GitStore(repo, migrateRemoteName)
	out := cmd.OutOrStdout()
	progressOut := cmd.ErrOrStderr()

	// Root perf span emits a single `perf` log entry on End() with the full
	// timing tree. Inspect via `entire doctor trace --hook migrate_checkpoints`
	// (requires log_level: DEBUG in .entire/settings.json or ENTIRE_LOG_LEVEL=DEBUG).
	ctx, rootSpan := perf.Start(ctx, "migrate_checkpoints")
	defer rootSpan.End()

	result, freshlyPackedRefs, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, progressOut, force)
	if err != nil {
		rootSpan.RecordError(err)
		return err
	}

	// Skip the generation-metadata repair pass on no-op reruns: it does a
	// `git ls-remote` plus a transcript-blob walk per archived /full/<n>,
	// minutes on big repos. When we did write archives, freshly-packed refs
	// are excluded — their generation.json is already correct from the
	// checkpoint metadata already loaded by the packer.
	var repairResult *strategy.RepairV2GenerationMetadataResult
	if len(freshlyPackedRefs) > 0 {
		stopRepair := startSpinner(cmd.ErrOrStderr(), "Repairing archived generation metadata")
		_, repairSpan := perf.Start(ctx, "repair_generation_metadata")
		var repairErr error
		repairResult, repairErr = strategy.RepairV2GenerationMetadata(ctx, freshlyPackedRefs)
		if repairErr != nil {
			repairSpan.RecordError(repairErr)
		}
		repairSpan.End()
		if repairErr != nil {
			stopRepair(false)
			return fmt.Errorf("failed to repair archived v2 generation metadata: %w", repairErr)
		}
		stopRepair(true)
		printV2GenerationRepairResult(out, cmd.ErrOrStderr(), repairResult)
	}

	printMigrateCompletion(out, result)
	fmt.Fprintln(out, "Note: V2 checkpoints are stored as custom refs under refs/entire/checkpoints/v2/*, not as a branch visible in the GitHub UI.")
	fmt.Fprintf(out, "To inspect pushed v2 checkpoint refs locally, run: git ls-remote %s \"refs/entire/checkpoints/v2/*\"\n", migrateRemoteName)
	fmt.Fprintln(out, `You may also open a checkpoint's details in the Entire web app and click the "session logs" link to view the log files and metadata.`)

	if result.failed > 0 {
		return NewSilentError(fmt.Errorf("%d checkpoint(s) failed to migrate", result.failed))
	}
	if repairResult != nil && len(repairResult.Failed) > 0 {
		fmt.Fprintf(out, "%d archived generation(s) failed metadata repair. Check warnings above for details.\n", len(repairResult.Failed))
		return NewSilentError(fmt.Errorf("%d archived generation(s) failed metadata repair", len(repairResult.Failed)))
	}

	return nil
}

// runMigrateCheckpointsV2DryRun reports v1 checkpoints that have no matching
// entry on the v2 /main ref. It performs no writes to either checkpoint store
// (only stdout is touched) and exits zero on success; only setup or git
// failures produce a non-zero exit.
func runMigrateCheckpointsV2DryRun(ctx context.Context, cmd *cobra.Command) error {
	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run from within a git repository.")
		return NewSilentError(err)
	}

	v1Store := checkpoint.NewGitStore(repo)
	v2Store := checkpoint.NewV2GitStore(repo, migrateRemoteName)
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Already validated in RunE; treat any race here as "no SHA lookup".
		root = ""
	}

	_, _, err = dryRunCheckpointsV2(ctx, v1Store, v2Store, root, cmd.OutOrStdout())
	return err
}

// dryRunCheckpointsV2 inspects v1 vs v2 stores and prints a report. Returns
// (pendingCount, totalV1, error). The pending set is the same as the trigger
// for the post-push migration hint: any v1 checkpoint ID absent from v2's
// /main ref. Output goes to `out`; nothing is written to either store.
func dryRunCheckpointsV2(ctx context.Context, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, repoRoot string, out io.Writer) (int, int, error) {
	v1List, err := v1Store.ListCommitted(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to list v1 checkpoints: %w", err)
	}
	if len(v1List) == 0 {
		fmt.Fprintln(out, "No v1 checkpoints found.")
		return 0, 0, nil
	}

	v2List, err := v2Store.ListCommitted(ctx)
	if err != nil {
		return 0, len(v1List), fmt.Errorf("failed to list v2 checkpoints: %w", err)
	}
	v2Set := make(map[string]struct{}, len(v2List))
	for _, info := range v2List {
		v2Set[info.CheckpointID.String()] = struct{}{}
	}

	pending := make([]checkpoint.CommittedInfo, 0)
	for _, info := range v1List {
		if _, ok := v2Set[info.CheckpointID.String()]; !ok {
			pending = append(pending, info)
		}
	}

	if len(pending) == 0 {
		fmt.Fprintf(out, "All %d v1 checkpoints are already in v2. Nothing to migrate.\n", len(v1List))
		return 0, len(v1List), nil
	}

	sortMigratableCheckpoints(pending)

	fmt.Fprintf(out, "%d of %d v1 checkpoints not yet in v2:\n\n", len(pending), len(v1List))
	fmt.Fprintf(out, "  %-12s  %-16s  %s\n", "CHECKPOINT", "CREATED", "V1 COMMIT")
	firstID := pending[0].CheckpointID
	for _, info := range pending {
		commit := lookupV1CommitInfo(ctx, repoRoot, info.CheckpointID)
		when := "(unknown time)  "
		switch {
		case !info.CreatedAt.IsZero():
			when = info.CreatedAt.Local().Format("2006-01-02 15:04")
		case !commit.authorTime.IsZero():
			when = commit.authorTime.Local().Format("2006-01-02 15:04")
		}
		sha := "-------"
		if commit.shortHash != "" {
			sha = commit.shortHash
		}
		fmt.Fprintf(out, "  %-12s  %-16s  %s\n", info.CheckpointID, when, sha)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "To investigate, e.g.:")
	fmt.Fprintf(out, "  entire checkpoint explain %s\n", firstID)
	fmt.Fprintf(out, "  git show entire/checkpoints/v1:%s/%s\n", string(firstID[:2]), string(firstID[2:]))
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Run 'entire migrate --checkpoints v2' to migrate these.")

	return len(pending), len(v1List), nil
}

// v1CommitInfo carries the short hash and author time of the most recent
// commit on entire/checkpoints/v1 that touched a given checkpoint's folder.
// Either field can be zero/empty when the lookup fails.
type v1CommitInfo struct {
	shortHash  string
	authorTime time.Time
}

// lookupV1CommitInfo returns the most recent commit on entire/checkpoints/v1
// that touched the checkpoint's sharded folder. We use path-scoped log rather
// than --grep so older subject formats and trailers-only matches still
// resolve. Failure (branch absent, no commit, parse error) returns a zero
// value — callers fall back to placeholder rendering.
func lookupV1CommitInfo(ctx context.Context, repoRoot string, cpID id.CheckpointID) v1CommitInfo {
	if repoRoot == "" {
		return v1CommitInfo{}
	}
	pathArg := string(cpID[:2]) + "/" + string(cpID[2:]) + "/"
	gitCmd := exec.CommandContext(ctx, "git", "log", "entire/checkpoints/v1",
		"-n", "1", "--pretty=%h %aI", "--", pathArg)
	gitCmd.Dir = repoRoot
	output, err := gitCmd.Output()
	if err != nil {
		return v1CommitInfo{}
	}
	line := strings.TrimSpace(string(output))
	if line == "" {
		return v1CommitInfo{}
	}
	fields := strings.SplitN(line, " ", 2)
	if len(fields) == 0 {
		return v1CommitInfo{}
	}
	info := v1CommitInfo{shortHash: fields[0]}
	if len(fields) == 2 {
		if t, err := time.Parse(time.RFC3339, fields[1]); err == nil {
			info.authorTime = t
		}
	}
	return info
}

const migrationLogFile = logging.LogsDir + "/entire.log"

func printMigrateCompletion(out io.Writer, result *migrateResult) {
	if result.total == 0 {
		fmt.Fprintln(out, "Nothing to migrate: no v1 checkpoints found")
		fmt.Fprintln(out)
		return
	}

	fmt.Fprintf(out, "Migration complete: %d migrated, %d skipped, %d failed\n",
		result.migrated, result.skipped, result.failed)

	if result.hasLoggedDetails() {
		fmt.Fprintf(out, "Details for skipped, missing, incomplete, or failed checkpoints were logged to %s.\n", migrationLogFile)
	}

	fmt.Fprintln(out)
}

func (r *migrateResult) hasLoggedDetails() bool {
	return r.skipped > 0 || r.failed > 0 || r.missingSessions > 0 || r.compactTranscriptSkipped > 0
}

func printV2GenerationRepairResult(out, errOut io.Writer, result *strategy.RepairV2GenerationMetadataResult) {
	if result == nil {
		return
	}

	for _, warning := range result.Warnings {
		fmt.Fprintf(errOut, "Warning: %s\n", warning)
	}

	if len(result.Repaired) == 0 && len(result.Failed) == 0 {
		return
	}

	fmt.Fprintf(out, "Archived generation metadata repair: %d repaired, %d skipped, %d failed\n",
		len(result.Repaired), len(result.Skipped), len(result.Failed))
}

var (
	errAlreadyMigrated          = errors.New("already migrated")
	errTranscriptNotGeneratable = errors.New("transcript.jsonl could not be generated")
	errNoMigratableSessions     = errors.New("no migratable v1 sessions")
)

const (
	migrateRemoteName  = "origin"
	migrateAuthorName  = "Entire Migration"
	migrateAuthorEmail = "migration@entire.dev"
)

var migrateMaxCheckpointsPerGeneration = checkpoint.DefaultMaxCheckpointsPerGeneration

type migratedFullCheckpoint struct {
	checkpointID id.CheckpointID
	sessions     []migratedFullSession
	taskTrees    map[int][]plumbing.Hash
}

type migratedFullSession struct {
	sessionIndex            int
	content                 *checkpoint.SessionContent
	rawTranscriptBlobHashes []plumbing.Hash
}

// migrateCheckpointsV2 returns the /full/<n> refs migration wrote so callers
// can pass them as exclusions to the generation-metadata repair pass.
func migrateCheckpointsV2(ctx context.Context, repo *git.Repository, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, progressOut io.Writer, force bool) (*migrateResult, []plumbing.ReferenceName, error) {
	_, listSpan := perf.Start(ctx, "list_v1_checkpoints")
	v1List, err := v1Store.ListCommitted(ctx)
	if err != nil {
		listSpan.RecordError(err)
	}
	listSpan.End()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list v1 checkpoints: %w", err)
	}

	if len(v1List) == 0 {
		return &migrateResult{}, nil, nil
	}

	sortMigratableCheckpoints(v1List)
	total := len(v1List)
	result := &migrateResult{total: total}
	progress := startProgressBar(progressOut, "Migrating checkpoints", total)
	defer progress.Finish()

	_, fullCurrentRefErr := repo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	fullCurrentExistsBefore := fullCurrentRefErr == nil

	// One up-front tree walk to make per-session "are /full/* artifacts
	// present?" checks O(1) inside the migration loop.
	_, indexSpan := perf.Start(ctx, "build_full_artifacts_index")
	fullArtifactsIndex, err := v2Store.BuildFullSessionArtifactsIndex()
	if err != nil {
		indexSpan.RecordError(err)
	}
	indexSpan.End()
	if err != nil {
		return nil, nil, fmt.Errorf("build v2 /full/* presence index: %w", err)
	}

	existingV2, err := listExistingV2Checkpoints(ctx, repo, v2Store)
	if err != nil {
		return nil, nil, fmt.Errorf("list v2 checkpoints: %w", err)
	}

	batchSize := migrateFullBatchSize()
	state := newMigrateLoopState(batchSize)

	// Span around the migration loop. No per-iteration spans: at 4k+ checkpoints
	// the resulting attr count would blow past trace.go's 1MB scanner limit.
	_, processSpan := perf.Start(ctx, "process_checkpoints")

	for _, info := range v1List {
		existing, _, existingErr := readExistingV2Checkpoint(ctx, v2Store, existingV2, info.CheckpointID)
		if existingErr != nil {
			recordMigrationSkipOrFailure(ctx, result, info.CheckpointID, existingErr)
			progress.Increment()
			continue
		}

		fullCheckpoint, mainOpts, outcome, migrateErr := migrateOneCheckpoint(ctx, repo, v1Store, v2Store, info, existing, force, fullArtifactsIndex, state.compactOffsets)
		result.missingSessions += outcome.missingSessions
		if outcome.compactTranscriptSkipped {
			result.compactTranscriptSkipped++
		}

		if migrateErr != nil {
			recordMigrationSkipOrFailure(ctx, result, info.CheckpointID, migrateErr)
			progress.Increment()
			continue
		}

		if len(mainOpts) > 0 {
			state.pendingMain = append(state.pendingMain, mainOpts...)
		}
		if fullCheckpoint != nil {
			state.pendingFull = append(state.pendingFull, *fullCheckpoint)
			if len(state.pendingFull) == batchSize {
				if err := state.packCurrentBatch(ctx, repo, v2Store, batchSize); err != nil {
					processSpan.RecordError(err)
					processSpan.End()
					return result, state.writtenRefs, err
				}
			}
		}
		result.migrated++
		progress.Increment()
	}

	progress.Finish()
	if err := state.flushMain(ctx, v2Store); err != nil {
		processSpan.RecordError(err)
		processSpan.End()
		return result, state.writtenRefs, err
	}
	processSpan.End()

	stopFinalize := startSpinner(progressOut, "Packing migrated raw transcripts")
	_, partialSpan := perf.Start(ctx, "pack_partial_generation")
	if len(state.pendingFull) > 0 {
		if err := writeMigratedFinalFullCurrent(ctx, repo, v2Store, state.pendingFull); err != nil {
			partialSpan.RecordError(err)
			partialSpan.End()
			stopFinalize(false)
			return result, state.writtenRefs, fmt.Errorf("failed to pack migrated raw transcripts: %w", err)
		}
		// If /full/current already had checkpoints, this final migration write can
		// briefly push the generation past the threshold before rotation. That
		// mirrors other v2 ref-merge cases where a generation may exceed the soft
		// threshold by a small amount.
		if refName, rotated, err := v2Store.RotateCurrentGenerationIfNeeded(ctx, batchSize); err != nil {
			partialSpan.RecordError(err)
			partialSpan.End()
			stopFinalize(false)
			return result, state.writtenRefs, fmt.Errorf("failed to rotate migrated full/current generation: %w", err)
		} else if rotated {
			state.writtenRefs = append(state.writtenRefs, refName)
		}
	} else if len(state.writtenRefs) > 0 && !fullCurrentExistsBefore {
		if err := ensureEmptyV2FullCurrent(ctx, repo); err != nil {
			partialSpan.RecordError(err)
			partialSpan.End()
			stopFinalize(false)
			return result, state.writtenRefs, fmt.Errorf("failed to pack migrated raw transcripts: %w", err)
		}
	}
	partialSpan.End()
	stopFinalize(true)

	return result, state.writtenRefs, nil
}

// migrateLoopState holds the mutable bookkeeping carried across iterations of
// migrateCheckpointsV2's main loop. Helpers hang off it so the loop body
// stays small enough to satisfy the maintainability-index lint.
type migrateLoopState struct {
	pendingFull    []migratedFullCheckpoint
	pendingMain    []checkpoint.WriteCommittedOptions
	writtenRefs    []plumbing.ReferenceName
	nextGeneration int
	batchSize      int

	compactOffsets *migrateCompactOffsetCache
}

func newMigrateLoopState(batchSize int) *migrateLoopState {
	return &migrateLoopState{
		pendingFull:    make([]migratedFullCheckpoint, 0, batchSize),
		pendingMain:    make([]checkpoint.WriteCommittedOptions, 0, batchSize),
		batchSize:      batchSize,
		compactOffsets: newMigrateCompactOffsetCache(),
	}
}

// flushMain pushes any buffered /main entries through WriteCommittedMainBatch.
func (s *migrateLoopState) flushMain(ctx context.Context, v2Store *checkpoint.V2GitStore) error {
	if len(s.pendingMain) == 0 {
		return nil
	}
	if err := v2Store.WriteCommittedMainBatch(ctx, s.pendingMain); err != nil {
		return fmt.Errorf("failed to write batched v2 /main entries: %w", err)
	}
	s.pendingMain = s.pendingMain[:0]
	return nil
}

// packCurrentBatch flushes /main, resolves the next archive slot if needed,
// then archives pendingFull into a /full/<n> ref.
func (s *migrateLoopState) packCurrentBatch(ctx context.Context, repo *git.Repository, v2Store *checkpoint.V2GitStore, batchSize int) error {
	// Flush /main entries first so the index ref can never lag behind the
	// data ref on a mid-batch crash.
	if err := s.flushMain(ctx, v2Store); err != nil {
		return err
	}
	if s.nextGeneration == 0 {
		// Resolve the archive slot only when the first full batch is ready;
		// force migration may prune existing archived refs earlier in the loop.
		next, err := v2Store.NextGenerationNumber()
		if err != nil {
			return fmt.Errorf("list archived v2 generations: %w", err)
		}
		s.nextGeneration = next
	}
	refName := checkpoint.ArchivedGenerationRefName(s.nextGeneration)
	archiveCommitHash, err := writeMigratedFullGeneration(ctx, repo, v2Store, refName, s.pendingFull)
	if err != nil {
		return fmt.Errorf("failed to pack migrated raw transcripts: %w", err)
	}
	if queueErr := queueMigratedFullGenerationPublication(ctx, v2Store, refName, archiveCommitHash); queueErr != nil {
		// The archive ref and pending publication record must move together.
		// If queueing fails, leave no archive ref behind so a retry can
		// repack the generation and queue it again.
		if removeErr := repo.Storer.RemoveReference(refName); removeErr != nil {
			queueErr = fmt.Errorf("%w; failed to remove unqueued generation ref %s: %w", queueErr, refName, removeErr)
		}
		return fmt.Errorf("failed to queue migrated raw transcript generation for push: %w", queueErr)
	}
	s.writtenRefs = append(s.writtenRefs, refName)
	s.nextGeneration++
	s.pendingFull = make([]migratedFullCheckpoint, 0, batchSize)
	return nil
}

// recordMigrationSkipOrFailure classifies a failing migrateOneCheckpoint
// outcome into the appropriate result counter and emits a log line.
func recordMigrationSkipOrFailure(ctx context.Context, result *migrateResult, cpID id.CheckpointID, err error) {
	switch {
	case errors.Is(err, errAlreadyMigrated):
		logCheckpointMigrationSkip(ctx, cpID, "already in v2", err)
		result.skipped++
	case errors.Is(err, errNoMigratableSessions):
		logCheckpointMigrationSkip(ctx, cpID, "no migratable v1 sessions", err)
		result.skipped++
	default:
		logging.Error(ctx, "checkpoint migration failed",
			slog.String("checkpoint_id", string(cpID)),
			slog.String("error", err.Error()),
		)
		result.failed++
	}
}

func logCheckpointMigrationSkip(ctx context.Context, checkpointID id.CheckpointID, reason string, err error) {
	logging.Info(ctx, "checkpoint migration skipped",
		slog.String("checkpoint_id", string(checkpointID)),
		slog.String("reason", reason),
		slog.String("error", err.Error()),
	)
}

// sortMigratableCheckpoints sorts oldest-first so archived generations
// preserve chronological order. Zero timestamps sort last; CheckpointID
// breaks ties for deterministic reruns.
func sortMigratableCheckpoints(checkpoints []checkpoint.CommittedInfo) {
	sort.SliceStable(checkpoints, func(i, j int) bool {
		left := checkpoints[i].CreatedAt
		right := checkpoints[j].CreatedAt
		switch {
		case left.IsZero() && right.IsZero():
			return checkpoints[i].CheckpointID.String() < checkpoints[j].CheckpointID.String()
		case left.IsZero():
			return false
		case right.IsZero():
			return true
		case left.Equal(right):
			return checkpoints[i].CheckpointID.String() < checkpoints[j].CheckpointID.String()
		default:
			return left.Before(right)
		}
	})
}

type migrateCheckpointOutcome struct {
	missingSessions          int
	compactTranscriptSkipped bool
}

// migrateOneCheckpoint returns the prepared /main
// WriteCommittedOptions alongside the in-memory full-checkpoint structure.
// The caller batches the /main writes across many checkpoints into a single
// ref CAS via V2GitStore.WriteCommittedMainBatch. The resume path
// (already-in-v2 without force) returns nil mainOpts; its /main updates flow
// through backfillCompactTranscripts which uses UpdateCommitted.
func migrateOneCheckpoint(ctx context.Context, repo *git.Repository, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, info checkpoint.CommittedInfo, existing *checkpoint.CheckpointSummary, force bool, fullArtifacts checkpoint.FullSessionArtifactsIndex, compactOffsets *migrateCompactOffsetCache) (*migratedFullCheckpoint, []checkpoint.WriteCommittedOptions, migrateCheckpointOutcome, error) {
	var outcome migrateCheckpointOutcome

	if existing != nil && !force {
		// Already in v2. Pack sessions whose /full/* artifacts are missing
		// (resume an interrupted run) and backfill transcript.jsonl on /main
		// where it's missing. With nothing to do on either front, return
		// errAlreadyMigrated so the caller counts it as skipped.
		fullCheckpoint, err := collectMissingFullCheckpointForPacking(ctx, repo, v1Store, v2Store, info, existing, fullArtifacts)
		if err != nil && !errors.Is(err, errAlreadyMigrated) {
			return nil, nil, outcome, err
		}
		backfilled, backfillErr := backfillCompactTranscripts(ctx, v1Store, v2Store, info, existing)
		if errors.Is(backfillErr, errTranscriptNotGeneratable) {
			outcome.compactTranscriptSkipped = true
		} else if backfillErr != nil && !errors.Is(backfillErr, errAlreadyMigrated) {
			return nil, nil, outcome, backfillErr
		}
		if fullCheckpoint == nil {
			if backfilled > 0 {
				return nil, nil, outcome, nil
			}
			return nil, nil, outcome, errAlreadyMigrated
		}
		return fullCheckpoint, nil, outcome, nil
	}

	if existing != nil && force {
		if pruneErr := pruneV2CheckpointForForce(ctx, repo, v2Store, info.CheckpointID); pruneErr != nil {
			return nil, nil, outcome, fmt.Errorf("failed to reset existing v2 checkpoint %s before force migration: %w", info.CheckpointID, pruneErr)
		}
	}

	summary, err := v1Store.ReadCommitted(ctx, info.CheckpointID)
	if err != nil {
		return nil, nil, outcome, fmt.Errorf("failed to read v1 summary: %w", err)
	}
	if summary == nil {
		return nil, nil, outcome, fmt.Errorf("v1 checkpoint %s has no summary", info.CheckpointID)
	}

	compactFailed := false
	shouldCopyTaskMetadata := false
	skippedMissingSessions := 0
	migratedSessions := 0
	v1ToV2SessionIdx := make(map[int]int, len(summary.Sessions))
	fullCheckpoint := &migratedFullCheckpoint{
		checkpointID: info.CheckpointID,
	}
	mainOptsBatch := make([]checkpoint.WriteCommittedOptions, 0, len(summary.Sessions))

	for sessionIdx := range len(summary.Sessions) {
		content, skipped, readErr := readV1SessionForMigration(ctx, v1Store, info.CheckpointID, sessionIdx)
		if skipped {
			skippedMissingSessions++
			outcome.missingSessions++
			continue
		}
		if readErr != nil {
			return nil, nil, outcome, fmt.Errorf("failed to read v1 session %d: %w", sessionIdx, readErr)
		}
		if content.Metadata.IsTask {
			shouldCopyTaskMetadata = true
		}

		opts := buildMigrateWriteOpts(content, info, summary.CombinedAttribution)

		compacted, offset := tryCompactTranscriptAndOffset(ctx, content.Transcript, content.Metadata, compactOffsets)
		if compacted != nil {
			opts.CompactTranscript = compacted
			opts.CompactTranscriptStart = offset
		} else if len(content.Transcript) > 0 {
			compactFailed = true
		}

		// /main entries land in the batch buffer; /full content stays in
		// memory for the per-batch pack.
		mainOpts := opts
		mainOpts.Transcript = redact.AlreadyRedacted(nil)
		mainOptsBatch = append(mainOptsBatch, mainOpts)

		// The checkpoint is empty in v2 here (new, or just pruned by force),
		// so WriteCommittedMainBatch will assign sessions sequentially in
		// input order. The local migratedSessions counter is the predicted
		// v2 session index.
		v2SessionIdx := migratedSessions
		v1ToV2SessionIdx[sessionIdx] = v2SessionIdx
		fullCheckpoint.sessions = append(fullCheckpoint.sessions, migratedFullSession{
			sessionIndex:            v2SessionIdx,
			content:                 content,
			rawTranscriptBlobHashes: content.TranscriptBlobHashes,
		})
		migratedSessions++
	}

	if migratedSessions == 0 {
		return nil, nil, outcome, fmt.Errorf("%w: v1 metadata lists %d session(s), but no transcript/session content exists for any of them", errNoMigratableSessions, len(summary.Sessions))
	}

	if shouldCopyTaskMetadata {
		taskTrees, taskErr := collectTaskMetadataForMigratedFullGeneration(repo, info.CheckpointID, summary, v1ToV2SessionIdx)
		if taskErr != nil {
			logging.Warn(ctx, "failed to copy task metadata to v2",
				slog.String("checkpoint_id", string(info.CheckpointID)),
				slog.String("error", taskErr.Error()),
			)
		} else {
			fullCheckpoint.taskTrees = taskTrees
		}
	}

	if compactFailed {
		outcome.compactTranscriptSkipped = true
		logging.Warn(ctx, "compact transcript not generated during checkpoint migration",
			slog.String("checkpoint_id", string(info.CheckpointID)),
			slog.Int("migrated_sessions", migratedSessions),
		)
	}
	if skippedMissingSessions > 0 {
		logging.Warn(ctx, "checkpoint migration skipped v1 sessions with missing transcript/session content",
			slog.String("checkpoint_id", string(info.CheckpointID)),
			slog.Int("missing_sessions", skippedMissingSessions),
		)
	}

	return fullCheckpoint, mainOptsBatch, outcome, nil
}

func migrateFullBatchSize() int {
	batchSize := migrateMaxCheckpointsPerGeneration
	if batchSize <= 0 {
		return checkpoint.DefaultMaxCheckpointsPerGeneration
	}
	return batchSize
}

func listExistingV2Checkpoints(ctx context.Context, repo *git.Repository, v2Store *checkpoint.V2GitStore) (map[id.CheckpointID]struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("context canceled before listing v2 checkpoints: %w", err)
	}

	existing := make(map[id.CheckpointID]struct{})
	_, rootTreeHash, err := v2Store.GetRefState(plumbing.ReferenceName(paths.V2MainRefName))
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return existing, nil
		}
		return nil, fmt.Errorf("read v2 /main ref: %w", err)
	}

	rootTree, err := repo.TreeObject(rootTreeHash)
	if err != nil {
		return nil, fmt.Errorf("read v2 /main tree: %w", err)
	}

	if err := checkpoint.WalkCheckpointShards(ctx, repo, rootTree, func(cpID id.CheckpointID, _ plumbing.Hash) error {
		existing[cpID] = struct{}{}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walk v2 /main checkpoints: %w", err)
	}

	return existing, nil
}

func readExistingV2Checkpoint(ctx context.Context, v2Store *checkpoint.V2GitStore, existing map[id.CheckpointID]struct{}, cpID id.CheckpointID) (*checkpoint.CheckpointSummary, bool, error) {
	if _, ok := existing[cpID]; !ok {
		return nil, false, nil
	}
	existingSummary, err := v2Store.ReadCommitted(ctx, cpID)
	if err != nil {
		return nil, true, fmt.Errorf("failed to check v2 for checkpoint %s: %w", cpID, err)
	}
	return existingSummary, true, nil
}

func writeMigratedFinalFullCurrent(ctx context.Context, repo *git.Repository, v2Store *checkpoint.V2GitStore, checkpoints []migratedFullCheckpoint) error {
	if len(checkpoints) == 0 {
		return nil
	}

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	parentHash, rootTreeHash, err := v2Store.GetRefState(refName)
	if err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("read v2 full/current ref: %w", err)
	}

	entries := make(map[string]object.TreeEntry)
	if rootTreeHash != plumbing.ZeroHash {
		rootTree, err := repo.TreeObject(rootTreeHash)
		if err != nil {
			return fmt.Errorf("read v2 full/current tree: %w", err)
		}
		if err := checkpoint.FlattenTree(repo, rootTree, "", entries); err != nil {
			return fmt.Errorf("flatten v2 full/current tree: %w", err)
		}
		delete(entries, paths.GenerationFileName)
	}

	// Evict any pre-existing raw transcript artifacts for the sessions we're
	// about to write so a shrinking chunk count can't leave stale .N files
	// behind from a prior migration of the same checkpoint.
	for _, cp := range checkpoints {
		for _, session := range cp.sessions {
			evictMigratedRawArtifacts(entries, cp.checkpointID, session.sessionIndex)
		}
	}
	pendingEntries, err := buildMigratedFullEntrySet(ctx, repo, checkpoints)
	if err != nil {
		return fmt.Errorf("write migrated full/current entries: %w", err)
	}
	pendingEntries.mergeInto(entries)

	treeHash, err := checkpoint.BuildTreeFromEntries(ctx, repo, entries)
	if err != nil {
		return fmt.Errorf("build migrated full/current tree: %w", err)
	}

	commitHash, err := checkpoint.CreateCommit(ctx, repo, treeHash, parentHash,
		"Write migrated partial generation\n",
		migrateAuthorName, migrateAuthorEmail)
	if err != nil {
		return fmt.Errorf("create migrated full/current commit: %w", err)
	}

	return updateV2FullCurrentRef(ctx, repo, parentHash, commitHash)
}

func updateV2FullCurrentRef(ctx context.Context, repo *git.Repository, expectedHash, newHash plumbing.Hash) error {
	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	newRef := plumbing.NewHashReference(refName, newHash)
	if expectedHash == plumbing.ZeroHash {
		return createV2FullCurrentRefIfMissing(ctx, repo, refName, newHash)
	}

	oldRef := plumbing.NewHashReference(refName, expectedHash)
	if err := repo.Storer.CheckAndSetReference(newRef, oldRef); err != nil {
		return fmt.Errorf("update %s: %w", refName, err)
	}
	return nil
}

func createV2FullCurrentRefIfMissing(ctx context.Context, repo *git.Repository, refName plumbing.ReferenceName, newHash plumbing.Hash) error {
	root, err := repoWorktreeRoot(repo)
	if err != nil {
		return fmt.Errorf("resolve worktree root for %s update: %w", refName, err)
	}

	cmd := exec.CommandContext(ctx, "git", "update-ref", "--no-deref", refName.String(), newHash.String(), "")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	if currentRef, refErr := repo.Reference(refName, true); refErr == nil {
		if currentRef.Hash() == newHash {
			return nil
		}
		return fmt.Errorf("update %s: %w", refName, storage.ErrReferenceHasChanged)
	}
	if len(output) > 0 {
		return fmt.Errorf("update %s: %w: %s", refName, err, strings.TrimSpace(string(output)))
	}
	return fmt.Errorf("update %s: %w", refName, err)
}

func repoWorktreeRoot(repo *git.Repository) (string, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("open worktree: %w", err)
	}
	root := worktree.Filesystem().Root()
	if root == "" {
		return "", errors.New("repository worktree filesystem has no root path")
	}
	return root, nil
}

func writeMigratedFullGeneration(ctx context.Context, repo *git.Repository, v2Store *checkpoint.V2GitStore, refName plumbing.ReferenceName, checkpoints []migratedFullCheckpoint) (plumbing.Hash, error) {
	fullEntries, err := buildMigratedFullEntrySet(ctx, repo, checkpoints)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("write migrated generation entries: %w", err)
	}

	entries := make(map[string]object.TreeEntry, len(fullEntries.rawEntries)+len(fullEntries.taskEntries))
	fullEntries.mergeInto(entries)
	treeHash, err := checkpoint.BuildTreeFromEntries(ctx, repo, entries)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("build migrated generation tree: %w", err)
	}

	gen, found := generationMetadataFromMigratedSessions(checkpoints)
	if !found {
		gen, found = checkpoint.AggregateTranscriptTimestamps(migratedTranscripts(checkpoints))
	}
	if !found {
		var err error
		gen, found, err = v2Store.ComputeGenerationCheckpointTimestamps(ctx, treeHash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("compute checkpoint timestamps: %w", err)
		}
	}
	if !found {
		return plumbing.ZeroHash, fmt.Errorf("no timestamps found for migrated generation %s", refName)
	}

	treeHash, err = v2Store.AddGenerationJSONToTree(treeHash, gen)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("add generation metadata: %w", err)
	}

	commitHash, err := checkpoint.CreateCommit(ctx, repo, treeHash, plumbing.ZeroHash,
		fmt.Sprintf("Archive migrated generation: %s\n", refName),
		migrateAuthorName, migrateAuthorEmail)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("create migrated generation commit: %w", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("update migrated generation ref %s: %w", refName, err)
	}
	return commitHash, nil
}

func queueMigratedFullGenerationPublication(ctx context.Context, v2Store *checkpoint.V2GitStore, refName plumbing.ReferenceName, archiveCommitHash plumbing.Hash) error {
	if err := v2Store.AppendPendingFullGenerationPublication(ctx, checkpoint.PendingV2FullGenerationPublication{
		ArchiveRefName:    refName.String(),
		ArchiveCommitHash: archiveCommitHash.String(),
		QueuedAt:          time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("append pending archive publication for %s: %w", refName, err)
	}
	return nil
}

func migratedTranscripts(checkpoints []migratedFullCheckpoint) [][]byte {
	var transcripts [][]byte
	for _, cp := range checkpoints {
		for _, session := range cp.sessions {
			if len(session.content.Transcript) == 0 {
				continue
			}
			transcripts = append(transcripts, session.content.Transcript)
		}
	}
	return transcripts
}

func generationMetadataFromMigratedSessions(checkpoints []migratedFullCheckpoint) (checkpoint.GenerationMetadata, bool) {
	var gen checkpoint.GenerationMetadata
	found := false
	for _, cp := range checkpoints {
		for _, session := range cp.sessions {
			checkpoint.MergeGenerationTime(&gen, &found, session.content.Metadata.CreatedAt)
		}
	}
	return gen, found
}

type migratedFullEntrySet struct {
	rawEntries  []object.TreeEntry
	taskEntries []object.TreeEntry
}

// mergeInto merges this entry set into dst. Raw entries override existing
// entries at the same path; task entries do not override.
func (s migratedFullEntrySet) mergeInto(dst map[string]object.TreeEntry) {
	for _, entry := range s.rawEntries {
		dst[entry.Name] = entry
	}
	for _, entry := range s.taskEntries {
		if _, exists := dst[entry.Name]; exists {
			continue
		}
		dst[entry.Name] = entry
	}
}

// evictMigratedRawArtifacts removes any pre-existing raw transcript blobs
// (`transcript.jsonl`, chunk-suffixed variants, and the hash file) for the
// given checkpoint session from entries.
func evictMigratedRawArtifacts(entries map[string]object.TreeEntry, checkpointID id.CheckpointID, sessionIndex int) {
	sessionPath := fmt.Sprintf("%s/%d/", checkpointID.Path(), sessionIndex)
	transcriptPath := sessionPath + paths.V2RawTranscriptFileName
	hashPath := sessionPath + paths.V2RawTranscriptHashFileName
	for key := range entries {
		if key == transcriptPath || strings.HasPrefix(key, transcriptPath+".") || key == hashPath {
			delete(entries, key)
		}
	}
}

func buildMigratedFullEntrySet(ctx context.Context, repo *git.Repository, checkpoints []migratedFullCheckpoint) (migratedFullEntrySet, error) {
	var entries migratedFullEntrySet
	for _, cp := range checkpoints {
		for _, session := range cp.sessions {
			sessionEntries, err := buildMigratedFullSessionEntrySet(ctx, repo, cp, session)
			if err != nil {
				return migratedFullEntrySet{}, fmt.Errorf("checkpoint %s session %d: %w", cp.checkpointID, session.sessionIndex, err)
			}
			entries.rawEntries = append(entries.rawEntries, sessionEntries.rawEntries...)
			entries.taskEntries = append(entries.taskEntries, sessionEntries.taskEntries...)
		}
	}
	return entries, nil
}

func buildMigratedFullSessionEntrySet(ctx context.Context, repo *git.Repository, cp migratedFullCheckpoint, session migratedFullSession) (migratedFullEntrySet, error) {
	sessionPath := fmt.Sprintf("%s/%d/", cp.checkpointID.Path(), session.sessionIndex)
	transcript := session.content.Transcript
	rawHashPath := sessionPath + paths.V2RawTranscriptHashFileName
	var entries migratedFullEntrySet

	if len(session.rawTranscriptBlobHashes) > 0 {
		for i, blobHash := range session.rawTranscriptBlobHashes {
			path := sessionPath + agent.ChunkFileName(paths.V2RawTranscriptFileName, i)
			entries.rawEntries = append(entries.rawEntries, object.TreeEntry{
				Name: path,
				Mode: filemode.Regular,
				Hash: blobHash,
			})
		}
	} else {
		chunks, err := agent.ChunkTranscript(ctx, transcript, session.content.Metadata.Agent)
		if err != nil {
			return migratedFullEntrySet{}, fmt.Errorf("chunk transcript: %w", err)
		}
		for i, chunk := range chunks {
			blobHash, blobErr := checkpoint.CreateBlobFromContent(repo, chunk)
			if blobErr != nil {
				return migratedFullEntrySet{}, fmt.Errorf("create transcript blob: %w", blobErr)
			}
			path := sessionPath + agent.ChunkFileName(paths.V2RawTranscriptFileName, i)
			entries.rawEntries = append(entries.rawEntries, object.TreeEntry{
				Name: path,
				Mode: filemode.Regular,
				Hash: blobHash,
			})
		}
	}

	contentHash := fmt.Sprintf("sha256:%x", sha256.Sum256(transcript))
	hashBlob, err := checkpoint.CreateBlobFromContent(repo, []byte(contentHash))
	if err != nil {
		return migratedFullEntrySet{}, fmt.Errorf("create transcript hash blob: %w", err)
	}
	entries.rawEntries = append(entries.rawEntries, object.TreeEntry{
		Name: rawHashPath,
		Mode: filemode.Regular,
		Hash: hashBlob,
	})

	for _, taskTreeHash := range cp.taskTrees[session.sessionIndex] {
		taskTree, treeErr := repo.TreeObject(taskTreeHash)
		if treeErr != nil {
			return migratedFullEntrySet{}, fmt.Errorf("read task metadata tree: %w", treeErr)
		}
		taskEntries := make(map[string]object.TreeEntry)
		if flattenErr := checkpoint.FlattenTree(repo, taskTree, sessionPath+"tasks", taskEntries); flattenErr != nil {
			return migratedFullEntrySet{}, fmt.Errorf("flatten task metadata tree: %w", flattenErr)
		}
		for _, entry := range taskEntries {
			entries.taskEntries = append(entries.taskEntries, entry)
		}
	}

	return entries, nil
}

func ensureEmptyV2FullCurrent(ctx context.Context, repo *git.Repository) error {
	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	if _, err := repo.Reference(refName, true); err == nil {
		return nil
	}

	emptyTreeHash, err := checkpoint.BuildTreeFromEntries(ctx, repo, map[string]object.TreeEntry{})
	if err != nil {
		return fmt.Errorf("build empty v2 full/current tree: %w", err)
	}

	commitHash, err := checkpoint.CreateCommit(ctx, repo, emptyTreeHash, plumbing.ZeroHash,
		"Start generation\n",
		migrateAuthorName, migrateAuthorEmail)
	if err != nil {
		return fmt.Errorf("create empty v2 full/current commit: %w", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)); err != nil {
		return fmt.Errorf("update %s: %w", refName, err)
	}
	return nil
}

func readV1SessionForMigration(ctx context.Context, v1Store *checkpoint.GitStore, checkpointID id.CheckpointID, sessionIdx int) (*checkpoint.SessionContent, bool, error) {
	content, readErr := v1Store.ReadSessionContent(ctx, checkpointID, sessionIdx)
	if readErr != nil {
		if errors.Is(readErr, checkpoint.ErrNoTranscript) || errors.Is(readErr, checkpoint.ErrCheckpointNotFound) {
			warnMissingV1Session(ctx, checkpointID, sessionIdx, readErr)
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("read v1 session content: %w", readErr)
	}
	return content, false, nil
}

func warnMissingV1Session(ctx context.Context, checkpointID id.CheckpointID, sessionIdx int, err error) {
	logging.Warn(ctx, "skipping v1 session with missing transcript during checkpoint migration",
		slog.String("checkpoint_id", checkpointID.String()),
		slog.Int("session_index", sessionIdx),
		slog.String("error", err.Error()),
	)
}

func pruneV2CheckpointForForce(ctx context.Context, repo *git.Repository, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID) error {
	for _, refName := range []plumbing.ReferenceName{
		plumbing.ReferenceName(paths.V2MainRefName),
		plumbing.ReferenceName(paths.V2FullCurrentRefName),
	} {
		if err := pruneV2CheckpointRef(ctx, repo, v2Store, refName, cpID); err != nil {
			return err
		}
	}

	archived, err := v2Store.ListArchivedGenerations()
	if err != nil {
		return fmt.Errorf("failed to list archived v2 generations while pruning checkpoint %s: %w", cpID, err)
	}
	for _, generation := range archived {
		refName := plumbing.ReferenceName(paths.V2FullRefPrefix + generation)
		if err := pruneV2ArchivedCheckpointRef(ctx, repo, v2Store, refName, cpID); err != nil {
			return err
		}
	}

	return nil
}

func pruneV2CheckpointRef(ctx context.Context, repo *git.Repository, v2Store *checkpoint.V2GitStore, refName plumbing.ReferenceName, cpID id.CheckpointID) error {
	parentHash, rootTreeHash, err := v2Store.GetRefState(refName)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil
		}
		return fmt.Errorf("failed to get v2 ref state for %s: %w", refName, err)
	}

	rootTree, err := repo.TreeObject(rootTreeHash)
	if err != nil {
		return fmt.Errorf("failed to read v2 tree for %s: %w", refName, err)
	}
	if _, err := rootTree.Tree(cpID.Path()); err != nil {
		return nil //nolint:nilerr // Checkpoint is absent from this ref, so there is nothing to prune.
	}

	shardPrefix := string(cpID[:2])
	shardSuffix := string(cpID[2:])
	newRoot, err := pruneCheckpointFromRoot(repo, rootTreeHash, shardPrefix, shardSuffix)
	if err != nil {
		return fmt.Errorf("failed to remove checkpoint subtree from %s: %w", refName, err)
	}
	if newRoot == rootTreeHash {
		return nil
	}

	commitHash, err := checkpoint.CreateCommit(ctx, repo, newRoot, parentHash,
		fmt.Sprintf("Reset checkpoint before force migration: %s\n", cpID),
		migrateAuthorName, migrateAuthorEmail)
	if err != nil {
		return fmt.Errorf("failed to create v2 prune commit for %s: %w", refName, err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)); err != nil {
		return fmt.Errorf("failed to update ref %s: %w", refName, err)
	}
	return nil
}

func pruneV2ArchivedCheckpointRef(ctx context.Context, repo *git.Repository, v2Store *checkpoint.V2GitStore, refName plumbing.ReferenceName, cpID id.CheckpointID) error {
	parentHash, rootTreeHash, err := v2Store.GetRefState(refName)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil
		}
		return fmt.Errorf("failed to get v2 ref state for %s: %w", refName, err)
	}

	rootTree, err := repo.TreeObject(rootTreeHash)
	if err != nil {
		return fmt.Errorf("failed to read v2 tree for %s: %w", refName, err)
	}
	if _, err := rootTree.Tree(cpID.Path()); err != nil {
		return nil //nolint:nilerr // Checkpoint is absent from this ref, so there is nothing to prune.
	}

	shardPrefix := string(cpID[:2])
	shardSuffix := string(cpID[2:])
	newRoot, err := pruneCheckpointFromRoot(repo, rootTreeHash, shardPrefix, shardSuffix)
	if err != nil {
		return fmt.Errorf("failed to remove checkpoint subtree from %s: %w", refName, err)
	}
	if newRoot == rootTreeHash {
		return nil
	}

	count, err := v2Store.CountCheckpointsInTree(ctx, newRoot)
	if err != nil {
		return fmt.Errorf("failed to count checkpoints in pruned %s: %w", refName, err)
	}
	if count == 0 {
		if err := repo.Storer.RemoveReference(refName); err != nil {
			return fmt.Errorf("failed to remove empty archived v2 generation %s: %w", refName, err)
		}
		return nil
	}

	newRoot, err = addRecomputedGenerationJSON(ctx, v2Store, newRoot)
	if err != nil {
		return fmt.Errorf("failed to recompute generation metadata for %s: %w", refName, err)
	}

	commitHash, err := checkpoint.CreateCommit(ctx, repo, newRoot, parentHash,
		fmt.Sprintf("Reset checkpoint before force migration: %s\n", cpID),
		migrateAuthorName, migrateAuthorEmail)
	if err != nil {
		return fmt.Errorf("failed to create v2 prune commit for %s: %w", refName, err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)); err != nil {
		return fmt.Errorf("failed to update ref %s: %w", refName, err)
	}
	return nil
}

func addRecomputedGenerationJSON(ctx context.Context, v2Store *checkpoint.V2GitStore, treeHash plumbing.Hash) (plumbing.Hash, error) {
	gen, found, err := v2Store.ComputeGenerationTimestampsFromTrees(ctx, treeHash, nil)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("compute raw transcript timestamps: %w", err)
	}
	if !found {
		gen, found, err = v2Store.ComputeGenerationCheckpointTimestamps(ctx, treeHash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("compute checkpoint timestamps: %w", err)
		}
	}
	if !found {
		return treeHash, nil
	}

	newTreeHash, err := v2Store.AddGenerationJSONToTree(treeHash, gen)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("add generation metadata: %w", err)
	}
	return newTreeHash, nil
}

func pruneCheckpointFromRoot(repo *git.Repository, rootTreeHash plumbing.Hash, shardPrefix, shardSuffix string) (plumbing.Hash, error) {
	newRoot, err := checkpoint.UpdateSubtree(repo, rootTreeHash,
		[]string{shardPrefix},
		nil,
		checkpoint.UpdateSubtreeOptions{
			MergeMode:   checkpoint.MergeKeepExisting,
			DeleteNames: []string{shardSuffix},
		},
	)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to prune checkpoint from shard: %w", err)
	}
	if newRoot == rootTreeHash {
		return newRoot, nil
	}

	newRootTree, err := repo.TreeObject(newRoot)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to read pruned root tree: %w", err)
	}
	shardTree, err := newRootTree.Tree(shardPrefix)
	if err != nil {
		return newRoot, nil //nolint:nilerr // The shard prefix was already absent after pruning.
	}
	if len(shardTree.Entries) > 0 {
		return newRoot, nil
	}

	prunedRoot, err := checkpoint.UpdateSubtree(repo, rootTreeHash,
		nil,
		nil,
		checkpoint.UpdateSubtreeOptions{
			MergeMode:   checkpoint.MergeKeepExisting,
			DeleteNames: []string{shardPrefix},
		},
	)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to prune empty shard prefix: %w", err)
	}
	return prunedRoot, nil
}

func buildMigrateWriteOpts(content *checkpoint.SessionContent, info checkpoint.CommittedInfo, combinedAttribution *checkpoint.InitialAttribution) checkpoint.WriteCommittedOptions {
	m := content.Metadata

	prompts := checkpoint.SplitPromptContent(content.Prompts)

	return checkpoint.WriteCommittedOptions{
		CheckpointID: info.CheckpointID,
		SessionID:    m.SessionID,
		CreatedAt:    m.CreatedAt,
		Strategy:     m.Strategy,
		Branch:       m.Branch,
		// All transcripts read here come from persisted checkpoint storage,
		// which is already redacted.
		Transcript:                  redact.AlreadyRedacted(content.Transcript),
		Prompts:                     prompts,
		FilesTouched:                m.FilesTouched,
		CheckpointsCount:            m.CheckpointsCount,
		Agent:                       m.Agent,
		Model:                       m.Model,
		TurnID:                      m.TurnID,
		TokenUsage:                  m.TokenUsage,
		SessionMetrics:              m.SessionMetrics,
		InitialAttribution:          m.InitialAttribution,
		PromptAttributionsJSON:      m.PromptAttributions,
		CombinedAttribution:         combinedAttribution,
		Summary:                     m.Summary,
		CheckpointTranscriptStart:   m.GetTranscriptStart(),
		TranscriptIdentifierAtStart: m.TranscriptIdentifierAtStart,
		IsTask:                      m.IsTask,
		ToolUseID:                   m.ToolUseID,
		AuthorName:                  migrateAuthorName,
		AuthorEmail:                 migrateAuthorEmail,
	}
}

func tryCompactTranscript(ctx context.Context, transcript []byte, m checkpoint.CommittedMetadata) []byte {
	return compactTranscriptForStartLine(ctx, transcript, m, 0)
}

type migrateCompactOffsetCache struct {
	offsets map[migrateCompactOffsetKey]int
}

type migrateCompactOffsetKey struct {
	sessionID          string
	agent              string
	transcriptPosition int
}

func newMigrateCompactOffsetCache() *migrateCompactOffsetCache {
	return &migrateCompactOffsetCache{offsets: make(map[migrateCompactOffsetKey]int)}
}

func (c *migrateCompactOffsetCache) lookup(m checkpoint.CommittedMetadata, transcriptPosition int) (int, bool) {
	if c == nil || m.SessionID == "" || transcriptPosition <= 0 {
		return 0, false
	}
	offset, ok := c.offsets[migrateCompactOffsetKey{
		sessionID:          m.SessionID,
		agent:              string(m.Agent),
		transcriptPosition: transcriptPosition,
	}]
	return offset, ok
}

func (c *migrateCompactOffsetCache) record(m checkpoint.CommittedMetadata, transcriptPosition, compactPosition int) {
	if c == nil || m.SessionID == "" || transcriptPosition <= 0 || compactPosition < 0 {
		return
	}
	c.offsets[migrateCompactOffsetKey{
		sessionID:          m.SessionID,
		agent:              string(m.Agent),
		transcriptPosition: transcriptPosition,
	}] = compactPosition
}

func tryCompactTranscriptAndOffset(ctx context.Context, transcript []byte, m checkpoint.CommittedMetadata, compactOffsets *migrateCompactOffsetCache) ([]byte, int) {
	compacted := tryCompactTranscript(ctx, transcript, m)
	if compacted == nil {
		return nil, 0
	}
	compactLines := bytes.Count(compacted, []byte{'\n'})

	startLine := m.GetTranscriptStart()
	offset := 0
	if startLine > 0 {
		var ok bool
		offset, ok = compactOffsets.lookup(m, startLine)
		if !ok {
			offset = computeCompactOffset(ctx, transcript, compacted, m)
		}
	}

	if position, ok := migrationTranscriptPosition(m.Agent, transcript); ok {
		compactOffsets.record(m, position, compactLines)
	}
	return compacted, offset
}

func migrationTranscriptPosition(agentType agenttypes.AgentType, content []byte) (int, bool) {
	if len(content) == 0 {
		return 0, true
	}

	switch agentType {
	case agent.AgentTypeGemini, agent.AgentTypeOpenCode:
		var session struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(content), &session); err != nil {
			return 0, false
		}
		return len(session.Messages), true
	default:
		lines := bytes.Split(content, []byte{'\n'})
		for len(lines) > 0 && len(bytes.TrimSpace(lines[len(lines)-1])) == 0 {
			lines = lines[:len(lines)-1]
		}
		return len(lines), true
	}
}

func compactTranscriptForStartLine(ctx context.Context, transcript []byte, m checkpoint.CommittedMetadata, startLine int) []byte {
	if len(transcript) == 0 {
		return nil
	}
	if m.Agent == "" {
		logging.Warn(ctx, "compact transcript skipped: no agent type in checkpoint metadata",
			slog.String("checkpoint_id", string(m.CheckpointID)),
		)
		return nil
	}

	compacted, err := compact.Compact(redact.AlreadyRedacted(transcript), compact.MetadataFields{
		Agent:      string(m.Agent),
		CLIVersion: versioninfo.Version,
		StartLine:  startLine,
	})
	if err != nil {
		logging.Warn(ctx, "compact transcript generation failed during migration",
			slog.String("checkpoint_id", string(m.CheckpointID)),
			slog.String("agent", string(m.Agent)),
			slog.String("error", err.Error()),
		)
		return nil
	}
	if len(compacted) == 0 {
		logging.Warn(ctx, "transcript.jsonl generation produced no output",
			slog.String("checkpoint_id", string(m.CheckpointID)),
			slog.String("agent", string(m.Agent)),
			slog.Int("input_bytes", len(transcript)),
		)
		return nil
	}
	return compacted
}

// computeCompactOffset determines the transcript.jsonl line offset for a checkpoint
// by comparing a full compact (startLine=0) against the scoped compact. The difference
// is the number of compact lines before this checkpoint's data.
func computeCompactOffset(ctx context.Context, fullTranscript, fullCompact []byte, m checkpoint.CommittedMetadata) int {
	startLine := m.GetTranscriptStart()
	if startLine == 0 || len(fullTranscript) == 0 || m.Agent == "" {
		return 0
	}

	if len(fullCompact) == 0 {
		return 0
	}

	scopedCompact, err := compact.Compact(redact.AlreadyRedacted(fullTranscript), compact.MetadataFields{
		Agent:      string(m.Agent),
		CLIVersion: versioninfo.Version,
		StartLine:  startLine,
	})
	if err != nil {
		logging.Warn(ctx, "compact transcript offset calculation failed during migration",
			slog.String("checkpoint_id", string(m.CheckpointID)),
			slog.String("agent", string(m.Agent)),
			slog.String("error", err.Error()),
		)
		return 0
	}
	if len(scopedCompact) == 0 {
		return 0
	}

	fullLines := bytes.Count(fullCompact, []byte{'\n'})
	scopedLines := bytes.Count(scopedCompact, []byte{'\n'})
	offset := fullLines - scopedLines
	if offset < 0 {
		logging.Warn(ctx, "compact transcript offset was negative during migration, defaulting to 0",
			slog.String("checkpoint_id", string(m.CheckpointID)),
			slog.Int("full_lines", fullLines),
			slog.Int("scoped_lines", scopedLines),
		)
		return 0
	}
	return offset
}

func collectTaskMetadataForMigratedFullGeneration(repo *git.Repository, cpID id.CheckpointID, summary *checkpoint.CheckpointSummary, v1ToV2SessionIdx map[int]int) (map[int][]plumbing.Hash, error) {
	rootTaskV2SessionIdx, attachRootTasks := latestMigratedV2SessionIndex(v1ToV2SessionIdx)
	return collectTaskMetadataForMigratedFullGenerationWithRootSession(repo, cpID, summary, v1ToV2SessionIdx, rootTaskV2SessionIdx, attachRootTasks)
}

func collectTaskMetadataForMigratedFullGenerationWithRootSession(
	repo *git.Repository,
	cpID id.CheckpointID,
	summary *checkpoint.CheckpointSummary,
	v1ToV2SessionIdx map[int]int,
	rootTaskV2SessionIdx int,
	attachRootTasks bool,
) (map[int][]plumbing.Hash, error) {
	v1Tree, err := resolveV1CheckpointTree(repo, cpID)
	if err != nil {
		return nil, err
	}

	taskTrees := make(map[int][]plumbing.Hash)

	// Legacy v1 layout stores task metadata at checkpoint root: <cp>/tasks/<tool-use-id>/...
	// Prefer attaching this tree to the latest session in v2.
	if rootTasksTree, rootTasksErr := v1Tree.Tree("tasks"); rootTasksErr == nil {
		if attachRootTasks {
			taskTrees[rootTaskV2SessionIdx] = append(taskTrees[rootTaskV2SessionIdx], rootTasksTree.Hash)
		}
	}

	for sessionIdx := range len(summary.Sessions) {
		sessionDir := strconv.Itoa(sessionIdx)
		sessionTree, sessionErr := v1Tree.Tree(sessionDir)
		if sessionErr != nil {
			continue
		}

		tasksTree, tasksErr := sessionTree.Tree("tasks")
		if tasksErr != nil {
			continue
		}

		v2SessionIdx, ok := v1ToV2SessionIdx[sessionIdx]
		if !ok {
			continue
		}
		taskTrees[v2SessionIdx] = append(taskTrees[v2SessionIdx], tasksTree.Hash)
	}

	return taskTrees, nil
}

func latestMigratedV2SessionIndex(v1ToV2SessionIdx map[int]int) (int, bool) {
	latest := -1
	for _, v2SessionIdx := range v1ToV2SessionIdx {
		if v2SessionIdx > latest {
			latest = v2SessionIdx
		}
	}
	if latest < 0 {
		return -1, false
	}
	return latest, true
}

func resolveV1CheckpointTree(repo *git.Repository, cpID id.CheckpointID) (*object.Tree, error) {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		// Try remote tracking branch
		remoteRefName := plumbing.NewRemoteReferenceName(migrateRemoteName, paths.MetadataBranchName)
		ref, err = repo.Reference(remoteRefName, true)
		if err != nil {
			return nil, fmt.Errorf("v1 branch not found: %w", err)
		}
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get v1 commit: %w", err)
	}

	rootTree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get v1 tree: %w", err)
	}

	cpTree, err := rootTree.Tree(cpID.Path())
	if err != nil {
		return nil, fmt.Errorf("checkpoint %s not found in v1 tree: %w", cpID, err)
	}

	return cpTree, nil
}

// collectMissingFullCheckpointForPacking returns a migratedFullCheckpoint
// scoped to the sessions that lack /full/* artifacts on this v2 checkpoint,
// reading their content from v1 so the caller can hand it to the same
// batched archive flow used for fresh migrations. Returns errAlreadyMigrated
// when every session is already packed.
func collectMissingFullCheckpointForPacking(
	ctx context.Context,
	repo *git.Repository,
	v1Store *checkpoint.GitStore,
	v2Store *checkpoint.V2GitStore,
	info checkpoint.CommittedInfo,
	v2Summary *checkpoint.CheckpointSummary,
	fullArtifacts checkpoint.FullSessionArtifactsIndex,
) (*migratedFullCheckpoint, error) {
	missingSessions, err := collectMissingFullSessionsForPacking(ctx, v2Store, info.CheckpointID, v2Summary, fullArtifacts)
	if err != nil {
		return nil, err
	}
	if len(missingSessions) == 0 {
		return nil, errAlreadyMigrated
	}

	v1Summary, err := v1Store.ReadCommitted(ctx, info.CheckpointID)
	if err != nil {
		return nil, fmt.Errorf("failed to read v1 summary while resuming v2 migration: %w", err)
	}
	if v1Summary == nil {
		return nil, fmt.Errorf("v1 checkpoint %s has no summary", info.CheckpointID)
	}

	v1BySessionID, err := collectV1SessionIndexesForPacking(ctx, v1Store, info.CheckpointID, v1Summary, missingSessions)
	if err != nil {
		return nil, err
	}

	fullCheckpoint := &migratedFullCheckpoint{
		checkpointID: info.CheckpointID,
	}
	v1ToV2SessionIdx := make(map[int]int)

	for _, missingSession := range missingSessions {
		v1Session, ok, readErr := readV1SessionForMissingFullArtifact(ctx, v1Store, info.CheckpointID, v1Summary, v1BySessionID, missingSession)
		if readErr != nil {
			return nil, readErr
		}
		if !ok {
			return nil, fmt.Errorf("failed to find v1 session for v2 session %d while resuming migration", missingSession.sessionIndex)
		}

		fullCheckpoint.sessions = append(fullCheckpoint.sessions, migratedFullSession{
			sessionIndex:            missingSession.sessionIndex,
			content:                 v1Session.content,
			rawTranscriptBlobHashes: v1Session.content.TranscriptBlobHashes,
		})
		v1ToV2SessionIdx[v1Session.sessionIndex] = missingSession.sessionIndex
	}

	latestV2SessionIdx := len(v2Summary.Sessions) - 1
	taskTrees, taskErr := collectTaskMetadataForMigratedFullGenerationWithRootSession(
		repo,
		info.CheckpointID,
		v1Summary,
		v1ToV2SessionIdx,
		latestV2SessionIdx,
		latestV2SessionIdx >= 0,
	)
	if taskErr != nil {
		return nil, fmt.Errorf("failed to collect task metadata while resuming migration: %w", taskErr)
	}
	fullCheckpoint.taskTrees = taskTrees

	return fullCheckpoint, nil
}

type missingFullSessionForPacking struct {
	sessionIndex int
	sessionID    string
}

type v1SessionForPacking struct {
	sessionIndex int
	content      *checkpoint.SessionContent
}

func collectMissingFullSessionsForPacking(
	ctx context.Context,
	v2Store *checkpoint.V2GitStore,
	checkpointID id.CheckpointID,
	summary *checkpoint.CheckpointSummary,
	fullArtifacts checkpoint.FullSessionArtifactsIndex,
) ([]missingFullSessionForPacking, error) {
	missingSessions := make([]missingFullSessionForPacking, 0)
	for sessionIdx := range len(summary.Sessions) {
		// Production passes a pre-built index; nil falls back to the per-call
		// predicate for tests that exercise this helper directly.
		var ok bool
		if fullArtifacts != nil {
			ok = fullArtifacts.Has(checkpointID, sessionIdx)
		} else {
			var checkErr error
			ok, checkErr = v2Store.HasFullSessionArtifacts(checkpointID, sessionIdx)
			if checkErr != nil {
				return nil, fmt.Errorf("failed to check v2 session %d artifacts: %w", sessionIdx, checkErr)
			}
		}
		if ok {
			continue
		}

		// Metadata-only read: only SessionID is needed downstream.
		meta, readErr := v2Store.ReadSessionMetadata(ctx, checkpointID, sessionIdx)
		if readErr != nil {
			return nil, fmt.Errorf("failed to read v2 session %d metadata while resuming migration: %w", sessionIdx, readErr)
		}

		missingSessions = append(missingSessions, missingFullSessionForPacking{
			sessionIndex: sessionIdx,
			sessionID:    meta.SessionID,
		})
	}

	return missingSessions, nil
}

// collectV1SessionIndexesForPacking maps each missing session's id to its v1
// session index. Resolving by session_id is necessary because v1/v2 indices
// can drift — v1 sessions without a transcript are skipped on fresh migration.
func collectV1SessionIndexesForPacking(
	ctx context.Context,
	v1Store *checkpoint.GitStore,
	checkpointID id.CheckpointID,
	summary *checkpoint.CheckpointSummary,
	missingSessions []missingFullSessionForPacking,
) (map[string][]int, error) {
	neededSessionIDs := make(map[string]struct{})
	for _, session := range missingSessions {
		if session.sessionID != "" {
			neededSessionIDs[session.sessionID] = struct{}{}
		}
	}

	bySessionID := make(map[string][]int)
	if len(neededSessionIDs) == 0 {
		return bySessionID, nil
	}

	for sessionIdx := range len(summary.Sessions) {
		metadata, err := v1Store.ReadSessionMetadata(ctx, checkpointID, sessionIdx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("context canceled while reading v1 session metadata: %w", ctxErr)
			}
			continue
		}
		if _, ok := neededSessionIDs[metadata.SessionID]; ok {
			bySessionID[metadata.SessionID] = append(bySessionID[metadata.SessionID], sessionIdx)
		}
	}

	return bySessionID, nil
}

func readV1SessionForMissingFullArtifact(
	ctx context.Context,
	v1Store *checkpoint.GitStore,
	checkpointID id.CheckpointID,
	summary *checkpoint.CheckpointSummary,
	bySessionID map[string][]int,
	missingSession missingFullSessionForPacking,
) (v1SessionForPacking, bool, error) {
	var triedSessionIndexes map[int]struct{}
	if missingSession.sessionID != "" {
		indexes := bySessionID[missingSession.sessionID]
		triedSessionIndexes = make(map[int]struct{}, len(indexes))
		for i := len(indexes) - 1; i >= 0; i-- {
			sessionIdx := indexes[i]
			triedSessionIndexes[sessionIdx] = struct{}{}
			session, found, err := readV1SessionForPacking(ctx, v1Store, checkpointID, sessionIdx)
			if err != nil || found {
				return session, found, err
			}
		}
	}

	if missingSession.sessionIndex >= len(summary.Sessions) {
		return v1SessionForPacking{}, false, nil
	}
	if _, tried := triedSessionIndexes[missingSession.sessionIndex]; tried {
		return v1SessionForPacking{}, false, nil
	}
	return readV1SessionForPacking(ctx, v1Store, checkpointID, missingSession.sessionIndex)
}

func readV1SessionForPacking(
	ctx context.Context,
	v1Store *checkpoint.GitStore,
	checkpointID id.CheckpointID,
	sessionIdx int,
) (v1SessionForPacking, bool, error) {
	content, err := v1Store.ReadSessionContent(ctx, checkpointID, sessionIdx)
	if err != nil {
		if errors.Is(err, checkpoint.ErrNoTranscript) || errors.Is(err, checkpoint.ErrCheckpointNotFound) {
			return v1SessionForPacking{}, false, nil
		}
		return v1SessionForPacking{}, false, fmt.Errorf("failed to read v1 session %d while resuming migration: %w", sessionIdx, err)
	}

	return v1SessionForPacking{
		sessionIndex: sessionIdx,
		content:      content,
	}, true, nil
}

func backfillCompactTranscripts(ctx context.Context, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, info checkpoint.CommittedInfo, v2Summary *checkpoint.CheckpointSummary) (int, error) {
	missingSessions, skippedNoAgent, err := collectMissingCompactSessionsForBackfill(ctx, v2Store, info.CheckpointID, v2Summary)
	if err != nil {
		return 0, err
	}
	if len(missingSessions) == 0 {
		if skippedNoAgent {
			return 0, fmt.Errorf("%w: no agent type in metadata", errTranscriptNotGeneratable)
		}
		return 0, errAlreadyMigrated
	}

	v1Summary, err := v1Store.ReadCommitted(ctx, info.CheckpointID)
	if err != nil {
		return 0, fmt.Errorf("failed to read v1 summary while backfilling compact transcripts: %w", err)
	}
	if v1Summary == nil {
		return 0, fmt.Errorf("v1 checkpoint %s has no summary", info.CheckpointID)
	}

	v1BySessionID, err := collectV1SessionIndexesForPacking(ctx, v1Store, info.CheckpointID, v1Summary, missingSessions)
	if err != nil {
		return 0, err
	}

	backfilled := 0
	skippedBackfill := false
	var lastAgent string
	for _, missingSession := range missingSessions {
		v1Session, ok, readErr := readV1SessionForMissingFullArtifact(ctx, v1Store, info.CheckpointID, v1Summary, v1BySessionID, missingSession)
		if readErr != nil {
			logging.Warn(ctx, "transcript.jsonl backfill: could not read v1 session",
				slog.String("checkpoint_id", string(info.CheckpointID)),
				slog.Int("session_index", missingSession.sessionIndex),
				slog.String("error", readErr.Error()),
			)
			skippedBackfill = true
			continue
		}
		if !ok {
			logging.Warn(ctx, "transcript.jsonl backfill: no matching v1 session",
				slog.String("checkpoint_id", string(info.CheckpointID)),
				slog.Int("session_index", missingSession.sessionIndex),
			)
			skippedBackfill = true
			continue
		}

		content := v1Session.content
		if content.Metadata.Agent != "" {
			lastAgent = string(content.Metadata.Agent)
		}

		compacted := tryCompactTranscript(ctx, content.Transcript, content.Metadata)
		if compacted == nil {
			if len(content.Transcript) == 0 {
				logging.Warn(ctx, "transcript.jsonl backfill: empty transcript in v1",
					slog.String("checkpoint_id", string(info.CheckpointID)),
					slog.Int("session_index", missingSession.sessionIndex),
				)
			}
			skippedBackfill = true
			continue
		}

		updateErr := v2Store.UpdateCommitted(ctx, checkpoint.UpdateCommittedOptions{
			CheckpointID:      info.CheckpointID,
			SessionID:         content.Metadata.SessionID,
			CompactTranscript: compacted,
		})
		if updateErr != nil {
			logging.Warn(ctx, "transcript.jsonl backfill: failed to write to v2",
				slog.String("checkpoint_id", string(info.CheckpointID)),
				slog.Int("session_index", missingSession.sessionIndex),
				slog.String("error", updateErr.Error()),
			)
			skippedBackfill = true
			continue
		}

		backfilled++
	}

	if backfilled == 0 {
		if lastAgent != "" {
			return 0, fmt.Errorf("%w: agent %q", errTranscriptNotGeneratable, lastAgent)
		}
		if skippedNoAgent {
			return 0, fmt.Errorf("%w: no agent type in metadata", errTranscriptNotGeneratable)
		}
		return 0, errTranscriptNotGeneratable
	}
	if skippedBackfill {
		if lastAgent != "" {
			return backfilled, fmt.Errorf("%w: agent %q", errTranscriptNotGeneratable, lastAgent)
		}
		return backfilled, errTranscriptNotGeneratable
	}

	return backfilled, nil
}

func collectMissingCompactSessionsForBackfill(
	ctx context.Context,
	v2Store *checkpoint.V2GitStore,
	checkpointID id.CheckpointID,
	summary *checkpoint.CheckpointSummary,
) ([]missingFullSessionForPacking, bool, error) {
	var missingSessions []missingFullSessionForPacking
	skippedNoAgent := false
	for sessionIdx, session := range summary.Sessions {
		if session.Transcript != "" {
			continue
		}

		meta, err := v2Store.ReadSessionMetadata(ctx, checkpointID, sessionIdx)
		if err != nil {
			return nil, false, fmt.Errorf("failed to read v2 session %d metadata while backfilling compact transcript: %w", sessionIdx, err)
		}
		if meta.Agent == "" {
			skippedNoAgent = true
			continue
		}
		missingSessions = append(missingSessions, missingFullSessionForPacking{
			sessionIndex: sessionIdx,
			sessionID:    meta.SessionID,
		})
	}
	return missingSessions, skippedNoAgent, nil
}
