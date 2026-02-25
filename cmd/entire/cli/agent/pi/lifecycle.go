package pi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Compile-time interface assertions for new lifecycle interfaces.
var (
	_ agent.TranscriptAnalyzer = (*PiAgent)(nil)
	_ agent.TokenCalculator    = (*PiAgent)(nil)
)

const (
	turnEndTranscriptWaitTimeout  = 5 * time.Second
	turnEndTranscriptPollInterval = 50 * time.Millisecond
)

// HookNames returns the hook verbs Pi supports.
func (p *PiAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameStop,
		HookNameUserPromptSubmit,
		HookNameBeforeCompact,
		HookNameBeforeTool,
		HookNameAfterTool,
	}
}

// ParseHookEvent translates a Pi hook into a normalized lifecycle Event.
// Returns nil if the hook has no lifecycle significance.
func (p *PiAgent) ParseHookEvent(hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		return p.parseSessionStart(stdin)
	case HookNameUserPromptSubmit:
		return p.parseTurnStart(stdin)
	case HookNameStop:
		return p.parseTurnEnd(stdin)
	case HookNameSessionEnd:
		return p.parseSessionEnd(stdin)
	case HookNameBeforeCompact:
		return p.parseCompaction(stdin)
	case HookNameBeforeTool, HookNameAfterTool:
		// Tool lifecycle events are acknowledged but don't trigger checkpoint lifecycle actions.
		return nil, nil //nolint:nilnil // nil event = no lifecycle action
	default:
		return nil, nil //nolint:nilnil // Unknown hooks have no lifecycle action
	}
}

// ReadTranscript reads the raw transcript bytes for a session.
func (p *PiAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		// Graceful degradation: if Pi is shutting down and transcript is unavailable,
		// continue hook lifecycle with empty transcript instead of failing hard.
		if os.IsNotExist(err) {
			return []byte{}, nil
		}
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return data, nil
}

// ExtractPrompts extracts user prompts from transcript starting at the given line offset.
func (p *PiAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	return p.ExtractPromptsWithLeaf(sessionRef, fromOffset, "")
}

// ExtractPromptsWithLeaf extracts prompts from a specific active leaf in tree-based transcripts.
func (p *PiAgent) ExtractPromptsWithLeaf(sessionRef string, fromOffset int, leafID string) ([]string, error) {
	entries, _, err := ParseTranscriptFromLineWithLeaf(sessionRef, fromOffset, strings.TrimSpace(leafID))
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}
	return ExtractAllUserPromptsFromEntries(entries), nil
}

// ExtractSummary extracts the last assistant message as a session summary.
func (p *PiAgent) ExtractSummary(sessionRef string) (string, error) {
	return p.ExtractSummaryWithLeaf(sessionRef, "")
}

// ExtractSummaryWithLeaf extracts summary text from a specific active leaf in tree-based transcripts.
func (p *PiAgent) ExtractSummaryWithLeaf(sessionRef string, leafID string) (string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return "", fmt.Errorf("failed to read transcript: %w", err)
	}

	entries, parseErr := ParseTranscriptWithLeaf(data, strings.TrimSpace(leafID))
	if parseErr != nil {
		return "", fmt.Errorf("failed to parse transcript: %w", parseErr)
	}

	return ExtractLastAssistantMessageFromEntries(entries), nil
}

// CalculateTokenUsage computes token usage from transcript starting at the given line offset.
func (p *PiAgent) CalculateTokenUsage(sessionRef string, fromOffset int) (*agent.TokenUsage, error) {
	return p.CalculateTokenUsageWithLeaf(sessionRef, fromOffset, "")
}

// CalculateTokenUsageWithLeaf computes token usage scoped to the active leaf (for tree-based transcripts).
func (p *PiAgent) CalculateTokenUsageWithLeaf(sessionRef string, fromOffset int, leafID string) (*agent.TokenUsage, error) {
	return CalculateTokenUsageFromFileWithLeaf(sessionRef, fromOffset, strings.TrimSpace(leafID))
}

// ExtractModifiedFilesFromOffsetWithLeaf extracts modified files from a transcript offset,
// resolving active branch content from the provided leaf ID.
func (p *PiAgent) ExtractModifiedFilesFromOffsetWithLeaf(path string, startOffset int, leafID string) (files []string, currentPosition int, err error) {
	return ExtractModifiedFilesSinceOffsetWithLeaf(path, startOffset, strings.TrimSpace(leafID))
}

func (p *PiAgent) parseSessionStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := readAndParse[piHookInput](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.SessionStart,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  time.Now(),
	}, nil
}

func (p *PiAgent) parseTurnStart(stdin io.Reader) (*agent.Event, error) {
	raw, err := readAndParse[piHookInput](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.TurnStart,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Prompt:     raw.Prompt,
		Timestamp:  time.Now(),
	}, nil
}

func (p *PiAgent) parseTurnEnd(stdin io.Reader) (*agent.Event, error) {
	raw, err := readAndParse[piHookInput](stdin)
	if err != nil {
		return nil, err
	}

	transcriptRef, ensureErr := ensureTurnEndTranscriptPath(raw.SessionID, raw.TranscriptPath)
	if ensureErr != nil {
		return nil, ensureErr
	}
	if strings.TrimSpace(raw.TranscriptPath) != "" {
		// Pi may emit stop before transcript bytes are fully flushed.
		// Wait briefly for non-empty content; continue even on timeout.
		_ = waitForNonEmptyFile(transcriptRef, turnEndTranscriptWaitTimeout, turnEndTranscriptPollInterval)
	}

	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  raw.SessionID,
		SessionRef: transcriptRef,
		Timestamp:  time.Now(),
	}
	if leafID := strings.TrimSpace(raw.LeafID); leafID != "" {
		event.Metadata = map[string]string{"leaf_id": leafID}
	}
	return event, nil
}

func (p *PiAgent) parseSessionEnd(stdin io.Reader) (*agent.Event, error) {
	raw, err := readAndParse[piHookInput](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.SessionEnd,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  time.Now(),
	}, nil
}

func (p *PiAgent) parseCompaction(stdin io.Reader) (*agent.Event, error) {
	raw, err := readAndParse[piHookInput](stdin)
	if err != nil {
		return nil, err
	}
	return &agent.Event{
		Type:       agent.Compaction,
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
		Timestamp:  time.Now(),
	}, nil
}

func ensureTurnEndTranscriptPath(sessionID, transcriptPath string) (string, error) {
	return ensureTurnEndTranscriptPathInRoot(sessionID, transcriptPath, "")
}

func ensureTurnEndTranscriptPathInRoot(sessionID, transcriptPath, repoRoot string) (string, error) {
	root := strings.TrimSpace(repoRoot)
	if root == "" {
		root = "."
		if detectedRoot, err := paths.WorktreeRoot(); err == nil && strings.TrimSpace(detectedRoot) != "" {
			root = detectedRoot
		}
	}

	resolvedPath := strings.TrimSpace(transcriptPath)
	if resolvedPath == "" {
		resolvedPath = filepath.Join(root, ".entire", "tmp", sanitizeSessionIDForFileName(sessionID)+".jsonl")
	} else if !filepath.IsAbs(resolvedPath) {
		resolvedPath = filepath.Join(root, resolvedPath)
	}

	if err := os.MkdirAll(filepath.Dir(resolvedPath), 0o750); err != nil {
		return "", fmt.Errorf("failed to prepare transcript directory: %w", err)
	}

	if _, err := os.Stat(resolvedPath); err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("failed to stat transcript file: %w", err)
		}
		file, createErr := os.OpenFile(resolvedPath, os.O_CREATE|os.O_WRONLY, 0o600)
		if createErr != nil {
			return "", fmt.Errorf("failed to create transcript file: %w", createErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return "", fmt.Errorf("failed to close transcript file: %w", closeErr)
		}
	}

	return resolvedPath, nil
}

func sanitizeSessionIDForFileName(sessionID string) string {
	trimmed := strings.TrimSpace(sessionID)
	if trimmed == "" {
		return "unknown-session"
	}
	replacer := strings.NewReplacer("/", "-", "\\", "-", ":", "-")
	return replacer.Replace(trimmed)
}

func waitForNonEmptyFile(path string, timeout, pollInterval time.Duration) bool {
	resolvedPath := strings.TrimSpace(path)
	if resolvedPath == "" {
		return false
	}
	if timeout <= 0 {
		timeout = turnEndTranscriptWaitTimeout
	}
	if pollInterval <= 0 {
		pollInterval = turnEndTranscriptPollInterval
	}

	deadline := time.Now().Add(timeout)
	for {
		info, err := os.Stat(resolvedPath)
		if err == nil && info.Size() > 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pollInterval)
	}
}

// readAndParse reads stdin and unmarshals JSON into the given type.
func readAndParse[T any](stdin io.Reader) (*T, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("failed to read hook input: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("empty hook input")
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}
	return &result, nil
}
