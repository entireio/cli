package antigravity

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/skilldiscovery"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// DiscoverReviewSkills walks Antigravity's three user-skill scopes looking for
// review-adjacent skills. agy stores skills as <scope>/<name>/SKILL.md with
// YAML frontmatter (name + description); the model activates them by
// description, and they are referenced with a "/name" slash invocation.
//
// Scopes, in precedence order (global wins on name collision):
//   - global (agy 1.1+):           ~/.gemini/config/skills
//   - global (pre-1.1 layouts):    ~/.gemini/antigravity-cli/skills, ~/.gemini/skills
//   - workspace (agy 1.1+):        <repo>/.agents/skills
//   - workspace (legacy, honored): <repo>/.agent/skills
//
// agy 1.1 moved the defaults ("Antigravity now defaults to .agents/skills,
// but still maintains backward support for .agent/skills"; global discovery
// under ~/.gemini/config/) — all roots are scanned so skills keep resolving
// across agy versions.
//
// Google's built-in skills under ~/.gemini/antigravity-cli/builtin/skills are
// deliberately NOT scanned — they are shipped guides (e.g. antigravity-guide),
// not user review tools.
//
// Best-effort per the SkillDiscoverer contract: unreadable HOME or missing
// directories yield (nil, nil); malformed SKILL.md files are skipped by the
// shared scanner with a Debug log.
//
//nolint:unparam // error return is part of the SkillDiscoverer contract
func (a *AntigravityAgent) DiscoverReviewSkills(ctx context.Context) ([]agent.DiscoveredSkill, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		logging.Debug(ctx, "antigravity discovery: UserHomeDir failed", slog.String("error", err.Error()))
		return nil, nil
	}

	var found []agent.DiscoveredSkill
	found = append(found, skilldiscovery.ScanSkillsDir(ctx, filepath.Join(home, ".gemini", "config", "skills"), "")...)
	found = append(found, skilldiscovery.ScanSkillsDir(ctx, filepath.Join(home, ".gemini", "antigravity-cli", "skills"), "")...)
	found = append(found, skilldiscovery.ScanSkillsDir(ctx, filepath.Join(home, ".gemini", "skills"), "")...)
	if root, rootErr := paths.WorktreeRoot(ctx); rootErr == nil {
		found = append(found, skilldiscovery.ScanSkillsDir(ctx, filepath.Join(root, ".agents", "skills"), "")...)
		found = append(found, skilldiscovery.ScanSkillsDir(ctx, filepath.Join(root, ".agent", "skills"), "")...)
	}

	found = skilldiscovery.Dedupe(found)
	if len(found) == 0 {
		return nil, nil
	}
	return found, nil
}
