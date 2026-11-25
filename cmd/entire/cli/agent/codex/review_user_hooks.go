package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// userEntireHookStateOverride returns a -c override that disables, for one
// isolated review run, the Entire hooks in the user's own Codex config
// ($CODEX_HOME/hooks.json and config.toml). Those layers still load when the
// checkout is untrusted, and the run registers Entire's hooks itself, so
// without this every event would fire twice. The user's other hooks are left
// alone. Codex merges a hooks.state table override with the user's own
// hooks.state entries rather than replacing them.
//
// A file that can't be read is skipped with a warning: the worst case is the
// duplicate hooks this exists to prevent.
func userEntireHookStateOverride(ctx context.Context) []string {
	dir, isOverride, err := agent.LookupOverride("CODEX_HOME")
	if err == nil && !isOverride {
		dir, err = resolveCodexHome()
	}
	if err != nil {
		logging.Warn(ctx, "review: codex home unresolved; user-level Entire hooks are not disabled",
			slog.String("error", err.Error()))
		return nil
	}
	return ReviewUserHookStateOverride(ctx, dir, isOverride)
}

// ReviewUserHookStateOverride is userEntireHookStateOverride for an explicit
// Codex home: codexHome is the CODEX_HOME value when isOverride, else the
// default ~/.codex. Exported for the pinned app-server contract test.
func ReviewUserHookStateOverride(ctx context.Context, codexHome string, isOverride bool) []string {
	home, err := codexHomeForHookKeys(codexHome, isOverride)
	if err != nil {
		logging.Warn(ctx, "review: codex home unresolved; user-level Entire hooks are not disabled",
			slog.String("error", err.Error()))
		return nil
	}
	var keys []string
	for _, source := range []struct {
		name  string
		parse func([]byte) (map[string][]MatcherGroup, error)
	}{
		{HooksFileName, parseHooksJSONGroups},
		{"config.toml", parseConfigTOMLGroups},
	} {
		path := filepath.Join(home, source.name)
		found, err := userEntireHookKeys(path, source.parse)
		if err != nil {
			logging.Warn(ctx, "review: user-level Entire hooks in this codex file are not disabled",
				slog.String("path", path), slog.String("error", err.Error()))
			continue
		}
		keys = append(keys, found...)
	}
	return hookStateDisableOverride(keys)
}

// codexHomeForHookKeys returns the Codex home as Codex spells it in hook
// keys: Codex canonicalizes a CODEX_HOME override (symlinks resolved), but
// uses the default ~/.codex as joined from $HOME. A key spelled differently
// matches nothing, so the hook would stay enabled.
func codexHomeForHookKeys(dir string, isOverride bool) (string, error) {
	if !isOverride {
		return dir, nil
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolve CODEX_HOME: %w", err)
	}
	return resolved, nil
}

// userEntireHookKeys returns the Codex hook-state keys
// (<path>:<event label>:<group>:<handler>) of the Entire handlers in path. A
// missing file has none.
func userEntireHookKeys(path string, parse func([]byte) (map[string][]MatcherGroup, error)) ([]string, error) {
	data, err := readUserCodexFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	byEvent, err := parse(data)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, spec := range HookEventSpecs() {
		for g, group := range byEvent[spec.Event] {
			for h, hook := range group.Hooks {
				if isEntireHook(hook.Command) {
					keys = append(keys, path+":"+spec.Label+":"+strconv.Itoa(g)+":"+strconv.Itoa(h))
				}
			}
		}
	}
	return keys, nil
}

// readUserCodexFile reads a file in the user's Codex home, bounded like the
// project hooks file. The home is the user's own (it may be a symlink into a
// dotfiles checkout), so it is opened directly, as config.toml is elsewhere in
// this package.
func readUserCodexFile(path string) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // path resolved from CODEX_HOME or HOME
	if err != nil {
		return nil, err //nolint:wrapcheck // callers check os.ErrNotExist and name the path
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxHooksFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(data) > maxHooksFileBytes {
		return nil, fmt.Errorf("over %d bytes", maxHooksFileBytes)
	}
	return data, nil
}

// parseHooksJSONGroups parses a hooks.json document into its matcher groups
// by event, keeping each group's and handler's position.
func parseHooksJSONGroups(data []byte) (map[string][]MatcherGroup, error) {
	var doc struct {
		Hooks map[string][]MatcherGroup `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return doc.Hooks, nil
}

// parseConfigTOMLGroups parses the [[hooks.<Event>]] tables of a Codex
// config.toml, keeping each group's and handler's position.
func parseConfigTOMLGroups(data []byte) (map[string][]MatcherGroup, error) {
	type tomlHook struct {
		Command string `toml:"command"`
	}
	type tomlGroup struct {
		Hooks []tomlHook `toml:"hooks"`
	}
	var doc struct {
		Hooks map[string]any `toml:"hooks"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	out := make(map[string][]MatcherGroup)
	for _, spec := range HookEventSpecs() {
		raw, ok := doc.Hooks[spec.Event]
		if !ok {
			continue
		}
		// Re-decode just this event so the other [hooks] keys (state, ...)
		// don't have to fit the group shape.
		encoded, err := toml.Marshal(map[string]any{"groups": raw})
		if err != nil {
			return nil, fmt.Errorf("parse hooks.%s: %w", spec.Event, err)
		}
		var event struct {
			Groups []tomlGroup `toml:"groups"`
		}
		if err := toml.Unmarshal(encoded, &event); err != nil {
			return nil, fmt.Errorf("parse hooks.%s: %w", spec.Event, err)
		}
		groups := make([]MatcherGroup, len(event.Groups))
		for g, group := range event.Groups {
			for _, hook := range group.Hooks {
				groups[g].Hooks = append(groups[g].Hooks, HookEntry{Command: hook.Command})
			}
		}
		out[spec.Event] = groups
	}
	return out, nil
}

// hookStateDisableOverride builds one -c hooks.state={...} override that sets
// enabled=false on each key. Keys are JSON-quoted, which is valid TOML.
func hookStateDisableOverride(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	entries := make([]string, 0, len(keys))
	for _, key := range keys {
		quoted, err := json.Marshal(key)
		if err != nil {
			continue
		}
		entries = append(entries, string(quoted)+"={enabled=false}")
	}
	return []string{"-c", "hooks.state={" + strings.Join(entries, ",") + "}"}
}
