package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

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
	if err := review.ValidateAgentConfig("codex", cfg.AgentConfig, run.ForbiddenRoots); err != nil {
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
	for _, root := range roots {
		cfg.ExtraArgs = append(cfg.ExtraArgs, "-c", untrustedProjectOverride(root))
	}
	servers, err := codexMCPOverrides(cfg.AgentConfig.MCPServers)
	if err != nil {
		return cfg, nil, err
	}
	cfg.ExtraArgs = append(cfg.ExtraArgs, servers...)
	cfg.WorkDir = run.CheckoutRoot
	return cfg, nil, nil
}

// untrustedProjectOverride marks root untrusted for one codex run.
func untrustedProjectOverride(root string) string {
	quoted, err := json.Marshal(root)
	if err != nil {
		quoted = []byte(`""`)
	}
	return "projects={" + string(quoted) + `={trust_level="untrusted"}}`
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
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			URL     string            `json:"url"`
			Env     map[string]string `json:"env"`
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
		if err != nil {
			return nil, err
		}
	}
	return args, nil
}
