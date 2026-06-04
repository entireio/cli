package codex

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/skilldiscovery"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// DiscoverReviewSkills walks codex's on-disk skill layout looking for
// review-adjacent skills. Returns (nil, nil) when HOME is unreadable or the
// directories are missing — discovery is best-effort.
//
// Codex exposes skills as <root>/<name>/SKILL.md (same frontmatter shape as
// Claude). Three roots contribute, mirroring codex's own injected skills
// catalog:
//   - ~/.codex/skills/<name>/                          → user skills ($name)
//   - ~/.codex/plugins/cache/<m>/<p>/<v>/skills/<name>/ → plugin skills ($p:name)
//   - ~/.codex/superpowers/skills/<name>/              → superpowers ($superpowers:name)
//
// Skills are emitted in codex's dollar invocation form ($name / $plugin:name) —
// the literal token a user types to invoke the skill in the codex CLI — so the
// review prompt names skills exactly the way codex's skill system expects,
// loading the real SKILL.md rather than relying on a loose description match.
//
//nolint:unparam // error return is part of SkillDiscoverer contract; future implementations may report hard failures
func (c *CodexAgent) DiscoverReviewSkills(ctx context.Context) ([]agent.DiscoveredSkill, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		logging.Debug(ctx, "codex discovery: UserHomeDir failed", slog.String("error", err.Error()))
		return nil, nil
	}
	codexHome := filepath.Join(home, ".codex")

	form := skilldiscovery.DollarForm
	var found []agent.DiscoveredSkill
	found = append(found, skilldiscovery.ScanSkillsDir(ctx, filepath.Join(codexHome, "skills"), "", form)...)
	found = append(found, skilldiscovery.ScanPluginCache(ctx, filepath.Join(codexHome, "plugins", "cache"),
		func(versionRoot, pluginName string) []agent.DiscoveredSkill {
			return skilldiscovery.ScanSkillsDir(ctx, filepath.Join(versionRoot, "skills"), pluginName, form)
		})...)
	found = append(found, skilldiscovery.ScanSkillsDir(ctx, filepath.Join(codexHome, "superpowers", "skills"), "superpowers", form)...)
	found = skilldiscovery.DedupeByInvocation(found)
	if len(found) == 0 {
		return nil, nil
	}
	return found, nil
}
