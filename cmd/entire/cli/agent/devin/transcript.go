package devin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// ReadTranscript reads the raw ATIF transcript bytes for a session.
func (d *DevinAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return data, nil
}

// PrepareTranscript materializes or waits for Devin's transcript. Devin
// writes the canonical transcript only when a session run ends — so at
// TurnEnd time the file is either absent (first run, mid-session) or stale
// (resumed session). The framework requires the transcript file to exist
// before it saves a checkpoint, so:
//   - fresh file: return immediately
//   - stale file: keep it — real data from the previous run beats a stub;
//     no flush is coming mid-session
//   - missing file: poll briefly (print mode writes it ~60ms after Stop
//     fires), then materialize a minimal valid ATIF stub so the turn's code
//     checkpoint can proceed. Devin overwrites the file with the complete
//     transcript when the session ends, and the eager condensation at
//     SessionEnd captures that full version (see AGENT.md, flush timing).
func (d *DevinAgent) PrepareTranscript(_ context.Context, sessionRef string) error {
	if sessionRef == "" {
		return nil
	}
	const (
		maxWait      = 2 * time.Second
		pollInterval = 50 * time.Millisecond
		maxSkew      = 2 * time.Second
		// staleThreshold matches the claude-code fast path: a transcript that
		// hasn't been touched in minutes belongs to a previous session run and
		// no flush is coming.
		staleThreshold = 2 * time.Minute
	)

	hookStart := time.Now()
	if info, err := os.Stat(sessionRef); err == nil {
		age := time.Since(info.ModTime())
		if age < maxSkew {
			return nil // Already fresh
		}
		if age > staleThreshold {
			return nil // Previous run's transcript; no flush coming mid-session
		}
	}

	deadline := hookStart.Add(maxWait)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(sessionRef); err == nil && info.ModTime().After(hookStart.Add(-maxSkew)) {
			return nil
		}
		time.Sleep(pollInterval)
	}

	if _, err := os.Stat(sessionRef); os.IsNotExist(err) {
		return writeStubTranscript(sessionRef)
	}
	return nil // Stale file left in place: best-effort
}

// writeStubTranscript writes a minimal valid ATIF document for a session
// whose canonical transcript has not been flushed yet. Never overwrites an
// existing file. Devin regenerates transcripts from its session store on
// exit, so the stub's lifetime ends with the session run.
func writeStubTranscript(sessionRef string) error {
	sessionID := strings.TrimSuffix(filepath.Base(sessionRef), ".json")
	stub := ATIFTranscript{
		SchemaVersion: "ATIF-v1.7",
		SessionID:     sessionID,
		Steps:         []ATIFStep{},
	}
	data, err := json.Marshal(stub)
	if err != nil {
		return fmt.Errorf("failed to marshal stub transcript: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(sessionRef), 0o750); err != nil {
		return fmt.Errorf("failed to create transcript directory: %w", err)
	}
	f, err := os.OpenFile(sessionRef, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // Derived transcript path
	if err != nil {
		if os.IsExist(err) {
			return nil // Real transcript landed in the meantime — keep it
		}
		return fmt.Errorf("failed to create stub transcript: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("failed to write stub transcript: %w", err)
	}
	return nil
}

// parseTranscript unmarshals an ATIF document.
func parseTranscript(data []byte) (*ATIFTranscript, error) {
	var t ATIFTranscript
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("failed to parse ATIF transcript: %w", err)
	}
	return &t, nil
}

// GetTranscriptPosition returns the current step count of a Devin transcript.
// Devin uses a JSON document with a steps array, so position is the number of
// steps (message-count pattern, like Gemini/OpenCode).
// Returns 0 if the file doesn't exist or is empty.
func (d *DevinAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // Path comes from Devin transcript location
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read transcript file: %w", err)
	}
	if len(data) == 0 {
		return 0, nil
	}
	t, err := parseTranscript(data)
	if err != nil {
		return 0, err
	}
	return len(t.Steps), nil
}

// ExtractModifiedFilesFromOffset extracts files modified since a given step index.
func (d *DevinAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}
	data, readErr := os.ReadFile(path) //nolint:gosec // Path comes from Devin transcript location
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("failed to read transcript file: %w", readErr)
	}
	t, parseErr := parseTranscript(data)
	if parseErr != nil {
		return nil, 0, parseErr
	}
	return extractModifiedFilesFromSteps(t.Steps, startOffset), len(t.Steps), nil
}

// ExtractModifiedFiles extracts all modified file paths from a raw transcript.
func ExtractModifiedFiles(data []byte) ([]string, error) {
	t, err := parseTranscript(data)
	if err != nil {
		return nil, err
	}
	return extractModifiedFilesFromSteps(t.Steps, 0), nil
}

// extractModifiedFilesFromSteps collects file paths from write/edit tool
// calls on agent steps, starting at the given step index, deduplicated in
// first-seen order.
func extractModifiedFilesFromSteps(steps []ATIFStep, startOffset int) []string {
	if startOffset < 0 {
		startOffset = 0
	}
	seen := make(map[string]struct{})
	var files []string
	for i := startOffset; i < len(steps); i++ {
		var info atifStepInfo
		if err := json.Unmarshal(steps[i], &info); err != nil {
			continue // Skip malformed steps
		}
		for _, call := range info.ToolCalls {
			if !isFileModificationTool(call.FunctionName) {
				continue
			}
			var input fileToolInput
			if err := json.Unmarshal(call.Arguments, &input); err != nil || input.FilePath == "" {
				continue
			}
			if _, ok := seen[input.FilePath]; !ok {
				seen[input.FilePath] = struct{}{}
				files = append(files, input.FilePath)
			}
		}
	}
	return files
}

// CalculateTokenUsage computes token usage from the transcript starting at
// the given step offset. Devin's per-step metrics report prompt_tokens
// inclusive of cache reads, so fresh input is prompt_tokens - cached_tokens.
func (d *DevinAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	t, err := parseTranscript(transcriptData)
	if err != nil {
		return nil, err
	}
	if fromOffset < 0 {
		fromOffset = 0
	}
	usage := &agent.TokenUsage{}
	for i := fromOffset; i < len(t.Steps); i++ {
		var info atifStepInfo
		if err := json.Unmarshal(t.Steps[i], &info); err != nil || info.Metrics == nil {
			continue
		}
		fresh := info.Metrics.PromptTokens - info.Metrics.CachedTokens
		if fresh < 0 {
			fresh = 0
		}
		usage.InputTokens += fresh
		usage.CacheReadTokens += info.Metrics.CachedTokens
		usage.OutputTokens += info.Metrics.CompletionTokens
		usage.APICallCount++
	}
	return usage, nil
}

// ExtractModel returns the model identifier recorded in the transcript.
// Devin records it on the agent block and on each agent step; hooks carry no
// model field, so the transcript is the only source.
func (d *DevinAgent) ExtractModel(transcriptData []byte) (string, error) {
	t, err := parseTranscript(transcriptData)
	if err != nil {
		return "", err
	}
	// Prefer the most recent agent step's model (sessions can switch models).
	for i := len(t.Steps) - 1; i >= 0; i-- {
		var info atifStepInfo
		if err := json.Unmarshal(t.Steps[i], &info); err != nil {
			continue
		}
		if info.Source == "agent" && info.ModelName != "" {
			return info.ModelName, nil
		}
	}
	if len(t.Agent) > 0 {
		var info atifAgentInfo
		if err := json.Unmarshal(t.Agent, &info); err == nil && info.ModelName != "" {
			return info.ModelName, nil
		}
	}
	return "", nil
}

// ChunkTranscript splits an ATIF transcript by distributing steps across
// chunks, preserving the envelope (schema_version, session_id, agent,
// final_metrics) in each chunk so every chunk is independently parseable.
func (d *DevinAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	t, err := parseTranscript(content)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript for chunking: %w", err)
	}
	if len(t.Steps) == 0 || len(content) <= maxSize {
		return [][]byte{content}, nil
	}

	envelope := ATIFTranscript{
		SchemaVersion: t.SchemaVersion,
		SessionID:     t.SessionID,
		Agent:         t.Agent,
		FinalMetrics:  t.FinalMetrics,
	}
	envelopeBytes, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal envelope for chunking: %w", err)
	}
	baseSize := len(envelopeBytes) + len(`,"steps":[]`)

	var chunks [][]byte
	var current []ATIFStep
	currentSize := baseSize

	flush := func() error {
		if len(current) == 0 {
			return nil
		}
		chunk := envelope
		chunk.Steps = current
		data, err := json.Marshal(chunk)
		if err != nil {
			return fmt.Errorf("failed to marshal chunk: %w", err)
		}
		chunks = append(chunks, data)
		current = nil
		currentSize = baseSize
		return nil
	}

	for _, step := range t.Steps {
		stepSize := len(step) + 1 // +1 for comma separator
		if currentSize+stepSize > maxSize && len(current) > 0 {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		current = append(current, step)
		currentSize += stepSize
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, errors.New("failed to create any chunks")
	}
	return chunks, nil
}

// ReassembleTranscript merges ATIF chunks by concatenating their step arrays.
// The envelope is taken from the first chunk.
func (d *DevinAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	if len(chunks) == 0 {
		return nil, errors.New("no chunks to reassemble")
	}
	var result *ATIFTranscript
	for i, chunk := range chunks {
		t, err := parseTranscript(chunk)
		if err != nil {
			return nil, fmt.Errorf("failed to parse chunk %d: %w", i, err)
		}
		if i == 0 {
			result = t
			continue
		}
		result.Steps = append(result.Steps, t.Steps...)
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal reassembled transcript: %w", err)
	}
	return data, nil
}
