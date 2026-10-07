package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
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

// agentConfigCommandKeys are Claude Code settings whose value is a command.
var agentConfigCommandKeys = []string{"apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "otelHeadersHelper"}

// projectLaunchers resolve the tool they run from the project before the
// user's install (local node_modules, project venvs), so the reviewed branch
// would choose the code.
var projectLaunchers = []string{"npx", "pnpx", "bunx", "uvx", "yarn", "pnpm", "bun", "deno", "uv", "poetry", "pipx"}

var mcpServerNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// toAgentConfig converts a profile's config for a run; nil stays nil.
func toAgentConfig(cfg *settings.ReviewAgentConfig) *reviewtypes.AgentConfig {
	if cfg == nil {
		return nil
	}
	return &reviewtypes.AgentConfig{Settings: cfg.Settings, MCPServers: cfg.MCPServers, Extensions: cfg.Extensions}
}

// ValidateAgentConfig checks a reviewer's profile config before it is saved or
// used. Every command it names must be an absolute path or a bare tool name,
// and none may resolve inside forbiddenRoots (the reviewed checkout and the
// user's own), or the reviewed branch would supply the code that runs.
func ValidateAgentConfig(agentName string, cfg *reviewtypes.AgentConfig, forbiddenRoots []string) error {
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
	for _, name := range sortedStringKeys(cfg.MCPServers) {
		if !mcpServerNamePattern.MatchString(name) {
			return fmt.Errorf("MCP server name %q: use letters, digits, '-' and '_'", name)
		}
		if err := validateMCPServer(name, cfg.MCPServers[name], forbiddenRoots); err != nil {
			return err
		}
	}
	if len(cfg.Settings) > 0 {
		if err := validateClaudeSettings(cfg.Settings, forbiddenRoots); err != nil {
			return err
		}
	}
	for _, ext := range cfg.Extensions {
		if !filepath.IsAbs(ext) {
			return fmt.Errorf("extension %q: use an absolute path", ext)
		}
		if under(ext, forbiddenRoots) {
			return fmt.Errorf("extension %q is inside a checkout; keep reviewer extensions outside the repository", ext)
		}
	}
	return nil
}

func validateMCPServer(name string, raw json.RawMessage, forbiddenRoots []string) error {
	var server struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		URL     string   `json:"url"`
	}
	if err := json.Unmarshal(raw, &server); err != nil {
		return fmt.Errorf("MCP server %q: %w", name, err)
	}
	switch {
	case server.Command != "":
		if err := validateCommand(server.Command, forbiddenRoots); err != nil {
			return fmt.Errorf("MCP server %q: %w", name, err)
		}
		for _, arg := range server.Args {
			if strings.Contains(arg, "CLAUDE_PROJECT_DIR") || (filepath.IsAbs(arg) && under(arg, forbiddenRoots)) {
				return fmt.Errorf("MCP server %q: argument %q points into a checkout", name, arg)
			}
		}
	case server.URL == "":
		return fmt.Errorf("MCP server %q needs a command or a url", name)
	}
	return nil
}

func validateClaudeSettings(raw json.RawMessage, forbiddenRoots []string) error {
	var settingsObj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settingsObj); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	for _, key := range agentConfigCommandKeys {
		var command string
		if json.Unmarshal(settingsObj[key], &command) == nil && command != "" {
			if err := validateCommand(command, forbiddenRoots); err != nil {
				return fmt.Errorf("settings %s: %w", key, err)
			}
		}
	}
	var events map[string][]struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if hooks, ok := settingsObj["hooks"]; ok {
		if err := json.Unmarshal(hooks, &events); err != nil {
			return fmt.Errorf("settings hooks: %w", err)
		}
	}
	for event, groups := range events {
		for _, group := range groups {
			for _, hook := range group.Hooks {
				if hook.Command == "" {
					continue
				}
				if err := validateCommand(hook.Command, forbiddenRoots); err != nil {
					return fmt.Errorf("settings hook %s: %w", event, err)
				}
			}
		}
	}
	return nil
}

// validateCommand accepts a command line whose program is an absolute path
// outside every forbidden root, or a bare tool name found on PATH, and which
// never refers to the project directory.
func validateCommand(command string, forbiddenRoots []string) error {
	if strings.Contains(command, "CLAUDE_PROJECT_DIR") {
		return fmt.Errorf("%q refers to the project directory, which is the reviewed checkout", command)
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return errors.New("empty command")
	}
	program := strings.Trim(fields[0], `"'`)
	switch {
	case filepath.IsAbs(program):
		if under(program, forbiddenRoots) {
			return fmt.Errorf("%q runs a program inside a checkout", command)
		}
	case strings.ContainsAny(program, `/\`):
		return fmt.Errorf("%q uses a relative path, which resolves inside the reviewed checkout; use an absolute path", command)
	case slices.Contains(projectLaunchers, program):
		return fmt.Errorf("%q runs %s, which resolves tools from the reviewed project; use an absolute path to the tool", command, program)
	}
	for _, field := range fields[1:] {
		arg := strings.Trim(field, `"'`)
		if strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") || (filepath.IsAbs(arg) && under(arg, forbiddenRoots)) {
			return fmt.Errorf("%q passes %q, which points into a checkout", command, arg)
		}
	}
	return nil
}

func under(path string, roots []string) bool {
	clean := filepath.Clean(path)
	for _, root := range roots {
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
