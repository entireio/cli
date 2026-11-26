package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

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
// duplicate hooks this exists to prevent. codexIsolatedWarnings tells the
// user when CODEX_HOME itself can't be used.
func userEntireHookStateOverride(ctx context.Context) []string {
	dir, isOverride, err := resolveCodexHomeWithOverride()
	if err != nil {
		logging.Warn(ctx, "review: codex home unresolved; user-level Entire hooks are not disabled",
			slog.String("error", err.Error()))
		return nil
	}
	return ReviewUserHookStateOverride(ctx, dir, isOverride)
}

// codexIsolatedWarnings tells the user, before an isolated run, when
// CODEX_HOME can't be used: Entire's hooks in their own Codex config then
// can't be located and turned off.
func codexIsolatedWarnings(context.Context) []string {
	if _, _, err := resolveCodexHomeWithOverride(); err != nil {
		return []string{err.Error() + "; Entire's hooks in your own Codex config are not turned off for this run, so they may fire twice"}
	}
	return nil
}

// ReviewUserHookStateOverride is userEntireHookStateOverride for an explicit
// Codex home: codexHome is the CODEX_HOME value when isOverride, else the
// default ~/.codex. Exported only as a seam for the pinned app-server
// contract test.
func ReviewUserHookStateOverride(ctx context.Context, codexHome string, isOverride bool) []string {
	home, err := codexHomeForHookKeys(codexHome, isOverride)
	if err != nil {
		logging.Warn(ctx, "review: codex home unresolved; user-level Entire hooks are not disabled",
			slog.String("error", err.Error()))
		return nil
	}
	hooksKeys := userEntireHookKeys(ctx, filepath.Join(home, HooksFileName), decodeJSONDocument)
	configKeys := userEntireHookKeys(ctx, filepath.Join(home, "config.toml"), decodeTOMLDocument)
	return hookStateDisableOverride(append(hooksKeys, configKeys...))
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
// missing file has none; one that can't be read or parsed is skipped with a
// warning.
func userEntireHookKeys(ctx context.Context, path string, decode func([]byte) (map[string]any, error)) []string {
	data, err := readUserCodexFile(path)
	if err == nil {
		var doc map[string]any
		if doc, err = decode(data); err == nil {
			return entireHookKeys(path, doc)
		}
	}
	if !os.IsNotExist(err) {
		logging.Warn(ctx, "review: user-level Entire hooks in this codex file are not disabled",
			slog.String("path", path), slog.String("error", err.Error()))
	}
	return nil
}

// entireHookKeys walks hooks.<Event>[group].hooks[handler].command in a
// decoded hooks.json or config.toml. Keys are matched exactly, as Codex
// matches them: Go's struct decoding is case-insensitive, so a stray
// "COMMAND" key would shift which handler looks like Entire's.
func entireHookKeys(path string, doc map[string]any) []string {
	events, _ := doc["hooks"].(map[string]any) //nolint:errcheck // absent or another shape: no hooks
	var keys []string
	for _, spec := range HookEventSpecs() {
		groups, _ := events[spec.Event].([]any) //nolint:errcheck // absent or another shape: no hooks
		for g, group := range groups {
			groupMap, _ := group.(map[string]any)    //nolint:errcheck // another shape: no handlers
			handlers, _ := groupMap["hooks"].([]any) //nolint:errcheck // another shape: no handlers
			for h, handler := range handlers {
				handlerMap, _ := handler.(map[string]any)    //nolint:errcheck // another shape: not a command
				command, _ := handlerMap["command"].(string) //nolint:errcheck // another shape: not a command
				if isEntireHook(command) {
					keys = append(keys, path+":"+spec.Label+":"+strconv.Itoa(g)+":"+strconv.Itoa(h))
				}
			}
		}
	}
	return keys
}

func decodeJSONDocument(data []byte) (map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return doc, nil
}

func decodeTOMLDocument(data []byte) (map[string]any, error) {
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return doc, nil
}

// readUserCodexFile reads a file in the user's Codex home, bounded like the
// project hooks file. The home is the user's own (it may be a symlink into a
// dotfiles checkout), so it is opened directly rather than through a
// project root.
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
		return nil, fmt.Errorf("too large (over %d bytes)", maxHooksFileBytes)
	}
	return data, nil
}

// hookStateDisableOverride builds one -c hooks.state={...} override that sets
// enabled=false on each key.
func hookStateDisableOverride(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	entries := make([]string, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, tomlQuotedKey(key)+"={enabled=false}")
	}
	return []string{"-c", "hooks.state={" + strings.Join(entries, ",") + "}"}
}

// tomlQuotedKey quotes s as a TOML basic string. JSON string escaping is valid
// TOML except for DEL, which JSON leaves raw and TOML forbids; Codex would
// then read the whole -c value as a plain string and refuse to start.
func tomlQuotedKey(s string) string {
	quoted, _ := json.Marshal(s) //nolint:errcheck,errchkjson // marshaling a string cannot fail
	return strings.ReplaceAll(string(quoted), "\x7f", `\u007f`)
}
