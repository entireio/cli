package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/transcript/compact"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
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

			ctx := cmd.Context()

			if _, err := paths.WorktreeRoot(ctx); err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run from within a git repository.")
				return NewSilentError(errors.New("not a git repository"))
			}

			logging.SetLogLevelGetter(GetLogLevel)
			if initErr := logging.Init(ctx, ""); initErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not initialize logging: %v\n", initErr)
			} else {
				defer logging.Close()
			}
			return runMigrateCheckpointsV2(ctx, cmd, forceFlag)
		},
	}

	cmd.Flags().StringVar(&checkpointsFlag, "checkpoints", "", "Target checkpoint format version (e.g., \"v2\")")
	cmd.Flags().BoolVar(&forceFlag, "force", false, "Force re-migration of all checkpoints, overwriting existing v2 data")

	return cmd
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

	result, freshlyPackedRefs, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, progressOut, force)
	if err != nil {
		return err
	}

	// Skip the generation-metadata repair pass on no-op reruns: it does a
	// `git ls-remote` plus a transcript-blob walk per archived /full/<n>,
	// minutes on big repos. When we did write archives, freshly-packed refs
	// are excluded — their generation.json is already correct from
	// AggregateTranscriptTimestamps in the packer.
	var repairResult *strategy.RepairV2GenerationMetadataResult
	if len(freshlyPackedRefs) > 0 {
		stopRepair := startSpinner(cmd.ErrOrStderr(), "Repairing archived generation metadata")
		var repairErr error
		repairResult, repairErr = strategy.RepairV2GenerationMetadata(ctx, freshlyPackedRefs)
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
	sessionIndex int
	content      *checkpoint.SessionContent
}

// migrateCheckpointsV2 returns the /full/<n> refs migration wrote so callers
// can pass them as exclusions to the generation-metadata repair pass.
func migrateCheckpointsV2(ctx context.Context, repo *git.Repository, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, progressOut io.Writer, force bool) (*migrateResult, []plumbing.ReferenceName, error) {
	v1List, err := v1Store.ListCommitted(ctx)
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
	fullArtifactsIndex, err := v2Store.BuildFullSessionArtifactsIndex()
	if err != nil {
		return nil, nil, fmt.Errorf("build v2 /full/* presence index: %w", err)
	}

	batchSize := migrateFullBatchSize()
	pendingFull := make([]migratedFullCheckpoint, 0, batchSize)
	var writtenRefs []plumbing.ReferenceName
	nextGeneration := 0

	for _, info := range v1List {
		fullCheckpoint, outcome, migrateErr := migrateOneCheckpoint(ctx, repo, v1Store, v2Store, info, force, fullArtifactsIndex)
		result.missingSessions += outcome.missingSessions
		if outcome.compactTranscriptSkipped {
			result.compactTranscriptSkipped++
		}

		if migrateErr != nil {
			switch {
			case errors.Is(migrateErr, errAlreadyMigrated):
				logCheckpointMigrationSkip(ctx, info.CheckpointID, "already in v2", migrateErr)
				result.skipped++
			case errors.Is(migrateErr, errNoMigratableSessions):
				logCheckpointMigrationSkip(ctx, info.CheckpointID, "no migratable v1 sessions", migrateErr)
				result.skipped++
			default:
				logging.Error(ctx, "checkpoint migration failed",
					slog.String("checkpoint_id", string(info.CheckpointID)),
					slog.String("error", migrateErr.Error()),
				)
				result.failed++
			}
			progress.Increment()
			continue
		}

		if fullCheckpoint != nil {
			pendingFull = append(pendingFull, *fullCheckpoint)
			if len(pendingFull) == batchSize {
				if nextGeneration == 0 {
					// Resolve the archive slot only when the first full batch is ready;
					// force migration may prune existing archived refs earlier in the loop.
					next, nextErr := v2Store.NextGenerationNumber()
					if nextErr != nil {
						return result, writtenRefs, fmt.Errorf("list archived v2 generations: %w", nextErr)
					}
					nextGeneration = next
				}
				refName := checkpoint.ArchivedGenerationRefName(nextGeneration)
				if packErr := writeMigratedFullGeneration(ctx, repo, refName, pendingFull); packErr != nil {
					return result, writtenRefs, fmt.Errorf("failed to pack migrated raw transcripts: %w", packErr)
				}
				writtenRefs = append(writtenRefs, refName)
				nextGeneration++
				pendingFull = make([]migratedFullCheckpoint, 0, batchSize)
			}
		}
		result.migrated++
		progress.Increment()
	}

	progress.Finish()
	stopFinalize := startSpinner(progressOut, "Packing migrated raw transcripts")
	if len(pendingFull) > 0 {
		if err := writeMigratedFinalFullCurrent(ctx, repo, v2Store, pendingFull); err != nil {
			stopFinalize(false)
			return result, writtenRefs, fmt.Errorf("failed to pack migrated raw transcripts: %w", err)
		}
		// If /full/current already had checkpoints, this final migration write can
		// briefly push the generation past the threshold before rotation. That
		// mirrors other v2 ref-merge cases where a generation may exceed the soft
		// threshold by a small amount.
		if refName, rotated, err := v2Store.RotateCurrentGenerationIfNeeded(ctx, batchSize); err != nil {
			stopFinalize(false)
			return result, writtenRefs, fmt.Errorf("failed to rotate migrated full/current generation: %w", err)
		} else if rotated {
			writtenRefs = append(writtenRefs, refName)
		}
	} else if len(writtenRefs) > 0 && !fullCurrentExistsBefore {
		if err := ensureEmptyV2FullCurrent(ctx, repo); err != nil {
			stopFinalize(false)
			return result, writtenRefs, fmt.Errorf("failed to pack migrated raw transcripts: %w", err)
		}
	}
	stopFinalize(true)

	return result, writtenRefs, nil
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

func migrateOneCheckpoint(ctx context.Context, repo *git.Repository, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, info checkpoint.CommittedInfo, force bool, fullArtifacts checkpoint.FullSessionArtifactsIndex) (*migratedFullCheckpoint, migrateCheckpointOutcome, error) {
	var outcome migrateCheckpointOutcome

	existing, err := v2Store.ReadCommitted(ctx, info.CheckpointID)
	if err != nil {
		return nil, outcome, fmt.Errorf("failed to check v2 for checkpoint %s: %w", info.CheckpointID, err)
	}

	if existing != nil && !force {
		// Already in v2. Pack sessions whose /full/* artifacts are missing
		// (resume an interrupted run) and backfill transcript.jsonl on /main
		// where it's missing. With nothing to do on either front, return
		// errAlreadyMigrated so the caller counts it as skipped.
		fullCheckpoint, err := collectMissingFullCheckpointForPacking(ctx, repo, v1Store, v2Store, info, existing, fullArtifacts)
		if err != nil && !errors.Is(err, errAlreadyMigrated) {
			return nil, outcome, err
		}
		backfilled, backfillErr := backfillCompactTranscripts(ctx, v1Store, v2Store, info, existing)
		if errors.Is(backfillErr, errTranscriptNotGeneratable) {
			outcome.compactTranscriptSkipped = true
		} else if backfillErr != nil && !errors.Is(backfillErr, errAlreadyMigrated) {
			return nil, outcome, backfillErr
		}
		if fullCheckpoint == nil {
			if backfilled > 0 {
				return nil, outcome, nil
			}
			return nil, outcome, errAlreadyMigrated
		}
		return fullCheckpoint, outcome, nil
	}

	if existing != nil && force {
		if pruneErr := pruneV2CheckpointForForce(ctx, repo, v2Store, info.CheckpointID); pruneErr != nil {
			return nil, outcome, fmt.Errorf("failed to reset existing v2 checkpoint %s before force migration: %w", info.CheckpointID, pruneErr)
		}
	}

	summary, err := v1Store.ReadCommitted(ctx, info.CheckpointID)
	if err != nil {
		return nil, outcome, fmt.Errorf("failed to read v1 summary: %w", err)
	}
	if summary == nil {
		return nil, outcome, fmt.Errorf("v1 checkpoint %s has no summary", info.CheckpointID)
	}

	compactFailed := false
	shouldCopyTaskMetadata := false
	skippedMissingSessions := 0
	migratedSessions := 0
	v1ToV2SessionIdx := make(map[int]int, len(summary.Sessions))
	fullCheckpoint := &migratedFullCheckpoint{
		checkpointID: info.CheckpointID,
	}

	for sessionIdx := range len(summary.Sessions) {
		content, skipped, readErr := readV1SessionForMigration(ctx, v1Store, info.CheckpointID, sessionIdx)
		if skipped {
			skippedMissingSessions++
			outcome.missingSessions++
			continue
		}
		if readErr != nil {
			return nil, outcome, fmt.Errorf("failed to read v1 session %d: %w", sessionIdx, readErr)
		}
		if content.Metadata.IsTask {
			shouldCopyTaskMetadata = true
		}

		opts := buildMigrateWriteOpts(content, info, summary.CombinedAttribution)

		compacted := tryCompactTranscript(ctx, content.Transcript, content.Metadata)
		if compacted != nil {
			opts.CompactTranscript = compacted
			opts.CompactTranscriptStart = computeCompactOffset(ctx, content.Transcript, compacted, content.Metadata)
		} else if len(content.Transcript) > 0 {
			compactFailed = true
		}

		mainOpts := opts
		mainOpts.Transcript = redact.AlreadyRedacted(nil)
		v2SessionIdx, writeErr := v2Store.WriteCommittedWithSessionIndex(ctx, mainOpts)
		if writeErr != nil {
			return nil, outcome, fmt.Errorf("failed to write v2 session %d: %w", sessionIdx, writeErr)
		}
		v1ToV2SessionIdx[sessionIdx] = v2SessionIdx
		fullCheckpoint.sessions = append(fullCheckpoint.sessions, migratedFullSession{
			sessionIndex: v2SessionIdx,
			content:      content,
		})
		migratedSessions++
	}

	if migratedSessions == 0 {
		return nil, outcome, fmt.Errorf("%w: v1 metadata lists %d session(s), but no transcript/session content exists for any of them", errNoMigratableSessions, len(summary.Sessions))
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

	return fullCheckpoint, outcome, nil
}

func migrateFullBatchSize() int {
	batchSize := migrateMaxCheckpointsPerGeneration
	if batchSize <= 0 {
		return checkpoint.DefaultMaxCheckpointsPerGeneration
	}
	return batchSize
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
	root := worktree.Filesystem.Root()
	if root == "" {
		return "", errors.New("repository worktree filesystem has no root path")
	}
	return root, nil
}

func writeMigratedFullGeneration(ctx context.Context, repo *git.Repository, refName plumbing.ReferenceName, checkpoints []migratedFullCheckpoint) error {
	fullEntries, err := buildMigratedFullEntrySet(ctx, repo, checkpoints)
	if err != nil {
		return fmt.Errorf("write migrated generation entries: %w", err)
	}

	entries := make(map[string]object.TreeEntry, len(fullEntries.rawEntries)+len(fullEntries.taskEntries))
	fullEntries.mergeInto(entries)
	treeHash, err := checkpoint.BuildTreeFromEntries(ctx, repo, entries)
	if err != nil {
		return fmt.Errorf("build migrated generation tree: %w", err)
	}

	v2Store := checkpoint.NewV2GitStore(repo, migrateRemoteName)
	// Reuse the transcripts already in memory rather than walking the tree
	// we just built — same first/last-event range, no redundant blob reads.
	gen, found := checkpoint.AggregateTranscriptTimestamps(migratedTranscripts(checkpoints))
	if !found {
		var err error
		gen, found, err = v2Store.ComputeGenerationCheckpointTimestamps(treeHash)
		if err != nil {
			return fmt.Errorf("compute checkpoint timestamps: %w", err)
		}
	}
	if !found {
		gen, found = generationMetadataFromMigratedSessions(checkpoints)
	}
	if !found {
		return fmt.Errorf("no timestamps found for migrated generation %s", refName)
	}

	treeHash, err = v2Store.AddGenerationJSONToTree(treeHash, gen)
	if err != nil {
		return fmt.Errorf("add generation metadata: %w", err)
	}

	commitHash, err := checkpoint.CreateCommit(ctx, repo, treeHash, plumbing.ZeroHash,
		fmt.Sprintf("Archive migrated generation: %s\n", refName),
		migrateAuthorName, migrateAuthorEmail)
	if err != nil {
		return fmt.Errorf("create migrated generation commit: %w", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)); err != nil {
		return fmt.Errorf("update migrated generation ref %s: %w", refName, err)
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

	count, err := v2Store.CountCheckpointsInTree(newRoot)
	if err != nil {
		return fmt.Errorf("failed to count checkpoints in pruned %s: %w", refName, err)
	}
	if count == 0 {
		if err := repo.Storer.RemoveReference(refName); err != nil {
			return fmt.Errorf("failed to remove empty archived v2 generation %s: %w", refName, err)
		}
		return nil
	}

	newRoot, err = addRecomputedGenerationJSON(v2Store, newRoot)
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

func addRecomputedGenerationJSON(v2Store *checkpoint.V2GitStore, treeHash plumbing.Hash) (plumbing.Hash, error) {
	gen, found, err := v2Store.ComputeGenerationTimestampsFromTrees(treeHash, nil)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("compute raw transcript timestamps: %w", err)
	}
	if !found {
		gen, found, err = v2Store.ComputeGenerationCheckpointTimestamps(treeHash)
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
			sessionIndex: missingSession.sessionIndex,
			content:      v1Session.content,
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
