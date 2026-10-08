package factoryaidroid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
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
	root, name, err := fallbackToolUseStateFile(ctx, sessionID)
	if err != nil {
		return "", err
	}

	state, err := loadFallbackToolUseState(root, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
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
	if err := saveFallbackToolUseState(root, name, state); err != nil {
		return "", err
	}

	return toolUseID, nil
}

func resolveFallbackToolUseID(
	ctx context.Context,
	sessionID, toolName string,
	toolInput json.RawMessage,
) (string, error) {
	root, name, err := fallbackToolUseStateFile(ctx, sessionID)
	if err != nil {
		return "", err
	}

	state, err := loadFallbackToolUseState(root, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
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
		if err := saveFallbackToolUseState(root, name, state); err != nil {
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

// fallbackToolUseStateFile returns the shared .entire root and the state file's
// name within it. The name is derived from a hash of the session ID rather than
// the ID itself, but it still goes through the root: every other .entire access
// does, and a name that never escapes is one less thing to re-argue here.
func fallbackToolUseStateFile(ctx context.Context, sessionID string) (*os.Root, string, error) {
	root, err := entiredir.Open(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("resolve fallback tool_use_id tmp dir: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, entireTmpName, 0o750); err != nil {
		return nil, "", fmt.Errorf("create fallback tool_use_id tmp dir: %w", err)
	}

	sessionHash := fallbackToolUseID(sessionID, "", nil)
	return root, entireTmpName + "/" + fallbackToolUseStatePrefix + sessionHash + ".json", nil
}

func loadFallbackToolUseState(root *os.Root, name string) (*fallbackToolUseState, error) {
	data, err := entiredir.ReadFile(root, name)
	if err != nil {
		return nil, fmt.Errorf("read fallback tool_use_id state: %w", err)
	}

	var state fallbackToolUseState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal fallback tool_use_id state: %w", err)
	}
	return &state, nil
}

func saveFallbackToolUseState(root *os.Root, name string, state *fallbackToolUseState) error {
	if len(state.Entries) == 0 {
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove empty fallback tool_use_id state: %w", err)
		}
		return nil
	}

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal fallback tool_use_id state: %w", err)
	}
	if err := entiredir.WriteFile(root, name, data, 0o600); err != nil {
		return fmt.Errorf("write fallback tool_use_id state: %w", err)
	}
	return nil
}

// entireTmpName is .entire/tmp relative to the .entire root.
var entireTmpName = entiredir.MustName(paths.EntireTmpDir)
