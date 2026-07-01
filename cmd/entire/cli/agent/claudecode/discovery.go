package claudecode

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/skilldiscovery"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// DiscoverReviewSkills walks Claude Code's on-disk plugin layout and user-
// level directories looking for review-adjacent invocations. Returns
// (nil, nil) when HOME is unreadable or directories are missing — discovery
// is best-effort.
//
// Claude Code exposes three kinds of invocable content per plugin:
//   - skills:   <plugin>/skills/<name>/SKILL.md   (YAML frontmatter with name + description)
//   - commands: <plugin>/commands/<name>.md       (YAML frontmatter with description; name = filename)
//   - agents:   <plugin>/agents/<name>.md         (YAML frontmatter with description; name = filename)
//
// All three are walked because users invoke them via the same slash-prefix
// syntax (`/plugin:name`) and any of them can be a review tool. The
// pr-review-toolkit plugin, for example, ships its review skills as
// commands/agents (not skills/), and was silently missed by a skills-only
// walker.
//
//nolint:unparam // error return is part of SkillDiscoverer contract; future implementations may report hard failures
func (c *ClaudeCodeAgent) DiscoverReviewSkills(ctx context.Context) ([]agent.DiscoveredSkill, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		logging.Debug(ctx, "claude-code discovery: UserHomeDir failed", slog.String("error", err.Error()))
		return nil, nil
	}

	var found []agent.DiscoveredSkill
	found = append(found, scanPluginCache(ctx, filepath.Join(home, ".claude", "plugins", "cache"))...)
	found = append(found, skilldiscovery.ScanSkillsDir(ctx, filepath.Join(home, ".claude", "skills"), "")...)
	found = append(found, scanFlatMarkdownDir(ctx, filepath.Join(home, ".claude", "commands"), "")...)
	found = append(found, scanFlatMarkdownDir(ctx, filepath.Join(home, ".claude", "agents"), "")...)
	found = skilldiscovery.Dedupe(found)
	if len(found) == 0 {
		return nil, nil
	}
	return found, nil
}

// scanPluginCache walks <root>/<marketplace>/<plugin>/<version>/{skills,commands,agents}/
// One plugin can contribute through any or all three directories.
//
// Multiple version directories per plugin are common after upgrades. Walking
// every version produces duplicate skills (same invocation name, same
// description) — confusing in the picker and wasteful in the prompt. We pick
// a single version per plugin via pickLatestVersion: prefer valid semver
// (highest), fall back to lexicographic max.
func scanPluginCache(ctx context.Context, root string) []agent.DiscoveredSkill {
	entries, err := os.ReadDir(root)
	if err != nil {
		logging.Debug(ctx, "claude-code discovery: plugin cache unreadable",
			slog.String("root", root), slog.String("error", err.Error()))
		return nil
	}
	var found []agent.DiscoveredSkill
	for _, marketEntry := range entries {
		if !marketEntry.IsDir() {
			continue
		}
		marketRoot := filepath.Join(root, marketEntry.Name())
		pluginEntries, err := os.ReadDir(marketRoot)
		if err != nil {
			continue
		}
		for _, pluginEntry := range pluginEntries {
			if !pluginEntry.IsDir() {
				continue
			}
			pluginName := pluginEntry.Name()
			pluginRoot := filepath.Join(marketRoot, pluginName)
			versionEntries, err := os.ReadDir(pluginRoot)
			if err != nil {
				continue
			}
			versionDir, ok := pickLatestVersion(versionEntries)
			if !ok {
				continue
			}
			versionRoot := filepath.Join(pluginRoot, versionDir)
			found = append(found, skilldiscovery.ScanSkillsDir(ctx, filepath.Join(versionRoot, "skills"), pluginName)...)
			found = append(found, scanFlatMarkdownDir(ctx, filepath.Join(versionRoot, "commands"), pluginName)...)
			found = append(found, scanFlatMarkdownDir(ctx, filepath.Join(versionRoot, "agents"), pluginName)...)
		}
	}
	return found
}

// pickLatestVersion returns the name of the "newest" version directory among
// entries. Strategy:
//
//   - If any entry name parses as semver (with or without a leading "v"), pick
//     the highest semver among those that parse. Non-semver entries are
//     ignored when at least one semver entry exists.
//   - Otherwise, fall back to the lexicographic max of all directory names.
//     This handles the "unknown" sentinel some plugins ship and one-off names.
//
// Returns ("", false) if no usable directory entry exists.
func pickLatestVersion(entries []os.DirEntry) (string, bool) {
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 0 {
		return "", false
	}
	var semverDirs []string
	for _, d := range dirs {
		if semver.IsValid(semverWithV(d)) {
			semverDirs = append(semverDirs, d)
		}
	}
	if len(semverDirs) > 0 {
		sort.Slice(semverDirs, func(i, j int) bool {
			return semver.Compare(semverWithV(semverDirs[i]), semverWithV(semverDirs[j])) > 0
		})
		return semverDirs[0], true
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	return dirs[0], true
}

// semverWithV ensures a version string has the "v" prefix that
// golang.org/x/mod/semver requires. Plugin version dirs are usually bare
// (e.g. "0.1.0"), but we tolerate either form.
func semverWithV(s string) string {
	if strings.HasPrefix(s, "v") {
		return s
	}
	return "v" + s
}

// scanFlatMarkdownDir reads *.md files directly under dir (no nesting), parses
// their YAML frontmatter for `description:`, and derives the invocation name
// from the filename (stripping the .md suffix). Used for both plugin
// commands/agents and user-level ~/.claude/commands and ~/.claude/agents.
//
// Frontmatter shape differs from SKILL.md — no `name:` field, so the
// filename is the source of truth for the invocation name.
func scanFlatMarkdownDir(ctx context.Context, dir, pluginName string) []agent.DiscoveredSkill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var found []agent.DiscoveredSkill
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		baseName := strings.TrimSuffix(entry.Name(), ".md")
		if strings.EqualFold(baseName, "README") {
			continue
		}
		filePath := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(filePath) //nolint:gosec // G304: filePath is constructed from a ReadDir walk under HOME, not user input
		if err != nil {
			continue
		}
		_, description, parseErr := skilldiscovery.ParseFrontmatter(data)
		if parseErr != nil {
			logging.Debug(ctx, "claude-code discovery: skipping malformed command/agent",
				slog.String("path", filePath), slog.String("error", parseErr.Error()))
			continue
		}
		invocation := skilldiscovery.InvocationName(baseName, pluginName)
		if !skilldiscovery.Matches(invocation, description) {
			continue
		}
		found = append(found, agent.DiscoveredSkill{
			Name:        invocation,
			Description: description,
			SourcePath:  filePath,
		})
	}
	return found
}
