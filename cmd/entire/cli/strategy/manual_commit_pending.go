package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/cmd/entire/cli/validation"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// ListPendingCheckpoints returns the session's pending (not yet condensed) checkpoints.
// Uses checkpoint.EphemeralStore for reading from shadow branches.
func (s *ManualCommitStrategy) ListPendingCheckpoints(ctx context.Context, limit int) ([]PendingCheckpoint, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	store, err := s.getEphemeralStore(ctx, repo)
	if err != nil {
		return nil, err
	}

	// Get current HEAD to find matching shadow branch
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	// Find sessions for current HEAD
	sessions, err := s.findSessionsForCommit(ctx, head.Hash().String())
	if err != nil {
		// Log error but continue to check for logs-only points
		sessions = nil
	}

	var allPoints []PendingCheckpoint

	// Collect checkpoint points from active sessions using temporary storage.
	// Cache session prompts by session ID to avoid re-reading the same prompt file
	sessionPrompts := make(map[string]string)

	for _, state := range sessions {
		checkpoints, err := store.ListCheckpoints(ctx, state.BaseCommit, state.WorktreeID, state.SessionID, limit)
		if err != nil {
			continue // Error reading checkpoints, skip this session
		}

		for _, cp := range checkpoints {
			// Get session prompt (cached by session ID)
			sessionPrompt, ok := sessionPrompts[cp.SessionID]
			if !ok {
				sessionPrompt = readSessionPrompt(repo, cp.CommitHash, cp.MetadataDir)
				sessionPrompts[cp.SessionID] = sessionPrompt
			}

			allPoints = append(allPoints, PendingCheckpoint{
				ID:               cp.CommitHash.String(),
				Message:          cp.Message,
				MetadataDir:      cp.MetadataDir,
				Date:             cp.Timestamp,
				IsTaskCheckpoint: cp.IsTaskCheckpoint,
				ToolUseID:        cp.ToolUseID,
				SessionID:        cp.SessionID,
				SessionPrompt:    sessionPrompt,
				Agent:            state.AgentType,
			})
		}

		// [Task] rows come from task records (#2058) — live and completed-unmaterialized
		// entries are both pending until condensed; no shadow commit exists, so no ID.
		for _, rec := range state.TaskRecords {
			date := rec.CompletedAt
			if date.IsZero() {
				date = rec.StartedAt
			}
			shortToolUseID := rec.ToolUseID
			if len(shortToolUseID) > id.ShortIDLength {
				shortToolUseID = shortToolUseID[:id.ShortIDLength]
			}
			message := FormatSubagentEndMessage(rec.SubagentType, rec.TaskDescription, shortToolUseID)
			if rec.CompletedAt.IsZero() {
				message = FormatSubagentRunningMessage(rec.SubagentType, rec.TaskDescription, shortToolUseID)
			}
			allPoints = append(allPoints, PendingCheckpoint{
				Message:          message,
				Date:             date,
				IsTaskCheckpoint: true,
				ToolUseID:        rec.ToolUseID,
				SessionID:        state.SessionID,
				SessionPrompt:    state.LastPrompt,
				Agent:            state.AgentType,
			})
		}
	}

	// Sort by date, most recent first
	sort.Slice(allPoints, func(i, j int) bool {
		return allPoints[i].Date.After(allPoints[j].Date)
	})

	if len(allPoints) > limit {
		allPoints = allPoints[:limit]
	}

	// Also include logs-only points from commit history
	logsOnlyPoints, err := s.ListLogsOnlyPendingCheckpoints(ctx, limit)
	if err == nil && len(logsOnlyPoints) > 0 {
		// Build set of existing point IDs for deduplication
		existingIDs := make(map[string]bool)
		for _, p := range allPoints {
			existingIDs[p.ID] = true
		}

		// Add logs-only points that aren't already in the list
		for _, p := range logsOnlyPoints {
			if !existingIDs[p.ID] {
				allPoints = append(allPoints, p)
			}
		}

		// Re-sort by date
		sort.Slice(allPoints, func(i, j int) bool {
			return allPoints[i].Date.After(allPoints[j].Date)
		})

		// Re-trim to limit
		if len(allPoints) > limit {
			allPoints = allPoints[:limit]
		}
	}

	return allPoints, nil
}

// ListLogsOnlyPendingCheckpoints finds commits in the current branch's history that have
// condensed session logs in committed checkpoint storage. These are commits that
// were created with session data but the shadow branch has been condensed.
//
// The function works by:
// 1. Getting all checkpoints from committed checkpoint storage
// 2. Building a map of checkpoint ID -> checkpoint info
// 3. Scanning the current branch history for commits with Entire-Checkpoint trailers
// 4. Matching by checkpoint ID (stable across amend/rebase)
func (s *ManualCommitStrategy) ListLogsOnlyPendingCheckpoints(ctx context.Context, limit int) ([]PendingCheckpoint, error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, err
	}
	defer repo.Close()

	// Get all checkpoints from committed checkpoint storage
	checkpoints, err := s.listCheckpoints(ctx)
	if err != nil {
		// No checkpoints yet is fine
		return nil, nil
	}

	if len(checkpoints) == 0 {
		return nil, nil
	}

	// Build map of checkpoint ID -> checkpoint info
	// Checkpoint ID is the stable link from Entire-Checkpoint trailer
	checkpointInfoMap := make(map[id.CheckpointID]CheckpointInfo)
	for _, cp := range checkpoints {
		if !cp.CheckpointID.IsEmpty() {
			checkpointInfoMap[cp.CheckpointID] = cp
		}
	}

	// Get committed metadata read tree for session prompts (best-effort, ignore errors)
	readRef := cpkg.ResolveRefs(ctx).Read
	metadataTree, _ := GetMetadataRefTree(repo, readRef) //nolint:errcheck // Best-effort for session prompts

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}

	// Use LogOptions with Order=LogOrderCommitterTime to traverse all parents of merge commits.
	iter, err := repo.Log(&git.LogOptions{
		From:  head.Hash(),
		Order: git.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get commit log: %w", err)
	}

	var points []PendingCheckpoint
	count := 0

	err = iter.ForEach(func(c *object.Commit) error {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // Propagating context cancellation
		}
		if count >= logsOnlyScanLimit {
			return errStop
		}
		count++

		// Extract all checkpoint IDs from Entire-Checkpoint trailers.
		// Squash merge commits may contain multiple trailers from the original commits.
		allCpIDs := trailers.ParseAllCheckpoints(c.Message)
		if len(allCpIDs) == 0 {
			return nil
		}

		// Resolve to the latest checkpoint by creation time (consistent with `entire resume`).
		cpInfo, found := ResolveLatestCheckpointFromMap(allCpIDs, checkpointInfoMap)
		if !found {
			return nil
		}

		// Create logs-only pending checkpoint
		message := strings.Split(c.Message, "\n")[0]

		// Read session prompts from metadata tree
		var sessionPrompt string
		var sessionPrompts []string
		if metadataTree != nil {
			checkpointPath := cpInfo.CheckpointID.Path()
			// For multi-session checkpoints, read all prompts
			if cpInfo.SessionCount > 1 && len(cpInfo.SessionIDs) > 1 {
				sessionPrompts = ReadAllSessionPromptsFromTree(metadataTree, checkpointPath, cpInfo.SessionCount, cpInfo.SessionIDs)
				// Prefer the latest non-empty prompt: the most-recent session may
				// have been recorded without a prompt, but an earlier one usually has one.
				for i := len(sessionPrompts) - 1; i >= 0; i-- {
					if sessionPrompts[i] != "" {
						sessionPrompt = sessionPrompts[i]
						break
					}
				}
			} else {
				sessionPrompt = ReadLatestSessionPromptFromCommittedTree(metadataTree, cpInfo.CheckpointID, cpInfo.SessionCount)
				if sessionPrompt == "" {
					sessionPrompt = ReadSessionPromptFromTree(metadataTree, checkpointPath)
				}
				if sessionPrompt != "" {
					sessionPrompts = []string{sessionPrompt}
				}
			}
		}

		points = append(points, PendingCheckpoint{
			ID:             c.Hash.String(),
			Message:        message,
			Date:           c.Author.When,
			IsLogsOnly:     true,
			CheckpointID:   cpInfo.CheckpointID,
			Agent:          cpInfo.Agent,
			SessionID:      cpInfo.SessionID,
			SessionPrompt:  sessionPrompt,
			SessionCount:   cpInfo.SessionCount,
			SessionIDs:     cpInfo.SessionIDs,
			SessionPrompts: sessionPrompts,
		})

		return nil
	})

	if err != nil && !errors.Is(err, errStop) {
		return nil, fmt.Errorf("error iterating commits: %w", err)
	}

	if len(points) > limit {
		points = points[:limit]
	}

	return points, nil
}

// ResolveLatestCheckpointFromMap picks the checkpoint with the latest CreatedAt
// from a list of checkpoint IDs. Filters to IDs present in infoMap, then picks
// the one with the most recent CreatedAt.
func ResolveLatestCheckpointFromMap(cpIDs []id.CheckpointID, infoMap map[id.CheckpointID]CheckpointInfo) (CheckpointInfo, bool) {
	var found bool
	var latest CheckpointInfo
	for _, cpID := range cpIDs {
		info, ok := infoMap[cpID]
		if !ok {
			continue
		}
		if !found || info.CreatedAt.After(latest.CreatedAt) {
			latest = info
			found = true
		}
	}
	return latest, found
}

// RestoreLogsOnly restores session logs from a logs-only pending checkpoint.
// This fetches the transcript from entire/checkpoints/v1 and writes it to the agent's session directory.
// Does not modify the working directory.
// When multiple sessions were condensed to the same checkpoint, ALL sessions are restored.
// If force is false, prompts for confirmation when local logs have newer timestamps.
// Returns info about each restored session so callers can print correct per-session resume commands.
func (s *ManualCommitStrategy) RestoreLogsOnly(ctx context.Context, w, errW io.Writer, point PendingCheckpoint, force bool) ([]RestoredSession, error) {
	if !point.IsLogsOnly {
		return nil, errors.New("not a logs-only pending checkpoint")
	}

	if point.CheckpointID.IsEmpty() {
		return nil, errors.New("missing checkpoint ID")
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	WarnIfMetadataDisconnected(ctx)
	stores, err := cpkg.Open(ctx, repo, cpkg.OpenOptions{BlobFetcher: s.blobFetcher, ReadRemotes: CheckpointReadRemotes(ctx)})
	if err != nil {
		return nil, fmt.Errorf("open checkpoint store: %w", err)
	}
	store := stores.Persistent
	summary, err := cpkg.ReadCheckpoint(ctx, store, point.CheckpointID)
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}
	// Get worktree root for agent session directory lookup
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get worktree root: %w", err)
	}

	// By default (no --force), never overwrite a session log that already exists
	// locally: the on-disk transcript is the live session being resumed, so we
	// keep it and only restore logs that are missing. The write loop below skips
	// any session whose ID is in skipExisting but still reports it so the caller
	// prints its resume command. --force overwrites everything from the checkpoint.
	skipExisting := map[string]bool{}
	if !force {
		for _, sess := range s.classifySessionsForRestore(ctx, repoRoot, store, point.CheckpointID, summary) {
			if sess.Status != StatusNew {
				skipExisting[sess.SessionID] = true
			}
		}
	}

	// Count sessions to restore
	totalSessions := len(summary.Sessions)
	if totalSessions > 1 {
		fmt.Fprintf(w, "Restoring %d sessions from checkpoint:\n", totalSessions)
	}

	// Restore all sessions (oldest to newest, using 0-based indexing)
	var restored []RestoredSession
	for i := range totalSessions {
		content, readErr := store.ReadSessionContent(ctx, point.CheckpointID, i)
		if readErr != nil {
			if !errors.Is(readErr, cpkg.ErrNoTranscript) {
				fmt.Fprintf(errW, "  Warning: failed to read session %d: %v\n", i, readErr)
			}
			continue
		}
		if content == nil || len(content.Transcript) == 0 {
			continue
		}

		sessionID := content.Metadata.SessionID
		if sessionID == "" {
			fmt.Fprintf(errW, "  Warning: session %d has no session ID, skipping\n", i)
			continue
		}
		// Checkpoint metadata comes from the shared entire/checkpoints/v1 branch
		// and is attacker-influenceable. Reject path separators/absolute IDs before
		// they reach ResolveSessionFile + Session, which would otherwise let a
		// crafted session ID overwrite files outside the agent session directory.
		if err := validation.ValidateSessionID(sessionID); err != nil {
			fmt.Fprintf(errW, "  Warning: session %d has unsafe session ID %q, skipping: %v\n", i, sessionID, err)
			continue
		}

		// Resolve per-session agent from metadata — skip if agent is unknown
		if content.Metadata.Agent == "" {
			fmt.Fprintf(errW, "  Warning: session %d (%s) has no agent metadata, skipping (cannot determine target directory)\n", i, sessionID)
			continue
		}
		sessionAgent, agErr := ResolveAgentForResume(content.Metadata.Agent)
		if agErr != nil {
			fmt.Fprintf(errW, "  Warning: session %d (%s) has unknown agent %q, skipping\n", i, sessionID, content.Metadata.Agent)
			continue
		}

		// Compute transcript path from current repo location for cross-machine portability.
		sessionAgentDir, dirErr := sessionAgent.GetSessionDir(repoRoot)
		if dirErr != nil {
			fmt.Fprintf(errW, "  Warning: failed to get session dir for session %d: %v\n", i, dirErr)
			continue
		}
		sessionFile := sessionAgent.ResolveSessionFile(sessionAgentDir, sessionID)
		if resolver, ok := sessionAgent.(agent.RestoredSessionPathResolver); ok {
			resolvedFile, resolveErr := resolver.ResolveRestoredSessionFile(sessionAgentDir, sessionID, content.Transcript)
			if resolveErr != nil {
				fmt.Fprintf(errW, "  Warning: failed to resolve restored session path for session %d (%s): %v (using fallback path)\n", i, sessionID, resolveErr)
			} else {
				sessionFile = resolvedFile
			}
		}

		promptPreview := restoredPromptPreview(sessionAgent, content.Prompts, content.Transcript, content.Metadata.ReviewPrompt)

		// Local log already present and not forcing: keep it untouched, but still
		// report the session so the caller can print its resume command.
		if skipExisting[sessionID] {
			if totalSessions > 1 {
				fmt.Fprintf(w, "  Session %d: keeping existing local log\n", i+1)
			} else {
				fmt.Fprintf(w, "Keeping existing local session log\n")
			}
			restored = append(restored, RestoredSession{
				SessionID:    sessionID,
				CheckpointID: point.CheckpointID.String(),
				Agent:        sessionAgent.Type(),
				Prompt:       promptPreview,
				CreatedAt:    content.Metadata.CreatedAt,
				Kind:         content.Metadata.Kind,
				ReviewPrompt: content.Metadata.ReviewPrompt,
			})
			continue
		}

		if totalSessions > 1 {
			isLatest := i == totalSessions-1
			if promptPreview != "" {
				if isLatest {
					fmt.Fprintf(w, "  Session %d (latest): %s\n", i+1, promptPreview)
				} else {
					fmt.Fprintf(w, "  Session %d: %s\n", i+1, promptPreview)
				}
			}
			fmt.Fprintf(w, "    Writing to: %s\n", sessionFile)
		} else {
			fmt.Fprintf(w, "Writing transcript to: %s\n", sessionFile)
		}

		// Ensure parent directory exists (session file may be in a different dir than sessionAgentDir)
		if mkdirErr := os.MkdirAll(filepath.Dir(sessionFile), 0o750); mkdirErr != nil {
			fmt.Fprintf(errW, "    Warning: failed to create directory: %v\n", mkdirErr)
			continue
		}

		agentSession := &agent.AgentSession{
			SessionID:  sessionID,
			AgentName:  sessionAgent.Name(),
			RepoPath:   repoRoot,
			SessionRef: sessionFile,
			NativeData: content.Transcript,
		}
		if writeErr := sessionAgent.WriteSession(ctx, agentSession); writeErr != nil {
			if totalSessions > 1 {
				fmt.Fprintf(errW, "    Warning: failed to write session: %v\n", writeErr)
				continue
			}
			return nil, fmt.Errorf("failed to write session: %w", writeErr)
		}

		restored = append(restored, RestoredSession{
			SessionID:    sessionID,
			CheckpointID: point.CheckpointID.String(),
			Agent:        sessionAgent.Type(),
			Prompt:       promptPreview,
			CreatedAt:    content.Metadata.CreatedAt,
			Kind:         content.Metadata.Kind,
			ReviewPrompt: content.Metadata.ReviewPrompt,
		})
	}

	return restored, nil
}

func restoredPromptPreview(sessionAgent agent.Agent, promptContent string, transcript []byte, reviewPrompt string) string {
	if prompt := ExtractFirstPrompt(promptContent); prompt != "" {
		return prompt
	}
	if prompt := strings.TrimSpace(reviewPrompt); prompt != "" {
		return prompt
	}
	extractor, ok := sessionAgent.(agent.PromptExtractor)
	if !ok || len(transcript) == 0 {
		return ""
	}
	prompts, err := extractPromptsFromTranscriptBytes(extractor, transcript)
	if err != nil || len(prompts) == 0 {
		return ""
	}
	if first := FirstDisplayPrompt(prompts); first != "" {
		return TruncateDescription(first, MaxDescriptionLength)
	}
	return ""
}

// FirstDisplayPrompt returns the first prompt in the list worth showing as a
// title/preview: the first entry that is non-empty, not separator-only, and not
// agent-injected (runtime preambles, AGENTS.md dumps — see
// textutil.IsInjectedPrompt). Returns "" if none qualifies, which callers that
// must show something should treat as "fall back to the raw first prompt".
//
// The returned text is not truncated; callers that render it into a fixed-width
// display are responsible for that (see restoredPromptPreview).
func FirstDisplayPrompt(prompts []string) string {
	for _, prompt := range prompts {
		cleaned := strings.TrimSpace(prompt)
		if cleaned == "" || isOnlySeparators(cleaned) || textutil.IsInjectedPrompt(cleaned) {
			continue
		}
		return cleaned
	}
	return ""
}

func extractPromptsFromTranscriptBytes(extractor agent.PromptExtractor, transcript []byte) ([]string, error) {
	tmp, err := os.CreateTemp("", "entire-restored-transcript-*.jsonl")
	if err != nil {
		return nil, fmt.Errorf("create temporary transcript: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(transcript); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write temporary transcript: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close temporary transcript: %w", err)
	}
	prompts, err := extractor.ExtractPrompts(tmpPath, 0)
	if err != nil {
		return nil, fmt.Errorf("extract prompts from temporary transcript: %w", err)
	}
	return prompts, nil
}

// ResolveAgentForResume resolves the agent from checkpoint metadata.
func ResolveAgentForResume(agentType types.AgentType) (agent.Agent, error) {
	ag, err := agent.GetByAgentType(agentType)
	if err != nil {
		return nil, fmt.Errorf("resolving agent %q: %w", agentType, err)
	}
	return ag, nil
}

// readSessionPrompt reads the first prompt from the session's prompt.txt file stored in git.
// Returns an empty string if the prompt cannot be read.
func readSessionPrompt(repo *git.Repository, commitHash plumbing.Hash, metadataDir string) string {
	// Get the commit and its tree
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		return ""
	}

	tree, err := commit.Tree()
	if err != nil {
		return ""
	}

	// Look for prompt.txt in the metadata directory
	promptPath := metadataDir + "/" + paths.PromptFileName
	promptEntry, err := tree.File(promptPath)
	if err != nil {
		return ""
	}

	content, err := promptEntry.Contents()
	if err != nil {
		return ""
	}

	return ExtractFirstPrompt(content)
}

// SessionRestoreStatus represents the status of a session being restored.
type SessionRestoreStatus int

const (
	StatusNew             SessionRestoreStatus = iota // Local file doesn't exist
	StatusUnchanged                                   // Local and checkpoint are the same
	StatusCheckpointNewer                             // Checkpoint has newer entries
	StatusLocalNewer                                  // Local has newer entries (conflict)
)

// SessionRestoreInfo contains information about a session being restored.
type SessionRestoreInfo struct {
	SessionID      string
	Prompt         string               // First prompt preview for display
	Status         SessionRestoreStatus // Status of this session
	LocalTime      time.Time
	CheckpointTime time.Time
}

// classifySessionsForRestore checks all sessions in a checkpoint and returns info
// about each session, including whether local logs have newer timestamps.
// repoRoot is used to compute per-session agent directories.
// Sessions without agent metadata are skipped (cannot determine target directory).
func (s *ManualCommitStrategy) classifySessionsForRestore(ctx context.Context, repoRoot string, store cpkg.SessionReader, checkpointID id.CheckpointID, summary *cpkg.CheckpointSummary) []SessionRestoreInfo {
	var sessions []SessionRestoreInfo

	totalSessions := len(summary.Sessions)
	// Check all sessions (0-based indexing)
	for i := range totalSessions {
		content, err := store.ReadSessionContent(ctx, checkpointID, i)
		if err != nil || content == nil || len(content.Transcript) == 0 {
			continue
		}

		sessionID := content.Metadata.SessionID
		if sessionID == "" || content.Metadata.Agent == "" {
			continue
		}
		// Skip unsafe session IDs (see RestoreLogsOnly): this path stats the resolved
		// transcript file, so a crafted ID could otherwise probe arbitrary locations.
		if validation.ValidateSessionID(sessionID) != nil {
			continue
		}

		sessionAgent, agErr := ResolveAgentForResume(content.Metadata.Agent)
		if agErr != nil {
			continue
		}

		// Compute transcript path from current repo location for cross-machine portability.
		sessionAgentDir, dirErr := sessionAgent.GetSessionDir(repoRoot)
		if dirErr != nil {
			continue
		}
		localPath := sessionAgent.ResolveSessionFile(sessionAgentDir, sessionID)

		localTime := paths.GetLastTimestampFromFile(localPath)
		checkpointTime := paths.GetLastTimestampFromBytes(content.Transcript)
		status := ClassifyTimestamps(localTime, checkpointTime)
		// ClassifyTimestamps reports StatusNew when the local file has no parseable
		// timestamp — but a present-but-untimestamped log still exists and must not
		// be silently overwritten. Only a truly-absent file counts as new.
		if status == StatusNew {
			if _, statErr := os.Stat(localPath); statErr == nil {
				status = StatusUnchanged
			}
		}

		sessions = append(sessions, SessionRestoreInfo{
			SessionID:      sessionID,
			Prompt:         ExtractFirstPrompt(content.Prompts),
			Status:         status,
			LocalTime:      localTime,
			CheckpointTime: checkpointTime,
		})
	}

	return sessions
}

// ClassifyTimestamps determines the restore status based on local and checkpoint timestamps.
func ClassifyTimestamps(localTime, checkpointTime time.Time) SessionRestoreStatus {
	// Local file doesn't exist (no timestamp found)
	if localTime.IsZero() {
		return StatusNew
	}

	// Can't determine checkpoint time - treat as new/safe
	if checkpointTime.IsZero() {
		return StatusNew
	}

	// Compare timestamps
	if localTime.After(checkpointTime) {
		return StatusLocalNewer
	}
	if checkpointTime.After(localTime) {
		return StatusCheckpointNewer
	}
	return StatusUnchanged
}
