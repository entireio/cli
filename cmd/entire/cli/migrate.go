package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/transcript/compact"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/spf13/cobra"
)

func newMigrateCmd() *cobra.Command {
	var checkpointsFlag string

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
			return runMigrateCheckpointsV2(ctx, cmd)
		},
	}

	cmd.Flags().StringVar(&checkpointsFlag, "checkpoints", "", "Target checkpoint format version (e.g., \"v2\")")

	return cmd
}

type migrateResult struct {
	migrated int
	skipped  int
	failed   int
}

func runMigrateCheckpointsV2(ctx context.Context, cmd *cobra.Command) error {
	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run from within a git repository.")
		return NewSilentError(err)
	}

	v1Store := checkpoint.NewGitStore(repo)
	v2Store := checkpoint.NewV2GitStore(repo, migrateRemoteName)
	out := cmd.OutOrStdout()

	result, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, out)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "\nMigration complete: %d migrated, %d skipped, %d failed\n",
		result.migrated, result.skipped, result.failed)

	if result.failed > 0 {
		fmt.Fprintf(out, "%d checkpoint(s) failed to migrate. Check .entire/logs/ for details.\n", result.failed)
		return NewSilentError(fmt.Errorf("%d checkpoint(s) failed to migrate", result.failed))
	}

	return nil
}

var (
	errAlreadyMigrated          = errors.New("already migrated")
	errTranscriptNotGeneratable = errors.New("transcript.jsonl could not be generated")
)

const migrateRemoteName = "origin"

func migrateCheckpointsV2(ctx context.Context, repo *git.Repository, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, out io.Writer) (*migrateResult, error) {
	v1List, err := v1Store.ListCommitted(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list v1 checkpoints: %w", err)
	}

	if len(v1List) == 0 {
		fmt.Fprintln(out, "Nothing to migrate: no v1 checkpoints found")
		return &migrateResult{}, nil
	}

	fmt.Fprintln(out, "Migrating v1 checkpoints to v2...")
	total := len(v1List)
	result := &migrateResult{}

	for i, info := range v1List {
		prefix := fmt.Sprintf("  [%d/%d] Migrating checkpoint %s...", i+1, total, info.CheckpointID)

		if migrateErr := migrateOneCheckpoint(ctx, repo, v1Store, v2Store, info, out, prefix); migrateErr != nil {
			switch {
			case errors.Is(migrateErr, errAlreadyMigrated):
				fmt.Fprintf(out, "%s skipped (already in v2)\n", prefix)
				result.skipped++
			case errors.Is(migrateErr, errTranscriptNotGeneratable):
				fmt.Fprintf(out, "%s in v2, but %s\n", prefix, migrateErr.Error())
				result.skipped++
			default:
				fmt.Fprintf(out, "%s failed\n", prefix)
				logging.Error(ctx, "checkpoint migration failed",
					slog.String("checkpoint_id", string(info.CheckpointID)),
					slog.String("error", migrateErr.Error()),
				)
				result.failed++
			}
			continue
		}

		result.migrated++
	}

	return result, nil
}

func migrateOneCheckpoint(ctx context.Context, repo *git.Repository, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, info checkpoint.CommittedInfo, out io.Writer, prefix string) error {
	existing, err := v2Store.ReadCommitted(ctx, info.CheckpointID)
	if err != nil {
		return fmt.Errorf("failed to check v2 for checkpoint %s: %w", info.CheckpointID, err)
	}

	// Already in v2 — check if any aspect of sessions are missing and backfill
	if existing != nil {
		repaired, repairErr := repairPartialV2Checkpoint(ctx, repo, v1Store, v2Store, info, existing)
		if repairErr != nil {
			return repairErr
		}

		currentV2, readCurrentErr := v2Store.ReadCommitted(ctx, info.CheckpointID)
		if readCurrentErr != nil {
			return fmt.Errorf("failed to re-read v2 checkpoint %s: %w", info.CheckpointID, readCurrentErr)
		}
		if currentV2 == nil {
			return fmt.Errorf("v2 checkpoint %s disappeared during migration", info.CheckpointID)
		}

		backfillErr := backfillCompactTranscripts(ctx, v1Store, v2Store, info, currentV2, out, prefix)
		if errors.Is(backfillErr, errAlreadyMigrated) && repaired {
			fmt.Fprintf(out, "%s repaired partial v2 checkpoint state\n", prefix)
			return nil
		}
		if errors.Is(backfillErr, errTranscriptNotGeneratable) && repaired {
			fmt.Fprintf(out, "%s repaired partial v2 checkpoint state (compact transcript not generated)\n", prefix)
			return nil
		}
		return backfillErr
	}

	summary, err := v1Store.ReadCommitted(ctx, info.CheckpointID)
	if err != nil {
		return fmt.Errorf("failed to read v1 summary: %w", err)
	}
	if summary == nil {
		return fmt.Errorf("v1 checkpoint %s has no summary", info.CheckpointID)
	}

	compactFailed := false
	shouldCopyTaskMetadata := false

	for sessionIdx := range len(summary.Sessions) {
		content, readErr := v1Store.ReadSessionContent(ctx, info.CheckpointID, sessionIdx)
		if readErr != nil {
			return fmt.Errorf("failed to read v1 session %d: %w", sessionIdx, readErr)
		}
		if content.Metadata.IsTask {
			shouldCopyTaskMetadata = true
		}

		opts := buildMigrateWriteOpts(content, info)

		compacted := tryCompactTranscript(ctx, content.Transcript, content.Metadata)
		if compacted != nil {
			opts.CompactTranscript = compacted
		} else if len(content.Transcript) > 0 {
			compactFailed = true
		}

		if writeErr := v2Store.WriteCommitted(ctx, opts); writeErr != nil {
			return fmt.Errorf("failed to write v2 session %d: %w", sessionIdx, writeErr)
		}
	}

	// Copy task metadata trees from v1 to v2 /full/current
	if shouldCopyTaskMetadata {
		if taskErr := copyTaskMetadataToV2(repo, v1Store, v2Store, info.CheckpointID, summary); taskErr != nil {
			logging.Warn(ctx, "failed to copy task metadata to v2",
				slog.String("checkpoint_id", string(info.CheckpointID)),
				slog.String("error", taskErr.Error()),
			)
		}
	}

	if compactFailed {
		fmt.Fprintf(out, "%s done (compact transcript not generated)\n", prefix)
	} else {
		fmt.Fprintf(out, "%s done\n", prefix)
	}

	return nil
}

func repairPartialV2Checkpoint(ctx context.Context, repo *git.Repository, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, info checkpoint.CommittedInfo, v2Summary *checkpoint.CheckpointSummary) (bool, error) {
	repaired := false

	// Spot-check already present sessions: ensure required /full/current artifacts exist.
	existingSessionCount := len(v2Summary.Sessions)
	for sessionIdx := range existingSessionCount {
		ok, checkErr := hasCurrentFullSessionArtifacts(repo, v2Store, info.CheckpointID, sessionIdx)
		if checkErr != nil {
			return false, fmt.Errorf("failed to check v2 session %d artifacts: %w", sessionIdx, checkErr)
		}
		if ok {
			continue
		}

		content, readErr := v1Store.ReadSessionContent(ctx, info.CheckpointID, sessionIdx)
		if readErr != nil {
			return false, fmt.Errorf("failed to read v1 session %d while repairing v2: %w", sessionIdx, readErr)
		}

		updateOpts := checkpoint.UpdateCommittedOptions{
			CheckpointID: info.CheckpointID,
			SessionID:    content.Metadata.SessionID,
			Transcript:   content.Transcript,
			Prompts:      checkpoint.SplitPromptContent(content.Prompts),
			Agent:        content.Metadata.Agent,
		}
		if compacted := tryCompactTranscript(ctx, content.Transcript, content.Metadata); compacted != nil {
			updateOpts.CompactTranscript = compacted
		}

		if updateErr := v2Store.UpdateCommitted(ctx, updateOpts); updateErr != nil {
			return false, fmt.Errorf("failed to repair v2 session %d: %w", sessionIdx, updateErr)
		}
		repaired = true
	}

	return repaired, nil
}

func hasCurrentFullSessionArtifacts(repo *git.Repository, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID, sessionIdx int) (bool, error) {
	_, rootTreeHash, err := v2Store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	if err != nil {
		return false, nil //nolint:nilerr // Missing /full/current ref means required artifacts are absent.
	}

	rootTree, err := repo.TreeObject(rootTreeHash)
	if err != nil {
		return false, fmt.Errorf("failed to read /full/current tree: %w", err)
	}

	sessionPath := fmt.Sprintf("%s/%d", cpID.Path(), sessionIdx)
	sessionTree, err := rootTree.Tree(sessionPath)
	if err != nil {
		return false, nil //nolint:nilerr // Missing session path means artifacts are absent, not a hard error.
	}

	hasTranscript := false
	for _, entry := range sessionTree.Entries {
		if entry.Name == paths.TranscriptFileName || strings.HasPrefix(entry.Name, paths.TranscriptFileName+".") {
			hasTranscript = true
			break
		}
	}
	if !hasTranscript {
		return false, nil
	}

	if _, err := sessionTree.File(paths.ContentHashFileName); err != nil {
		return false, nil //nolint:nilerr // Missing content hash indicates incomplete /full/current artifacts.
	}

	return true, nil
}

// backfillCompactTranscripts checks sessions in an already-migrated v2 checkpoint
// for missing transcript.jsonl and attempts to generate + write them from v1 data.
// Returns errAlreadyMigrated if all sessions already have compact transcripts.
func backfillCompactTranscripts(ctx context.Context, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, info checkpoint.CommittedInfo, v2Summary *checkpoint.CheckpointSummary, out io.Writer, prefix string) error {
	// Find sessions missing transcript.jsonl
	var needsBackfill []int
	for i, session := range v2Summary.Sessions {
		if session.Transcript == "" {
			needsBackfill = append(needsBackfill, i)
		}
	}

	if len(needsBackfill) == 0 {
		return errAlreadyMigrated
	}

	backfilled := 0
	var lastAgent string

	for _, sessionIdx := range needsBackfill {
		content, readErr := v1Store.ReadSessionContent(ctx, info.CheckpointID, sessionIdx)
		if readErr != nil {
			logging.Warn(ctx, "transcript.jsonl backfill: could not read v1 session",
				slog.String("checkpoint_id", string(info.CheckpointID)),
				slog.Int("session_index", sessionIdx),
				slog.String("error", readErr.Error()),
			)
			continue
		}

		if content.Metadata.Agent != "" {
			lastAgent = string(content.Metadata.Agent)
		}

		compacted := tryCompactTranscript(ctx, content.Transcript, content.Metadata)
		if compacted == nil {
			// tryCompactTranscript already logs for no-agent and compact-error cases;
			// log the empty-transcript case here.
			if len(content.Transcript) == 0 {
				logging.Warn(ctx, "transcript.jsonl backfill: empty transcript in v1",
					slog.String("checkpoint_id", string(info.CheckpointID)),
					slog.Int("session_index", sessionIdx),
				)
			}
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
				slog.Int("session_index", sessionIdx),
				slog.String("error", updateErr.Error()),
			)
			continue
		}

		backfilled++
	}

	if backfilled == 0 {
		if lastAgent != "" {
			return fmt.Errorf("%w: agent %q", errTranscriptNotGeneratable, lastAgent)
		}
		return fmt.Errorf("%w: no agent type in metadata", errTranscriptNotGeneratable)
	}

	fmt.Fprintf(out, "%s added transcript.jsonl for %d session(s)\n", prefix, backfilled)

	return nil
}

func buildMigrateWriteOpts(content *checkpoint.SessionContent, info checkpoint.CommittedInfo) checkpoint.WriteCommittedOptions {
	m := content.Metadata

	prompts := checkpoint.SplitPromptContent(content.Prompts)

	return checkpoint.WriteCommittedOptions{
		CheckpointID:                info.CheckpointID,
		SessionID:                   m.SessionID,
		Strategy:                    m.Strategy,
		Branch:                      m.Branch,
		Transcript:                  content.Transcript,
		Prompts:                     prompts,
		FilesTouched:                m.FilesTouched,
		CheckpointsCount:            m.CheckpointsCount,
		Agent:                       m.Agent,
		Model:                       m.Model,
		TurnID:                      m.TurnID,
		TokenUsage:                  m.TokenUsage,
		SessionMetrics:              m.SessionMetrics,
		InitialAttribution:          m.InitialAttribution,
		Summary:                     m.Summary,
		CheckpointTranscriptStart:   m.GetTranscriptStart(),
		TranscriptIdentifierAtStart: m.TranscriptIdentifierAtStart,
		IsTask:                      m.IsTask,
		ToolUseID:                   m.ToolUseID,
		AuthorName:                  "Entire Migration",
		AuthorEmail:                 "migration@entire.dev",
	}
}

func tryCompactTranscript(ctx context.Context, transcript []byte, m checkpoint.CommittedMetadata) []byte {
	if len(transcript) == 0 {
		return nil
	}
	if m.Agent == "" {
		logging.Warn(ctx, "compact transcript skipped: no agent type in checkpoint metadata",
			slog.String("checkpoint_id", string(m.CheckpointID)),
		)
		return nil
	}

	compacted, err := compact.Compact(transcript, compact.MetadataFields{
		Agent:      string(m.Agent),
		CLIVersion: versioninfo.Version,
		StartLine:  m.GetTranscriptStart(),
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

// copyTaskMetadataToV2 copies task metadata files (subagent transcripts, checkpoint JSONs)
// from the v1 branch to the v2 /full/current ref via tree surgery.
func copyTaskMetadataToV2(repo *git.Repository, _ *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID, summary *checkpoint.CheckpointSummary) error {
	// Resolve the v1 branch tree
	v1Tree, err := resolveV1CheckpointTree(repo, cpID)
	if err != nil {
		return err
	}

	// Legacy v1 layout stores task metadata at checkpoint root: <cp>/tasks/<tool-use-id>/...
	// Prefer attaching this tree to the latest session in v2.
	if rootTasksTree, rootTasksErr := v1Tree.Tree("tasks"); rootTasksErr == nil {
		if len(summary.Sessions) > 0 {
			latestSessionIdx := len(summary.Sessions) - 1
			if spliceErr := spliceTasksTreeToV2(repo, v2Store, cpID, latestSessionIdx, rootTasksTree.Hash); spliceErr != nil {
				return fmt.Errorf("latest session task tree splice failed: %w", spliceErr)
			}
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
			continue // No tasks directory in this session
		}

		if spliceErr := spliceTasksTreeToV2(repo, v2Store, cpID, sessionIdx, tasksTree.Hash); spliceErr != nil {
			return fmt.Errorf("session %d task tree splice failed: %w", sessionIdx, spliceErr)
		}
	}

	return nil
}

// resolveV1CheckpointTree reads the checkpoint subtree from the v1 branch.
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

func spliceTasksTreeToV2(repo *git.Repository, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID, sessionIdx int, tasksTreeHash plumbing.Hash) error {
	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	parentHash, rootTreeHash, err := v2Store.GetRefState(refName)
	if err != nil {
		return fmt.Errorf("failed to get v2 ref state: %w", err)
	}
	incomingTasksTree, err := repo.TreeObject(tasksTreeHash)
	if err != nil {
		return fmt.Errorf("failed to read tasks tree: %w", err)
	}

	shardPrefix := string(cpID[:2])
	shardSuffix := string(cpID[2:])
	sessionDir := strconv.Itoa(sessionIdx)

	newRoot, err := checkpoint.UpdateSubtree(repo, rootTreeHash,
		[]string{shardPrefix, shardSuffix, sessionDir, "tasks"},
		incomingTasksTree.Entries,
		checkpoint.UpdateSubtreeOptions{MergeMode: checkpoint.MergeKeepExisting},
	)
	if err != nil {
		return fmt.Errorf("tree surgery failed: %w", err)
	}

	commitHash, err := checkpoint.CreateCommit(repo, newRoot, parentHash,
		fmt.Sprintf("Add task metadata for %s\n", cpID),
		"Entire Migration", "migration@entire.dev")
	if err != nil {
		return fmt.Errorf("failed to create commit: %w", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)); err != nil {
		return fmt.Errorf("failed to update ref %s: %w", refName, err)
	}
	return nil
}
