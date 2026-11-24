package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// prepareCodexReviewConfig applies a review profile's agent config: the
// reviewed checkout, and the main repository a linked worktree shares trust
// with, are marked untrusted for this run, which drops their project config,
// hooks and rules; the profile's MCP servers are passed with -c.
func prepareCodexReviewConfig(ctx context.Context, cfg reviewtypes.RunConfig) (reviewtypes.RunConfig, func(), error) {
	if cfg.AgentConfig == nil {
		return cfg, nil, nil
	}
	run, err := review.NewAgentConfigRun(ctx)
	if err != nil {
		return cfg, nil, err //nolint:wrapcheck // already names the step
	}
	// Nothing is written for Codex; the run only resolves paths.
	defer run.Cleanup()
	if err := review.ValidateAgentConfig("codex", cfg.AgentConfig); err != nil {
		return cfg, nil, fmt.Errorf("review profile config: %w", err)
	}
	mainRoot, err := run.MainRepoRoot(ctx)
	if err != nil {
		return cfg, nil, err //nolint:wrapcheck // already names the step
	}
	roots := []string{run.CheckoutRoot}
	if mainRoot != run.CheckoutRoot {
		roots = append(roots, mainRoot)
	}
	cfg.ExtraArgs = append(cfg.ExtraArgs, "-c", untrustedProjectOverride(roots))
	if err := refuseUserMCPServerClash(cfg.AgentConfig.MCPServers); err != nil {
		return cfg, nil, err
	}
	servers, err := codexMCPOverrides(cfg.AgentConfig.MCPServers)
	if err != nil {
		return cfg, nil, err
	}
	cfg.ExtraArgs = append(cfg.ExtraArgs, servers...)
	// The untrusted checkout's hooks.json, Entire's own hooks included, isn't
	// loaded, so Entire's hooks are passed as session flags. Codex runs
	// session-flag hooks only with persisted trust or this bypass; it does
	// not load the checkout's hooks, which stay untrusted.
	cfg.ExtraArgs = append(cfg.ExtraArgs, entireHookOverrides(agent.UseWindowsProductionHooks(ctx))...)
	cfg.ExtraArgs = append(cfg.ExtraArgs, "--dangerously-bypass-hook-trust")
	cfg.WorkDir = run.CheckoutRoot
	return cfg, nil, nil
}

// untrustedProjectOverride marks roots untrusted for one codex run. It is one
// override: each -c projects=... replaces the whole table, so a second would
// drop the first.
func untrustedProjectOverride(roots []string) string {
	entries := make([]string, 0, len(roots))
	for _, root := range roots {
		quoted, err := json.Marshal(root)
		if err != nil {
			quoted = []byte(`""`)
		}
		entries = append(entries, string(quoted)+`={trust_level="untrusted"}`)
	}
	return "projects={" + strings.Join(entries, ",") + "}"
}

// codexMCPOverrides turns profile MCP servers into -c overrides. Values are
// JSON-encoded, which is valid TOML. Literal env values are refused: they
// would be visible in the process list.
func codexMCPOverrides(servers map[string]json.RawMessage) ([]string, error) {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	slices.Sort(names)
	var args []string
	for _, name := range names {
		var server struct {
			Command      string            `json:"command"`
			Args         []string          `json:"args"`
			URL          string            `json:"url"`
			Env          map[string]string `json:"env"`
			ApprovalMode string            `json:"default_tools_approval_mode"`
		}
		if err := json.Unmarshal(servers[name], &server); err != nil {
			return nil, fmt.Errorf("MCP server %q: %w", name, err)
		}
		if len(server.Env) > 0 {
			return nil, fmt.Errorf("MCP server %q: env values aren't supported for Codex in a review profile yet", name)
		}
		set := func(key string, value any) error {
			encoded, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("MCP server %q: %w", name, err)
			}
			args = append(args, "-c", "mcp_servers."+name+"."+key+"="+string(encoded))
			return nil
		}
		var err error
		if server.Command != "" {
			err = set("command", server.Command)
			if err == nil && len(server.Args) > 0 {
				err = set("args", server.Args)
			}
		} else {
			err = set("url", server.URL)
		}
		if err == nil && server.ApprovalMode != "" {
			err = set("default_tools_approval_mode", server.ApprovalMode)
		}
		if err != nil {
			return nil, err
		}
	}
	return args, nil
}

// refuseUserMCPServerClash refuses a profile MCP server whose name the user's
// Codex config.toml (CODEX_HOME or ~/.codex) already defines. Codex merges the
// -c session flags into that config table by table, so the server that ran
// would keep the user's env, headers, args or url alongside the profile's. A
// config that can't be read or parsed is refused too: Codex would not start
// on it, and a clash can't be ruled out. Only the user's config is checked;
// the reviewed checkout's project config is untrusted for the run.
func refuseUserMCPServerClash(servers map[string]json.RawMessage) error {
	if len(servers) == 0 {
		return nil
	}
	configPath := codexConfigPath()
	if configPath == "" {
		return errors.New("MCP servers in a Codex review profile: the Codex home can't be resolved to check config.toml for servers of the same name")
	}
	file, err := os.Open(configPath) //nolint:gosec // path resolved from CODEX_HOME or HOME
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("MCP servers in a Codex review profile: read %s: %w", configPath, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxHooksFileBytes+1))
	if err != nil {
		return fmt.Errorf("MCP servers in a Codex review profile: read %s: %w", configPath, err)
	}
	if len(data) > maxHooksFileBytes {
		return fmt.Errorf("MCP servers in a Codex review profile: %s is over %d bytes, too large to check for servers of the same name", configPath, maxHooksFileBytes)
	}
	var config struct {
		MCPServers map[string]any `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("MCP servers in a Codex review profile: parse %s: %w", configPath, err)
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if _, ok := config.MCPServers[name]; ok {
			return fmt.Errorf("MCP server %q is also defined in %s; Codex would merge the two, so rename it in the review profile", name, configPath)
		}
	}
	return nil
}

// entireHookOverrides returns -c overrides that register Entire's managed
// Codex hooks, the same commands `entire enable` writes to hooks.json.
func entireHookOverrides(windows bool) []string {
	var args []string
	for _, h := range managedHooks {
		command, err := json.Marshal(h.wrap("entire hooks codex "+h.verb, windows))
		if err != nil {
			continue
		}
		args = append(args, "-c", fmt.Sprintf(`hooks.%s=[{hooks=[{type="command",command=%s,timeout=%d}]}]`, h.event, command, h.timeout))
	}
	return args
}
