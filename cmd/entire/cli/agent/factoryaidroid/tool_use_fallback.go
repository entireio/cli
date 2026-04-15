package factoryaidroid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const fallbackToolUseStatePrefix = "factory-task-tool-use-"

type fallbackToolUseState struct {
	Entries []fallbackToolUseEntry `json:"entries"`
}

type fallbackToolUseEntry struct {
	Fingerprint string `json:"fingerprint"`
	ToolUseID   string `json:"tool_use_id"`
}

func registerFallbackToolUseID(
	ctx context.Context,
	sessionID, toolName string,
	toolInput json.RawMessage,
) (string, error) {
	statePath, err := fallbackToolUseStatePath(ctx, sessionID)
	if err != nil {
		return "", err
	}

	state, err := loadFallbackToolUseState(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			state = &fallbackToolUseState{}
		} else {
			return "", err
		}
	}

	toolUseID, err := newFallbackToolUseID()
	if err != nil {
		return "", err
	}

	state.Entries = append(state.Entries, fallbackToolUseEntry{
		Fingerprint: fallbackToolFingerprint(toolName, toolInput),
		ToolUseID:   toolUseID,
	})
	if err := saveFallbackToolUseState(statePath, state); err != nil {
		return "", err
	}

	return toolUseID, nil
}

func resolveFallbackToolUseID(
	ctx context.Context,
	sessionID, toolName string,
	toolInput json.RawMessage,
) (string, error) {
	statePath, err := fallbackToolUseStatePath(ctx, sessionID)
	if err != nil {
		return "", err
	}

	state, err := loadFallbackToolUseState(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fallbackToolUseID(sessionID, toolName, toolInput), nil
		}
		return "", err
	}

	fingerprint := fallbackToolFingerprint(toolName, toolInput)
	for i := len(state.Entries) - 1; i >= 0; i-- {
		if state.Entries[i].Fingerprint != fingerprint {
			continue
		}

		toolUseID := state.Entries[i].ToolUseID
		state.Entries = append(state.Entries[:i], state.Entries[i+1:]...)
		if err := saveFallbackToolUseState(statePath, state); err != nil {
			return "", err
		}
		return toolUseID, nil
	}

	return fallbackToolUseID(sessionID, toolName, toolInput), nil
}

func newFallbackToolUseID() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate fallback tool_use_id: %w", err)
	}
	return "factorytask_" + hex.EncodeToString(suffix[:]), nil
}

func fallbackToolUseStatePath(ctx context.Context, sessionID string) (string, error) {
	tmpDir, err := paths.AbsPath(ctx, paths.EntireTmpDir)
	if err != nil {
		return "", fmt.Errorf("resolve fallback tool_use_id tmp dir: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return "", fmt.Errorf("create fallback tool_use_id tmp dir: %w", err)
	}

	sessionHash := fallbackToolUseID(sessionID, "", nil)
	return filepath.Join(tmpDir, fallbackToolUseStatePrefix+sessionHash+".json"), nil
}

func loadFallbackToolUseState(path string) (*fallbackToolUseState, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read fallback tool_use_id state: %w", err)
	}

	var state fallbackToolUseState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal fallback tool_use_id state: %w", err)
	}
	return &state, nil
}

func saveFallbackToolUseState(path string, state *fallbackToolUseState) error {
	if len(state.Entries) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove empty fallback tool_use_id state: %w", err)
		}
		return nil
	}

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal fallback tool_use_id state: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write fallback tool_use_id state: %w", err)
	}
	return nil
}
