package claudecode

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/skilldiscovery"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// DiscoverReviewSkills walks Claude Code's on-disk plugin layout and user-
// level directories looking for review-adjacent invocations. Returns
// (nil, nil) when HOME is unreadable or directories are missing — discovery
// is best-effort.
//
// Claude Code exposes three kinds of invocable content per plugin, all invoked
// via the same slash-prefix syntax (`/name`, `/plugin:name`):
//   - skills:   <plugin>/skills/<name>/SKILL.md   (frontmatter: name + description)
//   - commands: <plugin>/commands/<name>.md       (frontmatter: description; name = filename)
//   - agents:   <plugin>/agents/<name>.md         (frontmatter: description; name = filename)
//
// All three are walked because any can be a review tool — the pr-review-toolkit
// plugin, for example, ships its review skills as commands/agents (not skills/).
//
// The generic SKILL.md / markdown scanning, version dedupe, and frontmatter
// parsing live in the shared skilldiscovery package; this method supplies the
// Claude-specific roots and slash invocation form.
//
//nolint:unparam // error return is part of SkillDiscoverer contract; future implementations may report hard failures
func (c *ClaudeCodeAgent) DiscoverReviewSkills(ctx context.Context) ([]agent.DiscoveredSkill, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		logging.Debug(ctx, "claude-code discovery: UserHomeDir failed", slog.String("error", err.Error()))
		return nil, nil
	}

	form := skilldiscovery.SlashForm
	var found []agent.DiscoveredSkill
	found = append(found, skilldiscovery.ScanPluginCache(ctx, filepath.Join(home, ".claude", "plugins", "cache"),
		func(versionRoot, pluginName string) []agent.DiscoveredSkill {
			var out []agent.DiscoveredSkill
			out = append(out, skilldiscovery.ScanSkillsDir(ctx, filepath.Join(versionRoot, "skills"), pluginName, form)...)
			out = append(out, skilldiscovery.ScanFlatMarkdownDir(ctx, filepath.Join(versionRoot, "commands"), pluginName, form)...)
			out = append(out, skilldiscovery.ScanFlatMarkdownDir(ctx, filepath.Join(versionRoot, "agents"), pluginName, form)...)
			return out
		})...)
	found = append(found, skilldiscovery.ScanSkillsDir(ctx, filepath.Join(home, ".claude", "skills"), "", form)...)
	found = append(found, skilldiscovery.ScanFlatMarkdownDir(ctx, filepath.Join(home, ".claude", "commands"), "", form)...)
	found = append(found, skilldiscovery.ScanFlatMarkdownDir(ctx, filepath.Join(home, ".claude", "agents"), "", form)...)
	found = skilldiscovery.DedupeByInvocation(found)
	if len(found) == 0 {
		return nil, nil
	}
	return found, nil
}
