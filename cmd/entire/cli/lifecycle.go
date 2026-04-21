// lifecycle.go implements the generic lifecycle event dispatcher.
// It routes normalized events from any agent to the appropriate framework actions.
//
// The dispatcher inverts the current flow from "agent handler calls framework functions"
// to "framework dispatcher calls agent methods." Agents are passive data providers;
// the dispatcher handles all orchestration: state transitions, strategy calls,
// file change detection, metadata generation.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/perf"
)

// DispatchLifecycleEvent routes a normalized lifecycle event to the appropriate handler.
// Returns nil if the event was handled successfully.
func DispatchLifecycleEvent(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	if ag == nil {
		return errors.New("agent cannot be nil")
	}
	if event == nil {
		return errors.New("event cannot be nil")
	}

	switch event.Type {
	case agent.SessionStart:
		return handleLifecycleSessionStart(ctx, ag, event)
	case agent.TurnStart:
		return handleLifecycleTurnStart(ctx, ag, event)
	case agent.TurnEnd:
		return handleLifecycleTurnEnd(ctx, ag, event)
	case agent.Compaction:
		return handleLifecycleCompaction(ctx, ag, event)
	case agent.SessionEnd:
		return handleLifecycleSessionEnd(ctx, ag, event)
	case agent.SubagentStart:
		return handleLifecycleSubagentStart(ctx, ag, event)
	case agent.SubagentEnd:
		return handleLifecycleSubagentEnd(ctx, ag, event)
	case agent.ModelUpdate:
		return handleLifecycleModelUpdate(ctx, ag, event)
	default:
		return fmt.Errorf("unknown lifecycle event type: %d", event.Type)
	}
}

// handleLifecycleSessionStart handles session start: shows banner, checks concurrent sessions,
// fires state machine transition.
func handleLifecycleSessionStart(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "session-start",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("session_ref", event.SessionRef),
		slog.String("model", event.Model),
	)

	if event.SessionID == "" {
		return fmt.Errorf("no session_id in %s event", event.Type)
	}
	if err := validation.ValidateSessionID(event.SessionID); err != nil {
		return fmt.Errorf("invalid %s event: %w", event.Type, err)
	}

	// Build informational message — warn early if repo has no commits yet,
	// since checkpoints require at least one commit to work.
	message := sessionStartMessage(ag.Name(), false)
	if repo, err := strategy.OpenRepository(ctx); err == nil && strategy.IsEmptyRepository(repo) {
		message = sessionStartMessage(ag.Name(), true)
	}

	// Check for concurrent sessions and append count if any
	_, countSessionsSpan := perf.Start(ctx, "count_active_sessions")
	strat := GetStrategy(ctx)
	if count, err := strat.CountOtherActiveSessionsWithCheckpoints(ctx, event.SessionID); err == nil && count > 0 {
		if ag.Name() == agent.AgentNameCodex {
			message += fmt.Sprintf(" %d other active conversation(s) in this workspace will also be included. Use 'entire status' for more information.", count)
		} else {
			message += fmt.Sprintf("\n  %d other active conversation(s) in this workspace will also be included.\n  Use 'entire status' for more information.", count)
		}
	}
	countSessionsSpan.End()

	// Output informational message if the agent supports hook responses.
	// Claude Code reads JSON from stdout; agents that don't implement
	// HookResponseWriter silently skip (avoids raw JSON in their terminal).
	_, hookResponseSpan := perf.Start(ctx, "write_hook_response")
	if event.ResponseMessage != "" {
		message = event.ResponseMessage
	}
	if writer, ok := agent.AsHookResponseWriter(ag); ok {
		if err := writer.WriteHookResponse(message); err != nil {
			hookResponseSpan.RecordError(err)
			hookResponseSpan.End()
			return fmt.Errorf("failed to write hook response: %w", err)
		}
	}
	hookResponseSpan.End()

	// Store model hint if the agent provided model info on SessionStart
	if event.Model != "" {
		if err := strategy.StoreModelHint(ctx, event.SessionID, event.Model); err != nil {
			logging.Warn(logCtx, "failed to store model hint on session start",
				slog.String("error", err.Error()))
		}
	}

	// Fire EventSessionStart for the current session (if state exists).
	if state, loadErr := strategy.LoadSessionState(ctx, event.SessionID); loadErr != nil {
		logging.Warn(logCtx, "failed to load session state on start",
			slog.String("error", loadErr.Error()))
	} else if state != nil {
		persistEventMetadataToState(event, state)
		if transErr := strategy.TransitionAndLog(ctx, state, session.EventSessionStart, session.TransitionContext{}, session.NoOpActionHandler{}); transErr != nil {
			logging.Warn(logCtx, "session start transition failed",
				slog.String("error", transErr.Error()))
		}
		if saveErr := strategy.SaveSessionState(ctx, state); saveErr != nil {
			logging.Warn(logCtx, "failed to update session state on start",
				slog.String("error", saveErr.Error()))
		}
	}

	return nil
}

func sessionStartMessage(agentName types.AgentName, emptyRepo bool) string {
	if agentName == agent.AgentNameCodex {
		if emptyRepo {
			return "Entire CLI found no commits yet — checkpoints will activate after your first commit."
		}
		return "Entire CLI will link this conversation to your next commit."
	}

	if emptyRepo {
		return "\n\nEntire CLI found no commits yet — checkpoints will activate after your first commit."
	}
	return "\n\nEntire CLI will link this conversation to your next commit."
}

// handleLifecycleModelUpdate persists the model name for the current session.
//
// If the session state file already exists (e.g., Gemini's BeforeModel fires
// after TurnStart), the model is written directly to state.ModelName — no hint
// file needed. Otherwise falls back to StoreModelHint for cross-process
// persistence (see its doc comment for the full rationale).
func handleLifecycleModelUpdate(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "model-update",
		slog.String("session_id", event.SessionID),
		slog.String("model", event.Model),
	)

	if event.SessionID == "" || event.Model == "" {
		return nil
	}

	// Prefer writing directly to session state when it exists
	state, loadErr := strategy.LoadSessionState(ctx, event.SessionID)
	if loadErr != nil {
		logging.Debug(logCtx, "could not load session state for model update, using hint file",
			slog.String("error", loadErr.Error()))
	}
	if loadErr == nil && state != nil {
		state.ModelName = event.Model
		if saveErr := strategy.SaveSessionState(ctx, state); saveErr != nil {
			logging.Warn(logCtx, "failed to update session state with model",
				slog.String("error", saveErr.Error()))
		}
		return nil
	}

	// State doesn't exist yet (or failed to load) — use hint file (see StoreModelHint doc)
	if err := strategy.StoreModelHint(ctx, event.SessionID, event.Model); err != nil {
		logging.Warn(logCtx, "failed to store model hint",
			slog.String("error", err.Error()))
	}

	return nil
}

// handleLifecycleTurnStart handles turn start: captures pre-prompt state,
// ensures strategy setup, initializes session.
func handleLifecycleTurnStart(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "turn-start",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("session_ref", event.SessionRef),
		slog.String("model", event.Model),
	)

	sessionID := event.SessionID
	if sessionID == "" {
		return fmt.Errorf("no session_id in %s event", event.Type)
	}
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid %s event: %w", event.Type, err)
	}

	// Fill model from hint file if the agent didn't provide it on this hook
	if event.Model == "" {
		if hint := strategy.LoadModelHint(ctx, sessionID); hint != "" {
			event.Model = hint
			logging.Debug(logCtx, "loaded model from hint file",
				slog.String("model", hint))
		}
	}

	// Capture pre-prompt state (including transcript position via TranscriptAnalyzer)
	_, captureSpan := perf.Start(ctx, "capture_pre_prompt_state")
	if err := CapturePrePromptState(ctx, ag, sessionID, event.SessionRef); err != nil {
		captureSpan.RecordError(err)
		captureSpan.End()
		return err
	}
	captureSpan.End()

	// Append prompt to prompt.txt on filesystem so it's available for
	// mid-turn commits (before SaveStep writes it to the shadow branch).
	// Prompts are separated by "\n\n---\n\n" to support multiple turns.
	if event.Prompt != "" {
		sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
		if sessionDirAbs, absErr := paths.AbsPath(ctx, sessionDir); absErr == nil {
			if mkErr := os.MkdirAll(sessionDirAbs, 0o750); mkErr == nil {
				promptPath := filepath.Join(sessionDirAbs, paths.PromptFileName)
				existing, readErr := os.ReadFile(promptPath) //nolint:gosec // session metadata path
				var content string
				if readErr == nil && len(existing) > 0 {
					content = string(existing) + "\n\n---\n\n" + event.Prompt
				} else {
					content = event.Prompt
				}
				if writeErr := os.WriteFile(promptPath, []byte(content), 0o600); writeErr != nil { //nolint:gosec // path from internal metadata, not user input
					logging.Warn(logCtx, "failed to write prompt.txt",
						slog.String("error", writeErr.Error()))
				}
			}
		}
	}

	// Ensure strategy setup and initialize session
	_, initSpan := perf.Start(ctx, "init_session")
	if err := strategy.EnsureSetup(ctx); err != nil {
		logging.Warn(logCtx, "failed to ensure strategy setup",
			slog.String("error", err.Error()))
	}

	strat := GetStrategy(ctx)
	if err := strat.InitializeSession(ctx, sessionID, ag.Type(), event.SessionRef, event.Prompt, event.Model); err != nil {
		logging.Warn(logCtx, "failed to initialize session state",
			slog.String("error", err.Error()))
	}

	// Best-effort: adopt any pending review marker written by `entire review`
	// before spawning the agent. Errors in load/save must not fail the turn.
	if stateStore, storeErr := session.NewStateStore(ctx); storeErr != nil {
		logging.Warn(logCtx, "failed to create state store for review marker adoption",
			slog.String("error", storeErr.Error()))
	} else if state, loadErr := stateStore.Load(ctx, sessionID); loadErr != nil {
		logging.Warn(logCtx, "failed to load session state for review marker adoption",
			slog.String("error", loadErr.Error()))
	} else if state != nil {
		if updated, modified, adoptErr := adoptPendingReviewMarkerInto(logCtx, *state); adoptErr != nil {
			logging.Warn(logCtx, "failed to adopt pending review marker",
				slog.String("error", adoptErr.Error()))
		} else if modified {
			if saveErr := stateStore.Save(ctx, &updated); saveErr != nil {
				logging.Warn(logCtx, "failed to save session state after review marker adoption",
					slog.String("error", saveErr.Error()))
			}
		}
	}
	initSpan.End()

	return nil
}

// handleLifecycleTurnEnd handles turn end: validates transcript, extracts metadata,
// detects file changes, saves step + checkpoint, transitions phase.
//
//nolint:maintidx // high complexity due to sequential orchestration of 8 steps (validation, extraction, file detection, filtering, token calc, step save, phase transition, cleanup) - splitting would obscure the flow
func handleLifecycleTurnEnd(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "turn-end",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("session_ref", event.SessionRef),
		slog.String("model", event.Model),
	)

	sessionID := event.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	// Fill model from hint file if the agent didn't provide it on this hook
	if event.Model == "" && sessionID != unknownSessionID {
		if hint := strategy.LoadModelHint(ctx, sessionID); hint != "" {
			event.Model = hint
			logging.Debug(logCtx, "loaded model from hint file",
				slog.String("model", hint))
		}
	}

	transcriptRef := event.SessionRef
	if transcriptRef == "" {
		return errors.New("transcript file not specified")
	}

	// If agent implements TranscriptPreparer, materialize the transcript file.
	// This must run BEFORE fileExists: agents like OpenCode lazily fetch transcripts
	// via `opencode export`, so the file doesn't exist until PrepareTranscript creates it.
	// Claude Code's PrepareTranscript just flushes (always succeeds). Agents without
	// TranscriptPreparer (Gemini, Droid) are unaffected.
	_, prepareSpan := perf.Start(ctx, "prepare_and_validate_transcript")
	if preparer, ok := agent.AsTranscriptPreparer(ag); ok {
		if err := preparer.PrepareTranscript(ctx, transcriptRef); err != nil {
			logging.Warn(logCtx, "failed to prepare transcript",
				slog.String("error", err.Error()))
		}
	}

	if !fileExists(transcriptRef) {
		prepareSpan.RecordError(fmt.Errorf("transcript file not found: %s", transcriptRef))
		prepareSpan.End()
		return fmt.Errorf("transcript file not found: %s", transcriptRef)
	}

	// Early check: bail out quickly if the repo has no commits yet.
	// Return nil (not an error) so the hook exits 0 — agents treat non-zero
	// exit codes as hook failures. The user was already warned at session start.
	if repo, err := strategy.OpenRepository(ctx); err == nil && strategy.IsEmptyRepository(repo) {
		prepareSpan.End()
		logging.Info(logCtx, "skipping checkpoint - will activate after first commit")
		return nil
	}
	prepareSpan.End()

	// Create session metadata directory
	_, copySpan := perf.Start(ctx, "copy_transcript")
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(ctx, sessionDir)
	if err != nil {
		sessionDirAbs = sessionDir
	}
	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		copySpan.RecordError(err)
		copySpan.End()
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	// Copy transcript to session directory
	transcriptData, err := ag.ReadTranscript(transcriptRef)
	if err != nil {
		copySpan.RecordError(err)
		copySpan.End()
		return fmt.Errorf("failed to read transcript: %w", err)
	}
	logFile := filepath.Join(sessionDirAbs, paths.TranscriptFileName)
	if err := os.WriteFile(logFile, transcriptData, 0o600); err != nil {
		copySpan.RecordError(err)
		copySpan.End()
		return fmt.Errorf("failed to write transcript: %w", err)
	}
	logging.Debug(logCtx, "copied transcript",
		slog.String("path", sessionDir+"/"+paths.TranscriptFileName))
	copySpan.End()

	// Load pre-prompt state (captured on TurnStart)
	_, extractSpan := perf.Start(ctx, "extract_metadata")
	preState, err := LoadPrePromptState(ctx, sessionID)
	if err != nil {
		logging.Warn(logCtx, "failed to load pre-prompt state",
			slog.String("error", err.Error()))
	}

	// Determine transcript offset
	transcriptOffset := resolveTranscriptOffset(ctx, preState, sessionID)

	// Backfill prompt.txt from transcript when prompt data is missing.
	// This handles agents whose exec mode doesn't fire UserPromptSubmit (e.g., Factory AI
	// Droid). The transcript is the source of truth — if ExtractPrompts returns nothing,
	// there genuinely were no prompts. We track whether backfill occurred so we can
	// update session state after SaveStep (which may reinitialize state).
	var backfilledPrompt string
	promptPath := filepath.Join(sessionDirAbs, paths.PromptFileName)
	existingPrompt, readPromptErr := os.ReadFile(promptPath) //nolint:gosec // file content is safe session metadata
	if readPromptErr != nil && !os.IsNotExist(readPromptErr) {
		logging.Warn(logCtx, "failed to read prompt.txt, skipping backfill",
			slog.String("error", readPromptErr.Error()))
	} else if len(existingPrompt) == 0 {
		if extractor, ok := agent.AsPromptExtractor(ag); ok {
			prompts, extractErr := extractor.ExtractPrompts(transcriptRef, transcriptOffset)
			if extractErr != nil {
				logging.Warn(logCtx, "failed to extract prompts from transcript",
					slog.String("error", extractErr.Error()))
			} else if len(prompts) > 0 {
				content := strings.Join(prompts, "\n\n---\n\n")
				if writeErr := os.WriteFile(promptPath, []byte(content), 0o600); writeErr != nil {
					logging.Warn(logCtx, "failed to backfill prompt.txt from transcript",
						slog.String("error", writeErr.Error()))
				} else {
					logging.Debug(logCtx, "backfilled prompt.txt from transcript",
						slog.Int("prompt_count", len(prompts)))
					backfilledPrompt = prompts[len(prompts)-1]
				}
			}
		}
	}

	// Compute subagents directory for agents that support subagent extraction.
	// Subagent transcripts live in <transcriptDir>/<modelSessionID>/subagents/
	subagentsDir := filepath.Join(filepath.Dir(transcriptRef), event.SessionID, "subagents")

	// Extract metadata via agent interface (modified files)
	var modifiedFiles []string

	if analyzer, ok := agent.AsTranscriptAnalyzer(ag); ok {
		// Extract modified files - prefer SubagentAwareExtractor if available to include subagent files
		if subagentExtractor, subOk := agent.AsSubagentAwareExtractor(ag); subOk {
			if files, fileErr := subagentExtractor.ExtractAllModifiedFiles(transcriptData, transcriptOffset, subagentsDir); fileErr != nil {
				logging.Warn(logCtx, "failed to extract modified files (with subagents)",
					slog.String("error", fileErr.Error()))
			} else {
				modifiedFiles = files
			}
		} else {
			// Fall back to basic extraction (main transcript only)
			if files, _, fileErr := analyzer.ExtractModifiedFilesFromOffset(transcriptRef, transcriptOffset); fileErr != nil {
				logging.Warn(logCtx, "failed to extract modified files",
					slog.String("error", fileErr.Error()))
			} else {
				modifiedFiles = files
			}
		}
	}
	extractSpan.End()

	// Generate commit message from last prompt (read from session state, set at TurnStart).
	// In exec mode, session state LastPrompt may be empty because UserPromptSubmit never fires.
	// Fall back to backfilledPrompt extracted from the transcript.
	// Single load serves both prompt retrieval and backfill.
	_, commitMsgSpan := perf.Start(ctx, "generate_commit_message")
	lastPrompt := ""
	if sessionState, stateErr := strategy.LoadSessionState(ctx, sessionID); stateErr == nil && sessionState != nil {
		lastPrompt = sessionState.LastPrompt
		// Backfill LastPrompt so `entire status` shows the prompt even when
		// no files were modified (before the early return below).
		if lastPrompt == "" && backfilledPrompt != "" {
			lastPrompt = backfilledPrompt
			sessionState.LastPrompt = backfilledPrompt
			if saveErr := strategy.SaveSessionState(ctx, sessionState); saveErr != nil {
				logging.Warn(logCtx, "failed to backfill LastPrompt in session state",
					slog.String("error", saveErr.Error()))
			}
		}
	} else if backfilledPrompt != "" {
		lastPrompt = backfilledPrompt
	}
	commitMessage := generateCommitMessage(lastPrompt, ag.Type())
	logging.Debug(logCtx, "using commit message",
		slog.Int("message_length", len(commitMessage)))
	commitMsgSpan.End()

	// Get worktree root for path normalization
	_, detectSpan := perf.Start(ctx, "detect_file_changes")
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		detectSpan.RecordError(err)
		detectSpan.End()
		return fmt.Errorf("failed to get worktree root: %w", err)
	}

	var preUntrackedFiles []string
	if preState != nil {
		logging.Debug(logCtx, "pre-prompt state",
			slog.Int("pre_existing_untracked_files", len(preState.UntrackedFiles)))
		preUntrackedFiles = preState.PreUntrackedFiles()
	}

	// Detect file changes via git status
	changes, err := DetectFileChanges(ctx, preUntrackedFiles)
	if err != nil {
		logging.Warn(logCtx, "failed to compute file changes",
			slog.String("error", err.Error()))
	}
	detectSpan.End()

	// Filter and normalize all paths
	_, normalizeSpan := perf.Start(ctx, "filter_and_normalize_paths")
	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	var relNewFiles, relDeletedFiles []string
	if changes != nil {
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)

		// Merge git-status modified files as a fallback for transcript parsing.
		// Transcript parsing is the primary source for modified files, but it can miss
		// files if the agent uses an unrecognized tool or the transcript format changes.
		// Git status catches any tracked file with working-tree changes.
		relModifiedFiles = mergeUnique(relModifiedFiles, FilterAndNormalizePaths(changes.Modified, repoRoot))
	}

	// Filter transcript-extracted files to exclude files already committed to HEAD.
	// When an agent commits files mid-turn, those files are condensed by PostCommit
	// and should not be re-added to FilesTouched by SaveStep. A file is "committed"
	// if it exists in HEAD with the same content as the working tree.
	relModifiedFiles = filterToUncommittedFiles(ctx, relModifiedFiles, repoRoot)
	normalizeSpan.End()

	// Check if there are any changes
	totalChanges := len(relModifiedFiles) + len(relNewFiles) + len(relDeletedFiles)
	if totalChanges == 0 {
		logging.Info(logCtx, "no files modified during session, skipping checkpoint")
		transitionSessionTurnEnd(ctx, sessionID, event)
		if cleanupErr := CleanupPrePromptState(ctx, sessionID); cleanupErr != nil {
			logging.Warn(logCtx, "failed to cleanup pre-prompt state",
				slog.String("error", cleanupErr.Error()))
		}
		return nil
	}

	// Log file changes
	logFileChanges(ctx, relModifiedFiles, relNewFiles, relDeletedFiles)

	// Get git author
	author, err := GetGitAuthor(ctx)
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	// Get strategy and agent type
	strat := GetStrategy(ctx)
	agentType := ag.Type()

	// Get transcript position/identifier from pre-prompt state
	var transcriptIdentifierAtStart string
	var transcriptLinesAtStart int
	if preState != nil {
		transcriptIdentifierAtStart = preState.LastTranscriptIdentifier
		transcriptLinesAtStart = preState.TranscriptOffset
	}

	// Calculate token usage - prefer SubagentAwareExtractor to include subagent tokens
	tokenUsage := agent.CalculateTokenUsage(ctx, ag, transcriptData, transcriptLinesAtStart, subagentsDir)

	// Build fully-populated step context and delegate to strategy
	stepCtx := strategy.StepContext{
		SessionID:                sessionID,
		ModifiedFiles:            relModifiedFiles,
		NewFiles:                 relNewFiles,
		DeletedFiles:             relDeletedFiles,
		MetadataDir:              sessionDir,
		MetadataDirAbs:           sessionDirAbs,
		CommitMessage:            commitMessage,
		TranscriptPath:           transcriptRef,
		AuthorName:               author.Name,
		AuthorEmail:              author.Email,
		AgentType:                agentType,
		StepTranscriptIdentifier: transcriptIdentifierAtStart,
		StepTranscriptStart:      transcriptLinesAtStart,
		TokenUsage:               tokenUsage,
	}

	if err := strat.SaveStep(ctx, stepCtx); err != nil {
		return fmt.Errorf("failed to save step: %w", err)
	}

	// Update session state with backfilled prompt after SaveStep.
	// Done after SaveStep because SaveStep may reinitialize session state,
	// which would overwrite an earlier LastPrompt update.
	if backfilledPrompt != "" {
		if state, stateErr := strategy.LoadSessionState(ctx, sessionID); stateErr == nil && state != nil && state.LastPrompt == "" {
			state.LastPrompt = backfilledPrompt
			if saveErr := strategy.SaveSessionState(ctx, state); saveErr != nil {
				logging.Warn(logCtx, "failed to backfill LastPrompt in session state",
					slog.String("error", saveErr.Error()))
			}
		}
	}

	// Transition session phase and cleanup
	transitionSessionTurnEnd(ctx, sessionID, event)
	if cleanupErr := CleanupPrePromptState(ctx, sessionID); cleanupErr != nil {
		logging.Warn(logCtx, "failed to cleanup pre-prompt state",
			slog.String("error", cleanupErr.Error()))
	}

	return nil
}

// handleLifecycleCompaction handles context compaction: saves current progress
// but stays in ACTIVE phase (unlike TurnEnd which transitions to IDLE).
// Also resets the transcript offset since the transcript may be truncated.
func handleLifecycleCompaction(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "compaction",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
	)

	// Fire EventCompaction to trigger ActionCondenseIfFilesTouched (stays in ACTIVE)
	sessionID := event.SessionID
	sessionState, loadErr := strategy.LoadSessionState(ctx, sessionID)
	if loadErr != nil {
		logging.Warn(logCtx, "failed to load session state for compaction",
			slog.String("error", loadErr.Error()))
	}
	if sessionState != nil {
		persistEventMetadataToState(event, sessionState)

		if transErr := strategy.TransitionAndLog(ctx, sessionState, session.EventCompaction, session.TransitionContext{}, session.NoOpActionHandler{}); transErr != nil {
			logging.Warn(logCtx, "compaction transition failed",
				slog.String("error", transErr.Error()))
		}

		if saveErr := strategy.SaveSessionState(ctx, sessionState); saveErr != nil {
			logging.Warn(logCtx, "failed to save session state after compaction",
				slog.String("error", saveErr.Error()))
		}
	}

	logging.Info(logCtx, "context compaction detected")
	return nil
}

// handleLifecycleSessionEnd handles session end: marks the session as ended.
func handleLifecycleSessionEnd(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "session-end",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
	)

	if event.SessionID == "" {
		return nil // No session to update
	}

	// Note: We intentionally don't clean up cached transcripts here.
	// Post-session commits (carry-forward in ENDED phase) may still need
	// the transcript to extract file changes. Cleanup is handled by
	// `entire clean` or when the session state is fully removed.

	if err := markSessionEnded(ctx, event, event.SessionID); err != nil {
		logging.Warn(logCtx, "failed to mark session ended",
			slog.String("error", err.Error()))
		// Don't attempt eager condense if we couldn't even mark the session ended —
		// the session state may be in an inconsistent state.
		return nil
	}

	// Eagerly condense session data so PostCommit doesn't have to process it.
	// This prevents zombie ENDED sessions from accumulating and causing O(N)
	// overhead on every future commit (GitHub issue #591).
	// Fail-open: if this fails, PostCommit will still process it on the next commit.
	strat := GetStrategy(ctx)
	if err := strat.CondenseAndMarkFullyCondensed(ctx, event.SessionID); err != nil {
		logging.Warn(logCtx, "eager condense on session stop failed",
			slog.String("session_id", event.SessionID),
			slog.String("error", err.Error()))
	}

	return nil
}

// handleLifecycleSubagentStart handles subagent start: captures pre-task state.
func handleLifecycleSubagentStart(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "subagent started",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("tool_use_id", event.ToolUseID),
		slog.String("transcript", event.SessionRef),
	)

	// Capture pre-task state
	if err := CapturePreTaskState(ctx, event.ToolUseID); err != nil {
		return fmt.Errorf("failed to capture pre-task state: %w", err)
	}

	return nil
}

// handleLifecycleSubagentEnd handles subagent end: detects changes, saves task checkpoint.
func handleLifecycleSubagentEnd(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	if event.SubagentType == "" && event.TaskDescription == "" {
		// Extract subagent type and description from tool input
		event.SubagentType, event.TaskDescription = ParseSubagentTypeAndDescription(event.ToolInput)
	}

	// Determine subagent transcript path
	transcriptDir := filepath.Dir(event.SessionRef)
	var subagentTranscriptPath string
	if event.SubagentID != "" {
		subagentTranscriptPath = AgentTranscriptPath(transcriptDir, event.SubagentID)
		if !fileExists(subagentTranscriptPath) {
			subagentTranscriptPath = ""
		}
	}

	// Log context
	subagentEndAttrs := []any{
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("tool_use_id", event.ToolUseID),
	}
	if event.SubagentID != "" {
		subagentEndAttrs = append(subagentEndAttrs, slog.String("agent_id", event.SubagentID))
	}
	if subagentTranscriptPath != "" {
		subagentEndAttrs = append(subagentEndAttrs, slog.String("subagent_transcript", subagentTranscriptPath))
	}
	logging.Info(logCtx, "subagent completed", subagentEndAttrs...)

	// Extract modified files from hook payload and/or subagent transcript
	var modifiedFiles []string
	modifiedFiles = append(modifiedFiles, event.ModifiedFiles...)
	if analyzer, ok := agent.AsTranscriptAnalyzer(ag); ok {
		transcriptToScan := event.SessionRef
		if subagentTranscriptPath != "" {
			transcriptToScan = subagentTranscriptPath
		}
		if files, _, fileErr := analyzer.ExtractModifiedFilesFromOffset(transcriptToScan, 0); fileErr != nil {
			logging.Warn(logCtx, "failed to extract modified files from subagent",
				slog.String("error", fileErr.Error()))
		} else {
			modifiedFiles = mergeUnique(modifiedFiles, files)
		}
	}

	// Load pre-task state and detect file changes.
	// If no pre-task state exists (agent doesn't support pre-task hook), fall back
	// to the session's pre-prompt state. Without either, DetectFileChanges receives
	// nil and treats ALL untracked files as new — which would create spurious task
	// checkpoints for pre-existing untracked files (e.g., .github/hooks/entire.json).
	preState, err := LoadPreTaskState(ctx, event.ToolUseID)
	if err != nil {
		logging.Warn(logCtx, "failed to load pre-task state",
			slog.String("error", err.Error()))
	}
	var preUntrackedFiles []string
	if preState != nil {
		preUntrackedFiles = preState.PreUntrackedFiles()
	}
	changes, err := DetectFileChanges(ctx, preUntrackedFiles)
	if err != nil {
		logging.Warn(logCtx, "failed to compute file changes",
			slog.String("error", err.Error()))
	}

	// Get worktree root and normalize paths
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("failed to get worktree root: %w", err)
	}

	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	var relNewFiles, relDeletedFiles []string
	if changes != nil {
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)
		relModifiedFiles = mergeUnique(relModifiedFiles, FilterAndNormalizePaths(changes.Modified, repoRoot))
	}

	// If no changes, skip
	if len(relModifiedFiles) == 0 && len(relNewFiles) == 0 && len(relDeletedFiles) == 0 {
		logging.Info(logCtx, "no file changes detected, skipping task checkpoint")
		_ = CleanupPreTaskState(ctx, event.ToolUseID) //nolint:errcheck // best-effort cleanup
		return nil
	}

	// Find checkpoint UUID from main transcript (best-effort)
	var checkpointUUID string
	// Use the existing CLI-level checkpoint UUID finder
	mainLines, _ := parseTranscriptForCheckpointUUID(event.SessionRef) //nolint:errcheck // best-effort
	if mainLines != nil {
		checkpointUUID, _ = FindCheckpointUUID(mainLines, event.ToolUseID)
	}

	// Get git author
	author, err := GetGitAuthor(ctx)
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	// Build task checkpoint context
	strat := GetStrategy(ctx)
	agentType := ag.Type()

	taskStepCtx := strategy.TaskStepContext{
		SessionID:              event.SessionID,
		ToolUseID:              event.ToolUseID,
		AgentID:                event.SubagentID,
		ModifiedFiles:          relModifiedFiles,
		NewFiles:               relNewFiles,
		DeletedFiles:           relDeletedFiles,
		TranscriptPath:         event.SessionRef,
		SubagentTranscriptPath: subagentTranscriptPath,
		CheckpointUUID:         checkpointUUID,
		AuthorName:             author.Name,
		AuthorEmail:            author.Email,
		SubagentType:           event.SubagentType,
		TaskDescription:        event.TaskDescription,
		AgentType:              agentType,
	}

	if err := strat.SaveTaskStep(ctx, taskStepCtx); err != nil {
		return fmt.Errorf("failed to save task step: %w", err)
	}

	_ = CleanupPreTaskState(ctx, event.ToolUseID) //nolint:errcheck // best-effort cleanup
	return nil
}

// --- Helper functions ---

// resolveTranscriptOffset determines the transcript offset to use for parsing.
// Prefers pre-prompt state, falls back to session state.
func resolveTranscriptOffset(ctx context.Context, preState *PrePromptState, sessionID string) int {
	logCtx := logging.WithComponent(ctx, "lifecycle")
	if preState != nil && preState.TranscriptOffset > 0 {
		logging.Debug(logCtx, "pre-prompt state found, parsing transcript from offset",
			slog.Int("offset", preState.TranscriptOffset))
		return preState.TranscriptOffset
	}

	// Fall back to session state
	sessionState, loadErr := strategy.LoadSessionState(ctx, sessionID)
	if loadErr != nil {
		logging.Warn(logCtx, "failed to load session state",
			slog.String("error", loadErr.Error()))
		return 0
	}
	if sessionState != nil && sessionState.CheckpointTranscriptStart > 0 {
		logging.Debug(logCtx, "session state found, parsing transcript from offset",
			slog.Int("offset", sessionState.CheckpointTranscriptStart))
		return sessionState.CheckpointTranscriptStart
	}

	return 0
}

// parseTranscriptForCheckpointUUID is a thin wrapper around transcript parsing for checkpoint UUID lookup.
// Returns parsed transcript lines for use with FindCheckpointUUID.
func parseTranscriptForCheckpointUUID(transcriptPath string) ([]transcriptLine, error) {
	lines, err := transcript.ParseFromFileAtLine(transcriptPath, 0)
	if err != nil {
		return nil, fmt.Errorf("parsing transcript for checkpoint UUID: %w", err)
	}
	return lines, nil
}

// transitionSessionTurnEnd transitions the session phase to IDLE and dispatches turn-end actions.
func transitionSessionTurnEnd(ctx context.Context, sessionID string, event *agent.Event) {
	logCtx := logging.WithComponent(ctx, "lifecycle")
	turnState, loadErr := strategy.LoadSessionState(ctx, sessionID)
	if loadErr != nil {
		logging.Warn(logCtx, "failed to load session state for turn end",
			slog.String("error", loadErr.Error()))
		return
	}
	if turnState == nil {
		return
	}

	persistEventMetadataToState(event, turnState)

	if err := strategy.TransitionAndLog(ctx, turnState, session.EventTurnEnd, session.TransitionContext{}, session.NoOpActionHandler{}); err != nil {
		logging.Warn(logCtx, "turn-end transition failed",
			slog.String("error", err.Error()))
	}

	// Always dispatch to strategy for turn-end handling. The strategy reads
	// work items from state (e.g. TurnCheckpointIDs), not the action list.
	strat := GetStrategy(ctx)
	if err := strat.HandleTurnEnd(ctx, turnState); err != nil {
		logging.Warn(logCtx, "turn-end action dispatch failed",
			slog.String("error", err.Error()))
	}

	if updateErr := strategy.SaveSessionState(ctx, turnState); updateErr != nil {
		logging.Warn(logCtx, "failed to update session phase on turn end",
			slog.String("error", updateErr.Error()))
	}
}

// markSessionEnded transitions the session to ENDED phase via the state machine.
// If event is non-nil, hook-provided metrics are persisted to state before saving.
func markSessionEnded(ctx context.Context, event *agent.Event, sessionID string) error {
	state, err := strategy.LoadSessionState(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session state: %w", err)
	}
	if state == nil {
		return nil // No state file, nothing to update
	}

	if event != nil {
		persistEventMetadataToState(event, state)
	}

	if transErr := strategy.TransitionAndLog(ctx, state, session.EventSessionStop, session.TransitionContext{}, session.NoOpActionHandler{}); transErr != nil {
		logging.Warn(logging.WithComponent(ctx, "lifecycle"), "session stop transition failed",
			slog.String("error", transErr.Error()))
	}

	now := time.Now()
	state.EndedAt = &now

	if err := strategy.SaveSessionState(ctx, state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}
	return nil
}

// logFileChanges logs the files modified, created, and deleted during a session.
func logFileChanges(ctx context.Context, modified, newFiles, deleted []string) {
	logCtx := logging.WithComponent(ctx, "lifecycle")
	logging.Debug(logCtx, "files changed during session",
		slog.Int("modified", len(modified)),
		slog.Int("new", len(newFiles)),
		slog.Int("deleted", len(deleted)))
}

func persistEventMetadataToState(event *agent.Event, state *strategy.SessionState) {
	// Update ModelName if provided (model is known by turn-end even on first turn)
	if event.Model != "" {
		state.ModelName = event.Model
	}

	// Persist hook-provided session metrics (e.g., from Cursor hooks)
	if event.DurationMs > 0 {
		state.SessionDurationMs = event.DurationMs
	}
	// Use hook-reported turn count if available (take max); otherwise
	// increment on each TurnEnd event to count turns ourselves.
	if event.TurnCount > 0 {
		if event.TurnCount > state.SessionTurnCount {
			state.SessionTurnCount = event.TurnCount
		}
	} else if event.Type == agent.TurnEnd {
		state.SessionTurnCount++
	}
	if event.ContextTokens > 0 {
		state.ContextTokens = event.ContextTokens
	}
	if event.ContextWindowSize > 0 {
		state.ContextWindowSize = event.ContextWindowSize
	}
}
