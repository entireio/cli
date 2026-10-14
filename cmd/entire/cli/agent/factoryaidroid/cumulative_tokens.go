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

// CumulativeTokenUsage reads the session-cumulative tokenUsage Droid keeps in
// the settings.json beside the transcript (its transcripts carry no usage).
// Children are the Worker sessions it launched, keyed by Worker session ID.
//
// ASSUMPTION (unverified): thinkingTokens is disjoint from outputTokens, as
// Droid reports them as separate fields, so OutputTokens = outputTokens +
// thinkingTokens; captured runs reported no thinking. The file has no call
// count, so APICallCount is 0.
func (f *FactoryAIDroidAgent) CumulativeTokenUsage(_ context.Context, in agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage, error) {
	if in.TranscriptPath == "" {
		return nil, nil, nil
	}
	total, err := readSettingsTokenUsage(in.TranscriptPath)
	if err != nil {
		return nil, nil, err
	}
	sessionID := in.SessionID
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(in.TranscriptPath), ".jsonl")
	}
	// Workers live beside the parent or in the sessions root one level up.
	dir := filepath.Dir(in.TranscriptPath)
	var children map[string]*agent.TokenUsage
	for workerID, workerPath := range FindWorkerSessions([]string{dir, filepath.Dir(dir)}, sessionID) {
		if usage, err := readSettingsTokenUsage(workerPath); err == nil && usage != nil {
			if children == nil {
				children = make(map[string]*agent.TokenUsage)
			}
			children[workerID] = usage
		}
	}
	return total, children, nil
}

// readSettingsTokenUsage returns the tokenUsage from the settings.json beside
// transcriptPath, or nil when the file or the field is absent.
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
			ThinkingTokens      int `json:"thinkingTokens"`
		} `json:"tokenUsage"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("parse droid settings: %w", err)
	}
	u := settings.TokenUsage
	if u == nil {
		return nil, nil //nolint:nilnil // settings without tokenUsage carry no usage data
	}
	return &agent.TokenUsage{
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens + u.ThinkingTokens,
		CacheCreationTokens: u.CacheCreationTokens,
		CacheReadTokens:     u.CacheReadTokens,
	}, nil
}

// FindWorkerSessions returns the Worker sessions in dirs whose session_start
// line names parentSessionID as the calling session, mapped from Worker
// session ID to transcript path.
func FindWorkerSessions(dirs []string, parentSessionID string) map[string]string {
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
