package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// reviewPluginName namespaces the checkout's skills and commands, which are
// loaded as a plugin when a profile config replaces the checkout's settings.
const reviewPluginName = "project"

// prepareReviewAgentConfig applies a review profile's agent config: the
// checkout's settings and MCP servers are not loaded (--setting-sources user,
// --strict-mcp-config); the profile's settings, with Entire's own hooks added
// so the session is still tracked, and MCP servers are used instead. The
// checkout's skills and commands are loaded as the "project" plugin, copied
// from its committed tree.
func prepareReviewAgentConfig(ctx context.Context, cfg reviewtypes.RunConfig) (reviewtypes.RunConfig, func(), error) {
	if cfg.AgentConfig == nil {
		return cfg, nil, nil
	}
	run, err := review.NewAgentConfigRun(ctx)
	if err != nil {
		return cfg, nil, err //nolint:wrapcheck // already names the step
	}
	fail := func(err error) (reviewtypes.RunConfig, func(), error) {
		run.Cleanup()
		return cfg, nil, err
	}
	if err := review.ValidateAgentConfig("claude-code", cfg.AgentConfig, run.ForbiddenRoots); err != nil {
		return fail(fmt.Errorf("review profile config: %w", err))
	}
	settingsJSON, err := reviewProfileSettings(cfg.AgentConfig.Settings)
	if err != nil {
		return fail(err)
	}
	settingsPath, err := run.WriteFile("claude/settings.json", settingsJSON)
	if err != nil {
		return fail(err)
	}
	servers := cfg.AgentConfig.MCPServers
	if servers == nil {
		servers = map[string]json.RawMessage{}
	}
	mcpJSON, err := jsonutil.MarshalWithNoHTMLEscape(map[string]any{"mcpServers": servers})
	if err != nil {
		return fail(fmt.Errorf("encode MCP config: %w", err))
	}
	mcpPath, err := run.WriteFile("claude/mcp.json", mcpJSON)
	if err != nil {
		return fail(err)
	}
	cfg.ExtraArgs = append(cfg.ExtraArgs, flagSettingSources, "user", "--settings", settingsPath, "--strict-mcp-config", "--mcp-config", mcpPath)

	copied, err := run.CopyCheckoutTree(ctx, map[string]string{
		".claude/skills":   "claude/plugin/skills",
		".claude/commands": "claude/plugin/commands",
	})
	if err != nil {
		return fail(fmt.Errorf("load the checkout's skills: %w", err))
	}
	if copied > 0 {
		manifest := fmt.Sprintf(`{"name":%q,"version":"0.0.0","description":"Skills and commands from the reviewed checkout"}`, reviewPluginName)
		manifestPath, err := run.WriteFile("claude/plugin/.claude-plugin/plugin.json", []byte(manifest))
		if err != nil {
			return fail(err)
		}
		cfg.ExtraArgs = append(cfg.ExtraArgs, "--plugin-dir", filepath.Dir(filepath.Dir(manifestPath)))
		cfg.Skills = namespaceProjectSkills(ctx, run, cfg.Skills)
	}
	return cfg, run.Cleanup, nil
}

// reviewProfileSettings returns the profile's settings object with Entire's
// own hooks added, so the review session is tracked even though the
// checkout's settings (where Entire's hooks normally live) are not loaded.
func reviewProfileSettings(profile json.RawMessage) ([]byte, error) {
	obj := map[string]json.RawMessage{}
	if len(profile) > 0 {
		if err := json.Unmarshal(profile, &obj); err != nil {
			return nil, fmt.Errorf("review profile settings: %w", err)
		}
	}
	rawHooks := map[string]json.RawMessage{}
	if existing, ok := obj["hooks"]; ok {
		if err := json.Unmarshal(existing, &rawHooks); err != nil {
			return nil, fmt.Errorf("review profile settings hooks: %w", err)
		}
	}
	installHookEntries(rawHooks, false)
	hooks, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
	if err != nil {
		return nil, fmt.Errorf("encode hooks: %w", err)
	}
	obj["hooks"] = hooks
	out, err := jsonutil.MarshalWithNoHTMLEscape(obj)
	if err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	return out, nil
}

// namespaceProjectSkills rewrites "/name" to "/project:name" for skills and
// commands that came from the checkout, since plugin skills are invoked with
// the plugin's prefix.
func namespaceProjectSkills(ctx context.Context, run *review.AgentConfigRun, skills []string) []string {
	names := map[string]bool{}
	for _, dir := range []string{".claude/skills", ".claude/commands"} {
		for _, name := range run.TreeEntryNames(ctx, dir) {
			names[strings.TrimSuffix(name, ".md")] = true
		}
	}
	out := make([]string, 0, len(skills))
	for _, skill := range skills {
		if rest, ok := strings.CutPrefix(skill, "/"); ok && names[rest] {
			out = append(out, "/"+reviewPluginName+":"+rest)
			continue
		}
		out = append(out, skill)
	}
	return out
}
