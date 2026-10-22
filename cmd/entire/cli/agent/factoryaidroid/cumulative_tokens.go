package factoryaidroid

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

var _ agent.CumulativeTokenSource = (*FactoryAIDroidAgent)(nil)

// CumulativeTokenUsage prefers the session-cumulative tokenUsage Droid keeps in
// the settings.json beside the transcript (verified on captured Droid 0.243.0
// sessions, BYOK included). Droid writes it debounced, so it can lag the
// transcript; when it is absent the transcript's own usage is summed from
// line 0 (deduped by message ID), which covers versions that write usage there.
// A session's tokenUsage counts only its own calls, so the total excludes its
// Workers. Children are the Worker sessions it launched, keyed by Worker
// session ID and read the same way.
func (f *FactoryAIDroidAgent) CumulativeTokenUsage(ctx context.Context, in agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage, error) {
	if in.TranscriptPath == "" {
		return nil, nil, nil
	}
	total, err := sessionTokenUsage(ctx, in.TranscriptPath, in.TranscriptData)
	if err != nil {
		return nil, nil, err
	}
	sessionID := in.SessionID
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(in.TranscriptPath), ".jsonl")
	}
	// Workers live beside the parent or in the sessions root one level up.
	// PERF: this opens every .jsonl there on each call; the ledger can instead
	// pass the Workers it linked through ResolveSubagentSession.
	dir := filepath.Dir(in.TranscriptPath)
	var children map[string]*agent.TokenUsage
	for workerID, workerPath := range findWorkerSessions([]string{dir, filepath.Dir(dir)}, sessionID) {
		if usage, err := sessionTokenUsage(ctx, workerPath, nil); err == nil && usage != nil {
			if children == nil {
				children = make(map[string]*agent.TokenUsage)
			}
			children[workerID] = usage
		}
	}
	return total, children, nil
}

// sessionTokenUsage returns a session's usage: the per-field max of its
// settings.json total and its transcript's usage (data, or the file when data
// is nil). Droid writes settings.json on a debounce, so it can lag the
// transcript; taking the max keeps the total from going down when it does.
// nil means no data in either.
func sessionTokenUsage(ctx context.Context, transcriptPath string, data []byte) (*agent.TokenUsage, error) {
	settings, err := readSettingsTokenUsage(ctx, transcriptPath)
	if err != nil {
		return nil, err
	}
	var lines []TranscriptLine
	if data != nil {
		lines, _, err = ParseDroidTranscriptFromBytes(data, 0)
	} else {
		lines, _, err = ParseDroidTranscript(transcriptPath, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("parse droid transcript: %w", err)
	}
	transcript := CalculateTokenUsage(lines)
	if transcript.APICallCount == 0 {
		transcript = nil
	}
	if settings != nil {
		// The settings file has no call count: one per assistant message.
		settings.APICallCount = countAssistantMessages(lines)
	}
	return agent.MaxTokenUsage(settings, transcript), nil
}

// readSettingsTokenUsage returns the tokenUsage from the settings.json beside
// transcriptPath (see droidTokenUsage for what it counts), or nil when the
// file or the field is absent.
func readSettingsTokenUsage(ctx context.Context, transcriptPath string) (*agent.TokenUsage, error) {
	settings, err := readSessionSettings(ctx, transcriptPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // no settings file means no usage data
	}
	if err != nil {
		return nil, err
	}
	u := settings.TokenUsage
	if u == nil {
		return nil, nil //nolint:nilnil // settings without tokenUsage carry no usage data
	}
	// The file has no call count, so APICallCount stays 0. An all-zero
	// placeholder is no data.
	return agent.UsageOrNil(&agent.TokenUsage{
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		CacheCreationTokens: u.CacheCreationTokens,
		CacheReadTokens:     u.CacheReadTokens,
	}), nil
}

// findWorkerSessions returns the Worker sessions in dirs whose session_start
// line names parentSessionID as the calling session, mapped from Worker
// session ID to transcript path.
func findWorkerSessions(dirs []string, parentSessionID string) map[string]string {
	workers := make(map[string]string)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			workerID, ok := strings.CutSuffix(entry.Name(), ".jsonl")
			// Regular files only: no directories, symlinks out of the session
			// store, or FIFOs that would block the open.
			if !ok || !entry.Type().IsRegular() || workerID == parentSessionID || validation.ValidateAgentSessionID(workerID) != nil {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if start, ok := readDroidSessionStart(path); ok && parentSessionID != "" && start.CallingSessionID == parentSessionID {
				workers[workerID] = path
			}
		}
	}
	return workers
}
