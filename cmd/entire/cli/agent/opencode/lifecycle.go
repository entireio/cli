package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

var runOpenCodeExportToFileFn = runOpenCodeExportToFile

// Compile-time assertion that OpenCode can inject context into the model.
var _ agent.ContextInjector = (*OpenCodeAgent)(nil)

// InjectionEvent reports that OpenCode injects model context at TurnStart. The
// embedded plugin reads the turn-start hook's stdout and applies the injection
// via experimental.chat.system.transform.
func (a *OpenCodeAgent) InjectionEvent() agent.EventType { return agent.TurnStart }

// RenderContextInjection emits a {"inject_context":"..."} envelope on stdout for
// the plugin to apply. Returns (nil, nil) for empty text.
func (a *OpenCodeAgent) RenderContextInjection(inj agent.ContextInjection) ([]byte, error) {
	if strings.TrimSpace(inj.Text) == "" {
		return nil, nil
	}
	b, err := json.Marshal(struct {
		InjectContext string `json:"inject_context"`
	}{InjectContext: inj.Text})
	if err != nil {
		return nil, fmt.Errorf("marshal opencode context injection: %w", err)
	}
	return append(b, '\n'), nil
}

// Hook name constants — these become CLI subcommands under `entire hooks opencode`.
const (
	HookNameSessionStart  = "session-start"
	HookNameSessionEnd    = "session-end"
	HookNameTurnStart     = "turn-start"
	HookNameTurnEnd       = "turn-end"
	HookNameCompaction    = "compaction"
	HookNameSubagentStart = "subagent-start"
	HookNameSubagentStop  = "subagent-stop"
)

// HookNames returns the hook verbs this agent supports.
func (a *OpenCodeAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameTurnStart,
		HookNameTurnEnd,
		HookNameCompaction,
		HookNameSubagentStart,
		HookNameSubagentStop,
	}
}

// ParseHookEvent translates OpenCode hook calls into normalized lifecycle events.
func (a *OpenCodeAgent) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:      agent.SessionStart,
			SessionID: raw.SessionID,
			Timestamp: time.Now(),
		}, nil

	case HookNameTurnStart:
		raw, err := agent.ReadAndParseHookInput[turnStartRaw](stdin)
		if err != nil {
			return nil, err
		}
		transcriptPath, err := sessionTranscriptPath(ctx, raw.SessionID)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.TurnStart,
			SessionID:  raw.SessionID,
			SessionRef: transcriptPath,
			Prompt:     raw.Prompt,
			Model:      raw.Model,
			Timestamp:  time.Now(),
		}, nil

	case HookNameTurnEnd:
		raw, err := agent.ReadAndParseHookInput[turnEndRaw](stdin)
		if err != nil {
			return nil, err
		}
		// Export is deferred to PrepareTranscript; we just compute the path here.
		transcriptPath, err := sessionTranscriptPath(ctx, raw.SessionID)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.TurnEnd,
			SessionID:  raw.SessionID,
			SessionRef: transcriptPath,
			Model:      raw.Model,
			Timestamp:  time.Now(),
		}, nil

	case HookNameCompaction:
		raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:      agent.Compaction,
			SessionID: raw.SessionID,
			Timestamp: time.Now(),
		}, nil

	case HookNameSessionEnd:
		raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:      agent.SessionEnd,
			SessionID: raw.SessionID,
			Timestamp: time.Now(),
		}, nil

	case HookNameSubagentStart:
		raw, parentRef, err := a.parseSubagentPayload(ctx, stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:               agent.SubagentStart,
			SessionID:          raw.SessionID,
			SessionRef:         parentRef,
			ToolUseID:          raw.ToolUseID,
			SubagentID:         raw.SubagentID,
			SubagentType:       raw.SubagentType,
			TaskDescription:    raw.TaskDescription,
			DeferredCompletion: true,
			Timestamp:          time.Now(),
		}, nil

	case HookNameSubagentStop:
		raw, parentRef, err := a.parseSubagentPayload(ctx, stdin)
		if err != nil {
			return nil, err
		}
		event := &agent.Event{
			Type:            agent.SubagentEnd,
			SessionID:       raw.SessionID,
			SessionRef:      parentRef,
			ToolUseID:       raw.ToolUseID,
			SubagentID:      raw.SubagentID,
			SubagentType:    raw.SubagentType,
			TaskDescription: raw.TaskDescription,
			Model:           raw.Model,
			Timestamp:       time.Now(),
			// Final: tool.execute.after is the one true-completion signal.
			// CompletionWithoutLaunch: a plugin restarted mid-task never saw the start.
			Final:                   true,
			CompletionWithoutLaunch: true,
		}
		a.attachSubagentTranscript(ctx, event)
		return event, nil

	default:
		return nil, nil //nolint:nilnil // nil event = no lifecycle action for unknown hooks
	}
}

// validateSubagentIdentity checks the three IDs an OpenCode subagent payload
// must carry. The child ID becomes an `opencode export` argument and a file
// name under .entire/tmp; the tool-use ID becomes tasks/<id>/ in the
// checkpoint. ValidateToolUseID accepts the empty string, so the explicit
// emptiness check is load-bearing.
func validateSubagentIdentity(parentID, toolUseID, childID string) error {
	if err := validation.ValidateSessionID(parentID); err != nil {
		return fmt.Errorf("invalid parent session ID: %w", err)
	}
	if toolUseID == "" {
		return errors.New("opencode subagent payload missing tool_use_id")
	}
	if err := validation.ValidateToolUseID(toolUseID); err != nil {
		return fmt.Errorf("invalid tool_use_id: %w", err)
	}
	if err := validation.ValidateSessionID(childID); err != nil {
		return fmt.Errorf("invalid subagent session ID: %w", err)
	}
	return nil
}

// parseSubagentPayload reads a subagent-start or subagent-stop payload,
// validates its identity fields, and resolves the parent's transcript path.
func (a *OpenCodeAgent) parseSubagentPayload(ctx context.Context, stdin io.Reader) (*subagentRaw, string, error) {
	raw, err := agent.ReadAndParseHookInput[subagentRaw](stdin)
	if err != nil {
		return nil, "", err
	}
	if err := validateSubagentIdentity(raw.SessionID, raw.ToolUseID, raw.SubagentID); err != nil {
		return nil, "", err
	}
	parentRef, err := sessionTranscriptPath(ctx, raw.SessionID)
	if err != nil {
		return nil, "", err
	}
	return raw, parentRef, nil
}

// attachSubagentTranscript exports the child session and declares it on the
// event with its exact token usage. On failure the event is left marked
// transcript-unavailable: OpenCode does implement TranscriptFetcher, but
// neither the SessionEnd sweep nor condensation calls it — condensation reads
// only DeclaredTranscriptPath candidates — so a failed export here permanently
// loses a transcript that still exists in OpenCode's store. Degrading to a
// completed, transcript-unavailable record is still better than an error that
// leaves the marker live until SessionEnd completes it transcriptless anyway.
// Follow-up: a lazy FetchTranscript at condensation for a declared-but-missing
// OpenCode path.
func (a *OpenCodeAgent) attachSubagentTranscript(ctx context.Context, event *agent.Event) {
	logCtx := logging.WithComponent(ctx, "lifecycle")
	path, err := a.fetchAndCacheExport(ctx, event.SubagentID)
	if err != nil {
		logging.Warn(logCtx, "opencode: could not export subagent transcript; completing task without it",
			slog.String("session_id", event.SessionID),
			slog.String("tool_use_id", event.ToolUseID),
			slog.String("subagent_id", event.SubagentID),
			slog.String("error", err.Error()))
		event.SubagentTranscriptUnavailable = true
		return
	}
	event.SubagentTranscriptPath = path
	data, err := a.ReadTranscript(path)
	if err != nil {
		logging.Warn(logCtx, "opencode: could not read exported subagent transcript for token usage",
			slog.String("subagent_id", event.SubagentID), slog.String("error", err.Error()))
		return
	}
	usage, err := a.CalculateTokenUsage(data, 0)
	if err != nil {
		logging.Warn(logCtx, "opencode: could not compute subagent token usage",
			slog.String("subagent_id", event.SubagentID), slog.String("error", err.Error()))
		return
	}
	event.TokenUsage = usage
}

// PrepareTranscript ensures the OpenCode transcript file is up-to-date by calling `opencode export`.
// OpenCode's transcript is created/updated via `opencode export`, but condensation may need fresh
// data mid-turn (e.g., during mid-turn commits or resumed sessions where the cached file is stale).
// This method always refreshes the transcript to ensure the latest agent activity is captured.
func (a *OpenCodeAgent) PrepareTranscript(ctx context.Context, sessionRef string) error {
	// Validate the session ref path
	if _, err := agent.StatTranscriptFile(sessionRef); err != nil && !os.IsNotExist(err) {
		// Permission denied, broken symlink, or other non-recoverable errors
		return fmt.Errorf("failed to stat OpenCode transcript path %s: %w", sessionRef, err)
	}

	// Extract session ID from path: basename without .json extension
	base := filepath.Base(sessionRef)
	if !strings.HasSuffix(base, ".json") {
		return fmt.Errorf("invalid OpenCode transcript path (expected .json): %s", sessionRef)
	}
	sessionID := strings.TrimSuffix(base, ".json")
	if sessionID == "" {
		return fmt.Errorf("empty session ID in transcript path: %s", sessionRef)
	}

	// Always call fetchAndCacheExport to get fresh transcript data.
	// This is critical for resumed sessions where the cached file may contain stale data
	// from a previous turn. Unlike turn-end (which always runs export), mid-turn commits
	// need to refresh the transcript to capture agent activity since the last export.
	_, err := a.fetchAndCacheExport(ctx, sessionID)
	return err
}

// FetchTranscript materializes the session's transcript via `opencode export`
// and returns the cached path. Unlike PrepareTranscript (which only refreshes
// an existing file), this works for sessions Entire never tracked — e.g.
// sessions spawned by an external host, where no hook ever cached an export.
func (a *OpenCodeAgent) FetchTranscript(ctx context.Context, sessionID string) (string, error) {
	return a.fetchAndCacheExport(ctx, sessionID)
}

// sessionTranscriptPath validates the session ID and returns the expected transcript path.
func sessionTranscriptPath(ctx context.Context, sessionID string) (string, error) {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return "", fmt.Errorf("invalid session ID for transcript path: %w", err)
	}
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}
	// Absolute on purpose: this value is handed to callers that pass it to other
	// processes and to agent-facing APIs. Reads and writes still go through the
	// shared root (see fetchAndCacheExport).
	return filepath.Join(repoRoot, paths.EntireTmpDir, sessionID+".json"), nil
}

// entireTmpName is .entire/tmp relative to the .entire root.
var entireTmpName = entiredir.MustName(paths.EntireTmpDir)

// fetchAndCacheExport calls `opencode export <sessionID>` and writes the result
// to a temporary file. Returns the path to the temp file.
//
// Integration testing: Set ENTIRE_TEST_OPENCODE_MOCK_EXPORT=1 to skip the
// `opencode export` call and use pre-written mock data instead. Tests must
// pre-write the transcript file to .entire/tmp/<sessionID>.json before
// triggering the hook. See integration_test/hooks.go:SimulateOpenCodeTurnEnd.
func (a *OpenCodeAgent) fetchAndCacheExport(ctx context.Context, sessionID string) (string, error) {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return "", fmt.Errorf("invalid session ID for export: %w", err)
	}

	// Get worktree root for the temp directory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}

	// Every read, write, and rename below goes through the shared .entire root.
	// The absolute paths are still needed alongside it, because `opencode export`
	// is a separate process that takes a path, and the transcript path is handed
	// back to callers that pass it across the same boundary.
	root, err := entiredir.OpenAt(repoRoot)
	if err != nil {
		return "", fmt.Errorf("open %s for export cache: %w", paths.EntireDir, err)
	}
	entireDirAbs := filepath.Join(repoRoot, paths.EntireDir)
	tmpName := entireTmpName + "/" + sessionID + ".json"
	tmpFile := filepath.Join(entireDirAbs, filepath.FromSlash(tmpName))

	// Integration test mode: use pre-written mock file without calling opencode export
	if os.Getenv("ENTIRE_TEST_OPENCODE_MOCK_EXPORT") != "" {
		if _, err := root.Stat(tmpName); err == nil {
			return tmpFile, nil
		}
		return "", fmt.Errorf("mock export file not found: %s (ENTIRE_TEST_OPENCODE_MOCK_EXPORT is set)", tmpFile)
	}

	// Write export directly to temp file under .entire. Avoid stdout capture,
	// which can truncate large payloads in some opencode versions.
	if err := osroot.MkdirAllNoSymlink(root, entireTmpName, 0o750); err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	// Export to a staging file and move it into place only once the bytes are
	// known good. tmpFile is frequently the ONLY local copy of the session — the
	// turn-end hook writes it and nothing condenses it into a checkpoint until
	// the user commits — and every caller re-exports over a possibly-populated
	// path (PrepareTranscript on every turn end, FetchTranscript on attach).
	// Writing in place means a missing binary, a rejected session, a timeout, or
	// an `opencode export` that exits 0 with truncated output replaces a good
	// transcript with nothing or with garbage. Garbage is the worse of the two:
	// attach's os.Stat branch accepts whatever is at this path and treats
	// PrepareTranscript's failure as best-effort, so a corrupt file is used
	// silently while a missing one at least falls through to a re-fetch.
	staged, err := stageExportPath(root, entireTmpName, sessionID)
	if err != nil {
		return "", err
	}
	stagedAbs := filepath.Join(entireDirAbs, filepath.FromSlash(staged))
	keepStaged := false
	defer func() {
		if !keepStaged {
			_ = root.Remove(staged) //nolint:errcheck // best-effort cleanup of a staging file we are abandoning
		}
	}()

	if err := runOpenCodeExportToFileFn(ctx, root, sessionID, staged); err != nil {
		return "", err
	}

	data, err := entiredir.ReadFile(root, staged)
	if err != nil {
		return "", fmt.Errorf("failed to read export file: %w", err)
	}

	if !json.Valid(data) {
		logging.Debug(logging.WithComponent(ctx, "lifecycle"),
			"opencode export file contained invalid JSON",
			slog.Int("bytes", len(data)),
			slog.String("path", stagedAbs),
		)
		return "", &openCodeExportError{
			message: fmt.Sprintf("OpenCode returned invalid transcript data for session %q. Try updating OpenCode and running the command again.", sessionID),
		}
	}

	if err := renameOverExisting(root, staged, tmpName); err != nil {
		// The staged export is intact and validated; keep it rather than delete a
		// transcript we may be the last holder of, and name it so the user can
		// recover it by hand.
		keepStaged = true
		return "", fmt.Errorf("failed to install export file (export saved at %s): %w", stagedAbs, err)
	}
	keepStaged = true

	return tmpFile, nil
}
