package factoryaidroid

import (
	"context"
	"encoding/json"
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
// the settings.json beside the transcript. Droid writes it debounced and not at
// all for exec or BYOK sessions, and its shape is known only from the binary,
// not from a captured session; when it is absent the transcript's own usage is
// summed from line 0 (deduped by message ID), which covers versions that write
// usage there. Children are the Worker sessions it launched, keyed by Worker
// session ID and read the same way.
func (f *FactoryAIDroidAgent) CumulativeTokenUsage(_ context.Context, in agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage, error) {
	if in.TranscriptPath == "" {
		return nil, nil, nil
	}
	total, err := sessionTokenUsage(in.TranscriptPath, in.TranscriptData)
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
		if usage, err := sessionTokenUsage(workerPath, nil); err == nil && usage != nil {
			if children == nil {
				children = make(map[string]*agent.TokenUsage)
			}
			children[workerID] = usage
		}
	}
	return total, children, nil
}

// sessionTokenUsage returns a session's usage from its settings.json, else
// from its transcript (data, or the file when data is nil). nil means no data.
func sessionTokenUsage(transcriptPath string, data []byte) (*agent.TokenUsage, error) {
	usage, err := readSettingsTokenUsage(transcriptPath)
	if err != nil || usage != nil {
		return usage, err
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
	if usage = CalculateTokenUsage(lines); usage.APICallCount == 0 {
		return nil, nil //nolint:nilnil // a transcript without usage carries no data
	}
	return usage, nil
}

// readSettingsTokenUsage returns the tokenUsage from the settings.json beside
// transcriptPath, or nil when the file or the field is absent. thinkingTokens
// is ignored: Droid estimates it from streamed text, and providers already
// count thinking in outputTokens.
func readSettingsTokenUsage(transcriptPath string) (*agent.TokenUsage, error) {
	data, err := os.ReadFile(strings.TrimSuffix(transcriptPath, ".jsonl") + ".settings.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // no settings file means no usage data
	}
	if err != nil {
		return nil, fmt.Errorf("read droid settings: %w", err)
	}
	var settings struct {
		TokenUsage *struct {
			InputTokens         int `json:"inputTokens"`
			OutputTokens        int `json:"outputTokens"`
			CacheCreationTokens int `json:"cacheCreationTokens"`
			CacheReadTokens     int `json:"cacheReadTokens"`
		} `json:"tokenUsage"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("parse droid settings: %w", err)
	}
	u := settings.TokenUsage
	if u == nil {
		return nil, nil //nolint:nilnil // settings without tokenUsage carry no usage data
	}
	// The file has no call count, so APICallCount stays 0.
	return &agent.TokenUsage{
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		CacheCreationTokens: u.CacheCreationTokens,
		CacheReadTokens:     u.CacheReadTokens,
	}, nil
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
			if !ok || entry.IsDir() || workerID == parentSessionID || validation.ValidateAgentSessionID(workerID) != nil {
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
