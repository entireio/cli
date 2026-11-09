package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// Agents and the review-profile config fields each one can apply.
var agentConfigFields = map[string][]string{
	"claude-code": {"settings", "mcp_servers"},
	"codex":       {"mcp_servers"},
	"pi":          {"extensions"},
}

var mcpServerNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// codexMCPFields are the MCP server fields a Codex reviewer passes through.
// "type" is accepted and dropped: Codex infers it from command or url.
var codexMCPFields = []string{"command", "args", "url", "env", "default_tools_approval_mode", "type"}

// toAgentConfig converts a profile's config for a run; nil stays nil.
func toAgentConfig(cfg *settings.ReviewAgentConfig) *reviewtypes.AgentConfig {
	if cfg == nil {
		return nil
	}
	return &reviewtypes.AgentConfig{Settings: cfg.Settings, MCPServers: cfg.MCPServers, Extensions: cfg.Extensions}
}

// ValidateAgentConfig checks that a reviewer's profile config is one the agent
// can apply. It does not vet the commands the config names: like the hooks in
// the user's own agent settings, they are the user's code and run in the
// checkout as configured.
func ValidateAgentConfig(agentName string, cfg *reviewtypes.AgentConfig) error {
	if cfg == nil {
		return nil
	}
	allowed, known := agentConfigFields[agentName]
	if !known {
		return fmt.Errorf("%s does not support a review profile config", agentName)
	}
	check := func(field string, set bool) error {
		if set && !slices.Contains(allowed, field) {
			return fmt.Errorf("%s does not support %q in a review profile config", agentName, field)
		}
		return nil
	}
	if err := errors.Join(
		check("settings", len(cfg.Settings) > 0),
		check("mcp_servers", len(cfg.MCPServers) > 0),
		check("extensions", len(cfg.Extensions) > 0),
	); err != nil {
		return err
	}
	if len(cfg.Settings) > 0 {
		obj, err := jsonObject(cfg.Settings)
		if err != nil {
			return fmt.Errorf("settings: %w", err)
		}
		if hooks, ok := obj["hooks"]; ok {
			events, err := jsonObject(hooks)
			if err != nil {
				return fmt.Errorf("settings hooks: %w", err)
			}
			for _, event := range sortedStringKeys(events) {
				var matchers []map[string]json.RawMessage
				if err := json.Unmarshal(events[event], &matchers); err != nil || matchers == nil || slices.ContainsFunc(matchers, func(m map[string]json.RawMessage) bool { return m == nil }) {
					return fmt.Errorf("settings hooks %s: must be a list of matcher objects", event)
				}
			}
		}
	}
	for _, name := range sortedStringKeys(cfg.MCPServers) {
		if !mcpServerNamePattern.MatchString(name) {
			return fmt.Errorf("MCP server name %q: use letters, digits, '-' and '_'", name)
		}
		var server struct {
			Command string            `json:"command"`
			URL     string            `json:"url"`
			Env     map[string]string `json:"env"`
		}
		if err := json.Unmarshal(cfg.MCPServers[name], &server); err != nil {
			return fmt.Errorf("MCP server %q: %w", name, err)
		}
		if server.Command == "" && server.URL == "" {
			return fmt.Errorf("MCP server %q needs a command or a url", name)
		}
		if server.Command != "" && server.URL != "" {
			return fmt.Errorf("MCP server %q: set a command or a url, not both", name)
		}
		if agentName == "codex" {
			if err := checkCodexMCPFields(name, cfg.MCPServers[name]); err != nil {
				return err
			}
			if len(server.Env) > 0 {
				return fmt.Errorf("MCP server %q: env values aren't supported for Codex in a review profile yet", name)
			}
		}
	}
	// A relative extension would load from the reviewed checkout, the one
	// thing a profile config promises not to do.
	for _, ext := range cfg.Extensions {
		if !filepath.IsAbs(ext) {
			return fmt.Errorf("extension %q: use an absolute path", ext)
		}
	}
	return nil
}

// jsonObject decodes raw as a JSON object; null is refused, since the agent
// would treat it as missing.
func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err //nolint:wrapcheck // callers name the field
	}
	if obj == nil {
		return nil, errors.New("must be an object, not null")
	}
	return obj, nil
}

// checkCodexMCPFields refuses fields a Codex reviewer would not pass on, so a
// server doesn't run without, say, its auth header.
func checkCodexMCPFields(name string, raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("MCP server %q: %w", name, err)
	}
	for _, field := range sortedStringKeys(fields) {
		if !slices.Contains(codexMCPFields, field) {
			return fmt.Errorf("MCP server %q: %q isn't supported for Codex in a review profile yet", name, field)
		}
	}
	return nil
}

// AgentConfigWarnings lists values in cfg that name a path inside one of
// roots or the project directory variable: an easy copy-paste mistake, since
// those would run the reviewed branch's files. They are warnings, not
// refusals; the commands are the user's own.
func AgentConfigWarnings(cfg *reviewtypes.AgentConfig, roots []string) []string {
	if cfg == nil {
		return nil
	}
	var values []string
	collectStrings(cfg.Settings, &values)
	for _, name := range sortedStringKeys(cfg.MCPServers) {
		collectStrings(cfg.MCPServers[name], &values)
	}
	values = append(values, cfg.Extensions...)
	var warnings []string
	for _, value := range values {
		if namesCheckout(value, roots) {
			warnings = append(warnings, fmt.Sprintf("%q names a path inside the checkout; it would run the reviewed branch's files", value))
		}
	}
	return warnings
}

func collectStrings(raw json.RawMessage, out *[]string) {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return
	}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			*out = append(*out, t)
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, k := range sortedStringKeys(t) {
				walk(t[k])
			}
		}
	}
	walk(v)
}

func namesCheckout(value string, roots []string) bool {
	if strings.Contains(value, "CLAUDE_PROJECT_DIR") {
		return true
	}
	words := strings.FieldsFunc(value, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '=' || r == '"' || r == '\'' || r == filepath.ListSeparator
	})
	for _, word := range words {
		if filepath.IsAbs(word) && under(word, roots) {
			return true
		}
	}
	return false
}

// under reports whether path, or what it resolves to through symlinks, is
// inside one of roots. Windows paths compare case-insensitively.
func under(path string, roots []string) bool {
	candidates := []string{filepath.Clean(path), resolveExisting(path)}
	for _, candidate := range candidates {
		for _, root := range roots {
			if root != "" && pathWithin(candidate, filepath.Clean(root)) {
				return true
			}
		}
	}
	return false
}

// resolveExisting resolves symlinks in the longest existing prefix of path,
// so a file that doesn't exist yet still resolves through a linked parent.
func resolveExisting(path string) string {
	dir, rest := filepath.Clean(path), ""
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Clean(path)
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

func pathWithin(path, root string) bool {
	if runtime.GOOS == "windows" {
		path, root = strings.ToLower(path), strings.ToLower(root)
	}
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}

func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
