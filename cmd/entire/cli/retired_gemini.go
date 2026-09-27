package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// Gemini CLI support was removed, but repositories that enabled it still carry
// Entire hooks in .gemini/settings.json, and nothing re-runs setup when the CLI
// is upgraded. Two things keep those repositories working until the entries are
// gone: `entire hooks gemini <verb>` exits cleanly instead of failing every
// Gemini event (see newHooksCmd), and `entire doctor` and
// `entire disable --uninstall` remove the entries.

// retiredGeminiAgentName is the hook namespace Gemini CLI support used
// (`entire hooks gemini <verb>`).
const retiredGeminiAgentName types.AgentName = "gemini"

// retiredGeminiHookConfigRelPath is where Gemini CLI support installed hooks.
const retiredGeminiHookConfigRelPath = ".gemini/settings.json"

// retiredGeminiLegacyHooksKey is the non-array value old Entire versions wrote
// directly under "hooks" ("enabled": true). Gemini CLI 0.33+ rejects any
// non-array hooks property.
const retiredGeminiLegacyHooksKey = "enabled"

// retiredGeminiNameClaimed reports whether an external plugin owns the
// "gemini" name, registered or merely installed on $PATH. Every retired-Gemini
// path defers to it: the hooks such a plugin installs look exactly like the
// ones removed support left behind, so they must be neither no-op'd nor
// stripped. Checking $PATH without executing the plugin keeps this usable from
// doctor, which must not run plugins, and covers a plugin that discovery
// skipped (a timeout, or external_agents not enabled).
func retiredGeminiNameClaimed() bool {
	if _, err := agent.Get(retiredGeminiAgentName); err == nil {
		return true
	}
	return external.BinaryOnPath(retiredGeminiAgentName)
}

// removeRetiredGeminiHooks removes Entire-managed hook entries from the
// worktree's .gemini/settings.json and reports whether it changed the file.
// Other hooks, matchers, and settings are preserved field for field; a hook
// type left with no matchers is dropped, as Gemini CLI support's own uninstall
// did. A missing file is not an error. The file keeps its permissions.
func removeRetiredGeminiHooks(worktreeRoot string) (bool, error) {
	cfg, output, changed, err := planRetiredGeminiHookRemoval(worktreeRoot)
	if err != nil || !changed {
		return false, err
	}
	perm := os.FileMode(0o644)
	if root, name := cfg.Root(); root != nil {
		if info, statErr := root.Lstat(name); statErr == nil {
			perm = info.Mode().Perm()
		}
	}
	if err := cfg.Write(output, perm); err != nil {
		return false, err //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
	}
	return true, nil
}

// retiredGeminiHooksInstalled reports whether the worktree's
// .gemini/settings.json still holds Entire-managed hook entries, without
// writing anything. An error means the file could not be read or parsed, which
// is not the same answer as "none": callers deciding whether there is anything
// to clean up must not treat it as absence.
func retiredGeminiHooksInstalled(worktreeRoot string) (bool, error) {
	_, _, changed, err := planRetiredGeminiHookRemoval(worktreeRoot)
	return changed, err
}

// planRetiredGeminiHookRemoval reads .gemini/settings.json and returns the
// file handle and its content with Entire's entries stripped. changed is false
// when the file is missing or holds no Entire entries, and when a plugin has
// claimed the "gemini" name (see retiredGeminiNameClaimed).
//
// A symlinked .gemini (a dotfiles checkout, say) also reads as nothing to do.
// Entire refuses to write through it, and .gemini is no longer vouchable, so
// reporting it would be an error on every doctor run that no remedy clears.
// Entries left behind it are harmless: the hook command they run is a no-op.
func planRetiredGeminiHookRemoval(worktreeRoot string) (*agent.HookConfigFile, []byte, bool, error) {
	if retiredGeminiNameClaimed() {
		return nil, nil, false, nil
	}
	cfg, err := agent.OpenHookConfig(worktreeRoot, retiredGeminiHookConfigRelPath)
	if errors.Is(err, osroot.ErrSymlinkedPath) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
	}
	data, err := cfg.Read()
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, osroot.ErrSymlinkedPath) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("read %s: %w", cfg.Path(), err)
	}
	output, changed, err := stripRetiredGeminiHooks(data)
	if err != nil {
		return nil, nil, false, fmt.Errorf("%s: %w", cfg.Path(), err)
	}
	return cfg, output, changed, nil
}

// stripRetiredGeminiHooks returns settings with every Entire-managed hook
// command removed. When it removes any, it also drops the legacy
// "hooks.enabled" old Entire versions wrote there, which Gemini CLI 0.33+
// rejects (every hooks property must be an array), so the rewritten file still
// loads. That key never counts as Entire's on its own: a file holding it and no
// Entire entries is left untouched. Other values it does not recognize (another
// non-array hooks key, a matcher without a hooks list) are left as they are.
func stripRetiredGeminiHooks(data []byte) ([]byte, bool, error) {
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return nil, false, fmt.Errorf("parse settings: %w", err)
	}
	hooksRaw, ok := rawSettings["hooks"]
	if !ok {
		return nil, false, nil
	}
	var rawHooks map[string]json.RawMessage
	if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
		return nil, false, fmt.Errorf("parse hooks: %w", err)
	}

	changed := false
	for hookType, value := range rawHooks {
		var matchers []map[string]json.RawMessage
		if json.Unmarshal(value, &matchers) != nil {
			continue
		}
		kept, matchersChanged, err := stripManagedGeminiEntries(matchers)
		if err != nil {
			return nil, false, err
		}
		if !matchersChanged {
			continue
		}
		changed = true
		if len(kept) == 0 {
			delete(rawHooks, hookType)
			continue
		}
		encoded, err := jsonutil.MarshalWithNoHTMLEscape(kept)
		if err != nil {
			return nil, false, fmt.Errorf("marshal %s hooks: %w", hookType, err)
		}
		rawHooks[hookType] = encoded
	}
	if !changed {
		return nil, false, nil
	}
	if legacy, ok := rawHooks[retiredGeminiLegacyHooksKey]; ok {
		if trimmed := bytes.TrimSpace(legacy); len(trimmed) > 0 && trimmed[0] != '[' {
			delete(rawHooks, retiredGeminiLegacyHooksKey)
		}
	}

	if len(rawHooks) == 0 {
		delete(rawSettings, "hooks")
	} else {
		encoded, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
		if err != nil {
			return nil, false, fmt.Errorf("marshal hooks: %w", err)
		}
		rawSettings["hooks"] = encoded
	}
	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("marshal settings: %w", err)
	}
	return output, true, nil
}

// stripManagedGeminiEntries drops Entire-managed entries from each matcher's
// hooks list, and drops a matcher once its list is empty.
func stripManagedGeminiEntries(matchers []map[string]json.RawMessage) ([]map[string]json.RawMessage, bool, error) {
	kept := make([]map[string]json.RawMessage, 0, len(matchers))
	changed := false
	for _, matcher := range matchers {
		var entries []map[string]json.RawMessage
		if json.Unmarshal(matcher["hooks"], &entries) != nil {
			kept = append(kept, matcher)
			continue
		}
		remaining := make([]map[string]json.RawMessage, 0, len(entries))
		for _, entry := range entries {
			var command string
			if json.Unmarshal(entry["command"], &command) == nil && agent.IsManagedHookCommand(command) {
				changed = true
				continue
			}
			remaining = append(remaining, entry)
		}
		if len(remaining) == 0 && len(entries) > 0 {
			continue
		}
		if len(remaining) != len(entries) {
			encoded, err := jsonutil.MarshalWithNoHTMLEscape(remaining)
			if err != nil {
				return nil, false, fmt.Errorf("marshal hook entries: %w", err)
			}
			matcher["hooks"] = encoded
		}
		kept = append(kept, matcher)
	}
	return kept, changed, nil
}
