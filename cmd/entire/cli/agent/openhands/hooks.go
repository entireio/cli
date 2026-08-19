package openhands

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
	// hooksFileName is the file OpenHands loads hooks from.
	hooksFileName = "hooks.json"

	// hookTimeoutSeconds bounds each hook command. OpenHands' own default is 60.
	hookTimeoutSeconds = 60

	// matchAll is HookMatcher's default matcher.
	matchAll = "*"
)

// hooksPath returns the absolute path to the project hooks file.
//
// OpenHands also reads ~/.openhands/hooks.json, but Entire installs per repo,
// matching every other agent it supports.
func hooksPath(ctx context.Context) (string, error) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		//nolint:forbidigo // explicit fallback when WorktreeRoot fails
		root, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve repo root: %w", err)
		}
	}
	return filepath.Join(root, configDirName, hooksFileName), nil
}

// wantedCommands returns the hook commands Entire installs today, keyed by
// OpenHands config field.
func wantedCommands() map[string]string {
	out := make(map[string]string, len(openhandsHookEvents))
	for verb, field := range openhandsHookEvents {
		out[field] = agent.WrapProductionSilentHookCommand("entire hooks openhands " + verb)
	}
	return out
}

// loadHooks reads the hooks file as a raw field map.
//
// HookConfig sets extra="forbid", so OpenHands rejects the whole file if it
// contains a key it does not recognise. Keeping the document raw means Entire
// never introduces one, and any hook type it does not model is preserved.
func loadHooks(path string) (map[string]json.RawMessage, error) {
	raw := make(map[string]json.RawMessage)
	data, err := os.ReadFile(path) //nolint:gosec // path built from repo root
	if err != nil {
		if os.IsNotExist(err) {
			return raw, nil
		}
		return nil, fmt.Errorf("read openhands hooks: %w", err)
	}
	if len(data) == 0 {
		return raw, nil
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse openhands hooks: %w", err)
	}
	// Unwrap the legacy {"hooks": {...}} form OpenHands still accepts, so an
	// existing file in that shape is not double-nested on rewrite.
	if inner, ok := raw["hooks"]; ok && len(raw) == 1 {
		nested := make(map[string]json.RawMessage)
		if err := json.Unmarshal(inner, &nested); err == nil {
			return nested, nil
		}
	}
	return raw, nil
}

// InstallHooks writes Entire's hook entries into .openhands/hooks.json.
// Returns the number of entries added.
//
// Stale Entire commands are dropped on every install, not only under --force,
// so an upgrade never leaves two Entire hooks firing per event.
func (a *OpenHandsAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	path, err := hooksPath(ctx)
	if err != nil {
		return 0, err
	}
	hooks, err := loadHooks(path)
	if err != nil {
		return 0, err
	}

	want := wantedCommands()
	wantList := make([]string, 0, len(want))
	for _, cmd := range want {
		wantList = append(wantList, cmd)
	}

	added, changed := 0, false
	for field, command := range want {
		var entries []hookMatcher
		if existing, ok := hooks[field]; ok {
			//nolint:errcheck // a malformed entry list is treated as empty and rewritten
			_ = json.Unmarshal(existing, &entries)
		}

		kept, dropped := agent.DropStaleManagedHooks(entries,
			func(m hookMatcher) string {
				if len(m.Hooks) == 0 {
					return ""
				}
				return m.Hooks[0].Command
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

		entries = append(entries, hookMatcher{
			Matcher: matchAll,
			Hooks: []hookDefinition{{
				Type:    "command",
				Command: command,
				Timeout: hookTimeoutSeconds,
			}},
		})
		added++
		changed = true

		encoded, marshalErr := jsonutil.MarshalWithNoHTMLEscape(entries)
		if marshalErr != nil {
			return 0, fmt.Errorf("marshal openhands hooks for %s: %w", field, marshalErr)
		}
		hooks[field] = encoded
	}

	if !changed {
		return 0, nil
	}
	if err := writeHooks(path, hooks); err != nil {
		return 0, err
	}
	return added, nil
}

// UninstallHooks removes only Entire's entries, leaving user hooks in place.
func (a *OpenHandsAgent) UninstallHooks(ctx context.Context) error {
	path, err := hooksPath(ctx)
	if err != nil {
		return err
	}
	hooks, err := loadHooks(path)
	if err != nil {
		return err
	}
	if len(hooks) == 0 {
		return nil
	}

	for field, encoded := range hooks {
		var entries []hookMatcher
		if err := json.Unmarshal(encoded, &entries); err != nil {
			continue
		}
		kept := make([]hookMatcher, 0, len(entries))
		for _, m := range entries {
			if len(m.Hooks) > 0 && agent.IsManagedHookCommand(m.Hooks[0].Command) {
				continue
			}
			kept = append(kept, m)
		}
		if len(kept) == 0 {
			delete(hooks, field)
			continue
		}
		reencoded, marshalErr := jsonutil.MarshalWithNoHTMLEscape(kept)
		if marshalErr != nil {
			return fmt.Errorf("marshal openhands hooks for %s: %w", field, marshalErr)
		}
		hooks[field] = reencoded
	}
	return writeHooks(path, hooks)
}

// AreHooksInstalled reports whether every managed event has a current Entire
// command registered.
func (a *OpenHandsAgent) AreHooksInstalled(ctx context.Context) bool {
	path, err := hooksPath(ctx)
	if err != nil {
		return false
	}
	hooks, err := loadHooks(path)
	if err != nil {
		return false
	}
	for field, command := range wantedCommands() {
		encoded, ok := hooks[field]
		if !ok {
			return false
		}
		var entries []hookMatcher
		if err := json.Unmarshal(encoded, &entries); err != nil {
			return false
		}
		if !hasCommand(entries, command) {
			return false
		}
	}
	return true
}

// GetSupportedHooks returns the OpenHands config fields Entire registers.
func (a *OpenHandsAgent) GetSupportedHooks() []string {
	fields := make([]string, 0, len(openhandsHookEvents))
	for _, field := range openhandsHookEvents {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

func hasCommand(entries []hookMatcher, command string) bool {
	for _, m := range entries {
		for _, h := range m.Hooks {
			if h.Command == command {
				return true
			}
		}
	}
	return false
}

func withoutCommand(entries []hookMatcher, command string) []hookMatcher {
	kept := make([]hookMatcher, 0, len(entries))
	for _, m := range entries {
		if len(m.Hooks) > 0 && m.Hooks[0].Command == command {
			continue
		}
		kept = append(kept, m)
	}
	return kept
}

// writeHooks persists the hooks file, removing it entirely when nothing is left
// so OpenHands is not handed an empty document.
func writeHooks(path string, hooks map[string]json.RawMessage) error {
	if len(hooks) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove empty openhands hooks: %w", err)
		}
		return nil
	}
	out, err := json.MarshalIndent(hooks, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal openhands hooks: %w", err)
	}
	//nolint:gosec // G301: openhands reads this directory
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create openhands config dir: %w", err)
	}
	//nolint:gosec // G306: openhands reads this file
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write openhands hooks: %w", err)
	}
	return nil
}
