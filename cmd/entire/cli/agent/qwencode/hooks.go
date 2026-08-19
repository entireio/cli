package qwencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const (
	// configDirName is Qwen's project config directory.
	configDirName = ".qwen"

	// settingsFileName is the file Qwen reads hooks from.
	settingsFileName = "settings.json"

	// hooksKey is the settings key holding the hook table.
	hooksKey = "hooks"

	// hookTimeoutMillis bounds each hook command. Qwen's documented examples
	// express timeout in milliseconds, unlike Goose's seconds.
	hookTimeoutMillis = 30000
)

// settingsPath returns the absolute path to the project settings file.
func settingsPath(ctx context.Context) (string, error) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Fall back to CWD so tests can run outside a git repo.
		//nolint:forbidigo // explicit fallback when WorktreeRoot fails
		root, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve repo root: %w", err)
		}
	}
	return filepath.Join(root, configDirName, settingsFileName), nil
}

// wantedCommands returns the hook commands Entire installs today, keyed by Qwen
// event name.
func wantedCommands() map[string]string {
	out := make(map[string]string, len(qwenHookEvents))
	for verb, event := range qwenHookEvents {
		out[event] = agent.WrapProductionSilentHookCommand("entire hooks qwen-code " + verb)
	}
	return out
}

// loadSettings reads the settings file into a raw field map plus its hook table.
// Both are raw so that keys Entire does not model survive a rewrite: this file
// belongs to the user, not to Entire.
func loadSettings(path string) (raw, hooks map[string]json.RawMessage, err error) {
	raw = make(map[string]json.RawMessage)
	hooks = make(map[string]json.RawMessage)

	data, readErr := os.ReadFile(path) //nolint:gosec // path built from repo root
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return raw, hooks, nil
		}
		return nil, nil, fmt.Errorf("read qwen settings: %w", readErr)
	}
	if len(data) == 0 {
		return raw, hooks, nil
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("parse qwen settings: %w", err)
	}
	if existing, ok := raw[hooksKey]; ok {
		// A malformed hooks table is treated as empty rather than fatal; the
		// install then rewrites it.
		//nolint:errcheck // a malformed hooks table is treated as empty and rewritten
		_ = json.Unmarshal(existing, &hooks)
		if hooks == nil {
			hooks = make(map[string]json.RawMessage)
		}
	}
	return raw, hooks, nil
}

// InstallHooks writes Entire's hook entries into .qwen/settings.json, leaving
// every other setting and any user-authored hooks untouched.
//
// Returns the number of hook entries added. Stale Entire commands from older
// versions are dropped on every install, not only under --force, so an upgrade
// never leaves two Entire hooks firing per event.
func (a *QwenCodeAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	path, err := settingsPath(ctx)
	if err != nil {
		return 0, err
	}
	raw, hooks, err := loadSettings(path)
	if err != nil {
		return 0, err
	}

	want := wantedCommands()
	wantList := make([]string, 0, len(want))
	for _, cmd := range want {
		wantList = append(wantList, cmd)
	}

	added, changed := 0, false
	for event, command := range want {
		var entries []hookMatcherEntry
		if existing, ok := hooks[event]; ok {
			//nolint:errcheck // a malformed entry list is treated as empty and rewritten
			_ = json.Unmarshal(existing, &entries)
		}

		// Drop hooks an older Entire wrote before deciding whether to add.
		kept, dropped := agent.DropStaleManagedHooks(entries,
			func(e hookMatcherEntry) string {
				if len(e.Hooks) == 0 {
					return ""
				}
				return e.Hooks[0].Command
			}, wantList)
		if dropped {
			entries = kept
			changed = true
		}

		if !force && hasCommand(entries, command) {
			continue
		}
		if force {
			entries = withoutCommand(entries, command)
		}

		entries = append(entries, hookMatcherEntry{
			Hooks: []hookCommand{{
				Type:    "command",
				Command: command,
				Timeout: hookTimeoutMillis,
			}},
		})
		added++
		changed = true

		encoded, marshalErr := jsonutil.MarshalWithNoHTMLEscape(entries)
		if marshalErr != nil {
			return 0, fmt.Errorf("marshal qwen hooks for %s: %w", event, marshalErr)
		}
		hooks[event] = encoded
	}

	// Persist when a stale hook was dropped even if nothing was added,
	// otherwise the stale entry survives on disk.
	if !changed {
		return 0, nil
	}
	if err := writeSettings(path, raw, hooks); err != nil {
		return 0, err
	}
	return added, nil
}

// UninstallHooks removes only Entire's hook entries, leaving user hooks and all
// other settings in place.
func (a *QwenCodeAgent) UninstallHooks(ctx context.Context) error {
	path, err := settingsPath(ctx)
	if err != nil {
		return err
	}
	raw, hooks, err := loadSettings(path)
	if err != nil {
		return err
	}
	if len(hooks) == 0 {
		return nil
	}

	for event, encoded := range hooks {
		var entries []hookMatcherEntry
		if err := json.Unmarshal(encoded, &entries); err != nil {
			continue
		}
		kept := make([]hookMatcherEntry, 0, len(entries))
		for _, e := range entries {
			if len(e.Hooks) > 0 && agent.IsManagedHookCommand(e.Hooks[0].Command) {
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(hooks, event)
			continue
		}
		reencoded, marshalErr := jsonutil.MarshalWithNoHTMLEscape(kept)
		if marshalErr != nil {
			return fmt.Errorf("marshal qwen hooks for %s: %w", event, marshalErr)
		}
		hooks[event] = reencoded
	}
	return writeSettings(path, raw, hooks)
}

// AreHooksInstalled reports whether every event Entire manages has a current
// Entire command registered.
func (a *QwenCodeAgent) AreHooksInstalled(ctx context.Context) bool {
	path, err := settingsPath(ctx)
	if err != nil {
		return false
	}
	_, hooks, err := loadSettings(path)
	if err != nil {
		return false
	}
	for event, command := range wantedCommands() {
		encoded, ok := hooks[event]
		if !ok {
			return false
		}
		var entries []hookMatcherEntry
		if err := json.Unmarshal(encoded, &entries); err != nil {
			return false
		}
		if !hasCommand(entries, command) {
			return false
		}
	}
	return true
}

// GetSupportedHooks returns the Qwen event names Entire registers, sorted.
func (a *QwenCodeAgent) GetSupportedHooks() []string {
	events := make([]string, 0, len(qwenHookEvents))
	for _, event := range qwenHookEvents {
		events = append(events, event)
	}
	sort.Strings(events)
	return events
}

func hasCommand(entries []hookMatcherEntry, command string) bool {
	for _, e := range entries {
		for _, h := range e.Hooks {
			if h.Command == command {
				return true
			}
		}
	}
	return false
}

func withoutCommand(entries []hookMatcherEntry, command string) []hookMatcherEntry {
	kept := make([]hookMatcherEntry, 0, len(entries))
	for _, e := range entries {
		if len(e.Hooks) > 0 && e.Hooks[0].Command == command {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// writeSettings persists the settings file, dropping the hooks key entirely
// when no hooks remain so an empty object is not left behind.
func writeSettings(path string, raw, hooks map[string]json.RawMessage) error {
	if len(hooks) == 0 {
		delete(raw, hooksKey)
	} else {
		encoded, err := jsonutil.MarshalWithNoHTMLEscape(hooks)
		if err != nil {
			return fmt.Errorf("marshal qwen hooks: %w", err)
		}
		raw[hooksKey] = encoded
	}

	if len(raw) == 0 {
		// Nothing left worth keeping; remove rather than writing "{}".
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove empty qwen settings: %w", err)
		}
		return nil
	}

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal qwen settings: %w", err)
	}
	//nolint:gosec // G301: qwen reads this directory
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create qwen config dir: %w", err)
	}
	//nolint:gosec // G306: qwen reads this file
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write qwen settings: %w", err)
	}
	return nil
}
