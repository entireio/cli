package goose

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// Compile-time assertions.
var (
	_ agent.HookSupport        = (*GooseAgent)(nil)
	_ agent.TranscriptPreparer = (*GooseAgent)(nil)
)

// Hook verbs — these become subcommands under `entire hooks goose`.
const (
	HookNameSessionStart = "session-start"
	HookNameSessionEnd   = "session-end"
	HookNameTurnStart    = "turn-start"
	HookNameTurnEnd      = "turn-end"
)

// gooseHookEvents maps each Entire hook verb to the Goose event that fires it.
// This is the single place the mapping is stated; hooks.go renders the config
// from it so the installed file and the parser can never disagree.
var gooseHookEvents = map[string]string{
	HookNameSessionStart: "SessionStart",
	HookNameTurnStart:    "UserPromptSubmit",
	HookNameTurnEnd:      "Stop",
	HookNameSessionEnd:   "SessionEnd",
}

// HookNames returns the hook verbs this agent supports.
func (a *GooseAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameTurnStart,
		HookNameTurnEnd,
	}
}

// ParseHookEvent translates a Goose hook invocation into a lifecycle event.
//
// The verb identifies the event, not the payload's "event" field: the subcommand
// already encodes which hook fired, so nothing breaks if Goose renames or drops
// that field. Only session_id is read from stdin.
func (a *GooseAgent) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		raw, err := agent.ReadAndParseHookInput[hookPayload](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:      agent.SessionStart,
			SessionID: raw.SessionID,
			Timestamp: time.Now(),
		}, nil

	case HookNameTurnStart:
		raw, err := agent.ReadAndParseHookInput[hookPayload](stdin)
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
			Timestamp:  time.Now(),
		}, nil

	case HookNameTurnEnd:
		raw, err := agent.ReadAndParseHookInput[hookPayload](stdin)
		if err != nil {
			return nil, err
		}
		// The export itself is deferred to PrepareTranscript; only the path is
		// computed here so a slow export cannot stall the hook.
		transcriptPath, err := sessionTranscriptPath(ctx, raw.SessionID)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.TurnEnd,
			SessionID:  raw.SessionID,
			SessionRef: transcriptPath,
			Timestamp:  time.Now(),
		}, nil

	case HookNameSessionEnd:
		raw, err := agent.ReadAndParseHookInput[hookPayload](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:      agent.SessionEnd,
			SessionID: raw.SessionID,
			Timestamp: time.Now(),
		}, nil

	default:
		return nil, nil //nolint:nilnil // nil event = no lifecycle action for unknown hooks
	}
}

// PrepareTranscript refreshes the cached export before a read.
//
// Goose's conversation lives in SQLite, so unlike a file-backed agent there is
// no transcript on disk until we ask for one. This runs on every read rather
// than only on a cache miss: mid-turn commits and resumed sessions both need
// data newer than whatever the last turn-end wrote.
func (a *GooseAgent) PrepareTranscript(ctx context.Context, sessionRef string) error {
	if _, err := os.Stat(sessionRef); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to stat goose transcript path %s: %w", sessionRef, err)
	}

	base := filepath.Base(sessionRef)
	if !strings.HasSuffix(base, ".json") {
		return fmt.Errorf("invalid goose transcript path (expected .json): %s", sessionRef)
	}
	sessionID := strings.TrimSuffix(base, ".json")
	if sessionID == "" {
		return fmt.Errorf("empty session ID in transcript path: %s", sessionRef)
	}

	return a.fetchAndCacheExport(ctx, sessionID)
}

// sessionTranscriptPath validates the session ID and returns its cache path.
func sessionTranscriptPath(ctx context.Context, sessionID string) (string, error) {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return "", fmt.Errorf("invalid session ID for transcript path: %w", err)
	}
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}
	return filepath.Join(repoRoot, paths.EntireTmpDir, sessionID+".json"), nil
}

// fetchAndCacheExport runs `goose session export` into .entire/tmp.
//
// The destination is deterministic (sessionTranscriptPath computes the same
// path), so nothing is returned but the error.
//
// Integration testing: set ENTIRE_TEST_GOOSE_MOCK_EXPORT=1 to skip the export
// and use a pre-written file at .entire/tmp/<sessionID>.json instead.
func (a *GooseAgent) fetchAndCacheExport(ctx context.Context, sessionID string) error {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID for export: %w", err)
	}

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}

	tmpDir := filepath.Join(repoRoot, paths.EntireTmpDir)
	tmpFile := filepath.Join(tmpDir, sessionID+".json")

	if os.Getenv("ENTIRE_TEST_GOOSE_MOCK_EXPORT") != "" {
		if _, statErr := os.Stat(tmpFile); statErr == nil {
			return nil
		}
		return fmt.Errorf("mock export file not found: %s (ENTIRE_TEST_GOOSE_MOCK_EXPORT is set)", tmpFile)
	}

	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}

	if err := runGooseExportToFileFn(ctx, sessionID, tmpFile); err != nil {
		return fmt.Errorf("goose session export failed: %w", err)
	}

	//nolint:gosec // tmpFile is built from a validated session ID under .entire/tmp
	data, err := os.ReadFile(tmpFile)
	if err != nil {
		return fmt.Errorf("failed to read export file: %w", err)
	}

	if !json.Valid(data) {
		logging.Debug(logging.WithComponent(ctx, "lifecycle"),
			"goose export file contained invalid JSON",
			slog.Int("bytes", len(data)),
			slog.String("path", tmpFile),
		)
		return fmt.Errorf("goose session export returned invalid JSON (%d bytes)", len(data))
	}

	return nil
}
