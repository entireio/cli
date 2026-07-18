package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// HookTrustGaps returns the snake_case event labels declared in
// <repoRoot>/.codex/hooks.json that don't have a matching
// `[hooks.state.<"hooks.json:event:0:0">]` entry (any TOML key quoting)
// in the user's Codex config.toml — i.e. events the local user hasn't
// approved yet.
//
// This is the structural form of the trust check: we don't recompute
// Codex's hook hash, we only look at key presence. That misses the
// "command changed but key is still there" case (status = Modified),
// but Codex's own startup warning catches those — our purpose here is
// to surface fresh additions like "you trusted three hooks last month
// but a new PostToolUse arrived" inside our SessionStart welcome.
//
// The bool reports whether the check was actually performed. It is
// false — with nil gaps — when hooks.json is missing/malformed or the
// user's config.toml can't be read or parsed as TOML: "can't tell"
// cases where the caller must not claim the hooks were verified.
// Callers that only warn on gaps (the SessionStart banner) can ignore
// it; doctor uses it to say "not verified" instead of "OK".
func HookTrustGaps(repoRoot string) ([]string, bool) {
	hooksJSONPath := filepath.Join(repoRoot, ".codex", "hooks.json")
	declared, ok := declaredCodexEvents(hooksJSONPath)
	if !ok {
		return nil, false
	}
	if len(declared) == 0 {
		return nil, true
	}

	configPath := codexConfigPath()
	if configPath == "" {
		return nil, false
	}
	trusted, ok := readCodexTrustedKeys(configPath)
	if !ok {
		return nil, false
	}

	var gaps []string
	for _, ev := range declared {
		// Match any handler index — Codex's state key is
		// "<path>:<event>:<group>:<handler>". Trust on any handler counts.
		prefix := hooksJSONPath + ":" + ev + ":"
		if !codexAnyKeyHasPrefix(trusted, prefix) {
			gaps = append(gaps, ev)
		}
	}
	return gaps, true
}

func codexConfigPath() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "config.toml")
}

// declaredCodexEvents reads hooks.json and returns the snake_case labels
// of every event that has at least one handler declared. The bool reports
// whether the read+parse succeeded — false on missing/malformed file so
// callers can stay silent rather than mid-flow noise.
func declaredCodexEvents(hooksJSONPath string) ([]string, bool) {
	data, err := os.ReadFile(hooksJSONPath) //nolint:gosec // path constructed from caller-controlled repo root
	if err != nil {
		return nil, false
	}
	var file HooksFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, false
	}
	var events []string
	add := func(label string, groups []MatcherGroup) {
		for _, g := range groups {
			if len(g.Hooks) > 0 {
				events = append(events, label)
				return
			}
		}
	}
	add("session_start", file.Hooks.SessionStart)
	add("user_prompt_submit", file.Hooks.UserPromptSubmit)
	add("stop", file.Hooks.Stop)
	add("pre_tool_use", file.Hooks.PreToolUse)
	add("post_tool_use", file.Hooks.PostToolUse)
	return events, true
}

// MissingEntireHooks returns the snake_case event labels the CLI's
// canonical install ships today (SessionStart, UserPromptSubmit, Stop,
// PostToolUse) that aren't backed by an Entire-managed hook command in
// <repoRoot>/.codex/hooks.json. Surfaces drift when the user enabled
// Codex on an older release and the install set has since grown.
//
// Returns nil when hooks.json is missing or unreadable — those cases
// are "Codex isn't enabled here", which is a different problem.
func MissingEntireHooks(repoRoot string) []string {
	hooksJSONPath := filepath.Join(repoRoot, ".codex", "hooks.json")
	data, err := os.ReadFile(hooksJSONPath) //nolint:gosec // path constructed from caller-controlled repo root
	if err != nil {
		return nil
	}
	var file HooksFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil
	}
	var missing []string
	check := func(label string, groups []MatcherGroup) {
		if !hasEntireHook(groups) {
			missing = append(missing, label)
		}
	}
	check("session_start", file.Hooks.SessionStart)
	check("user_prompt_submit", file.Hooks.UserPromptSubmit)
	check("stop", file.Hooks.Stop)
	check("post_tool_use", file.Hooks.PostToolUse)
	return missing
}

// readCodexTrustedKeys parses the user's Codex config.toml and returns
// the set of keys under the `hooks.state` table. The parse is
// structural (not a header regex): Codex's writer has emitted both
// basic (double-quoted) and literal (single-quoted) TOML keys — on
// Windows the backslashes in the hooks.json path make literal quoting
// the natural serialization — and a real TOML parse handles every
// quoting form, unescaping basic strings so keys compare cleanly
// against filepath.Join output (issue #1761).
//
// Returns ok=false when the file is missing or not valid TOML — "can't
// tell" cases where callers must stay silent rather than flag every
// hook as untrusted. A parseable config without a hooks.state table
// returns an empty set with ok=true: that's a real "nothing approved
// yet" state and the gap warning should fire.
func readCodexTrustedKeys(configPath string) (map[string]struct{}, bool) {
	data, err := os.ReadFile(configPath) //nolint:gosec // path resolved from CODEX_HOME or HOME
	if err != nil {
		return nil, false
	}
	var config map[string]any
	if err := toml.Unmarshal(data, &config); err != nil {
		return nil, false
	}
	keys := make(map[string]struct{})
	hooks, ok := config["hooks"].(map[string]any)
	if !ok {
		return keys, true
	}
	state, ok := hooks["state"].(map[string]any)
	if !ok {
		return keys, true
	}
	for k := range state {
		keys[k] = struct{}{}
	}
	return keys, true
}

func codexAnyKeyHasPrefix(keys map[string]struct{}, prefix string) bool {
	for k := range keys {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}
