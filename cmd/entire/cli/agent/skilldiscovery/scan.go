package skilldiscovery

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// InvocationForm builds an agent's invocation string for a discovered skill.
// The only thing that differs between agents is the prefix and namespace
// joiner: Claude Code uses slash form (`/name`, `/plugin:name`), codex uses
// dollar form (`$name`, `$plugin:name`) — the literal token a user types to
// invoke the skill in that CLI. Discovery emits Name already in this form so
// downstream prompt composition stays agent-agnostic and joins verbatim.
type InvocationForm func(name, pluginName string) string

// SlashForm is Claude Code's invocation syntax: "/name" or "/plugin:name".
func SlashForm(name, pluginName string) string {
	if pluginName == "" {
		return "/" + name
	}
	return "/" + pluginName + ":" + name
}

// DollarForm is codex's invocation syntax: "$name" or "$plugin:name". This is
// the explicit "use this skill" token from codex's own injected skills
// catalog ("name a skill with $SkillName or plain text").
func DollarForm(name, pluginName string) string {
	if pluginName == "" {
		return "$" + name
	}
	return "$" + pluginName + ":" + name
}

// DedupeByInvocation collapses entries sharing an invocation name, keeping the
// first occurrence. Plugins can ship a skill and a same-named wrapper that
// forwards to it; scan order decides which wins.
func DedupeByInvocation(in []agent.DiscoveredSkill) []agent.DiscoveredSkill {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]agent.DiscoveredSkill, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s.Name]; dup {
			continue
		}
		seen[s.Name] = struct{}{}
		out = append(out, s)
	}
	return out
}

// ScanPluginCache walks <root>/<marketplace>/<plugin>/<version>/ and invokes
// scanVersion once per plugin, for the single version directory chosen by
// PickLatestVersion. The callback receives the chosen version root and the
// plugin name (used as the invocation namespace). Both Claude Code and codex
// use this same market/plugin/version cache layout; they differ only in which
// subdirectories under the version root they scan and their invocation form.
func ScanPluginCache(ctx context.Context, root string, scanVersion func(versionRoot, pluginName string) []agent.DiscoveredSkill) []agent.DiscoveredSkill {
	entries, err := os.ReadDir(root)
	if err != nil {
		logging.Debug(ctx, "skill discovery: plugin cache unreadable",
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
			versionDir, ok := PickLatestVersion(versionEntries)
			if !ok {
				continue
			}
			found = append(found, scanVersion(filepath.Join(pluginRoot, versionDir), pluginName)...)
		}
	}
	return found
}

// PickLatestVersion returns the "newest" version directory name among entries:
//
//   - If any entry parses as semver (with or without a leading "v"), pick the
//     highest semver; non-semver entries are ignored when a semver exists.
//   - Otherwise fall back to the lexicographic max of all directory names.
//     This handles the "unknown" sentinel some plugins ship, and the opaque
//     content-hash version dirs codex plugins use (e.g. "fef63ecf").
//
// Returns ("", false) if no usable directory entry exists.
func PickLatestVersion(entries []os.DirEntry) (string, bool) {
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

// semverWithV ensures a version string has the "v" prefix golang.org/x/mod/semver
// requires. Plugin version dirs are usually bare (e.g. "0.1.0").
func semverWithV(s string) string {
	if strings.HasPrefix(s, "v") {
		return s
	}
	return "v" + s
}

// ScanSkillsDir reads each <dir>/<name>/SKILL.md, parses its frontmatter, and
// emits a DiscoveredSkill (in invoke's form) when Matches() returns true.
// pluginName is the namespace ("" for un-namespaced user skills). Missing dirs
// yield nil — discovery is best-effort.
func ScanSkillsDir(ctx context.Context, dir, pluginName string, invoke InvocationForm) []agent.DiscoveredSkill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var found []agent.DiscoveredSkill
	for _, skillEntry := range entries {
		if !skillEntry.IsDir() {
			continue
		}
		skillFile := filepath.Join(dir, skillEntry.Name(), "SKILL.md")
		data, err := os.ReadFile(skillFile) //nolint:gosec // G304: skillFile is built from a ReadDir walk under HOME, not user input
		if err != nil {
			continue
		}
		name, description, parseErr := ParseSkillFrontmatter(data)
		if parseErr != nil {
			logging.Debug(ctx, "skill discovery: skipping malformed SKILL.md",
				slog.String("path", skillFile), slog.String("error", parseErr.Error()))
			continue
		}
		if name == "" {
			name = skillEntry.Name()
		}
		invocation := invoke(name, pluginName)
		if !Matches(invocation, description) {
			continue
		}
		found = append(found, agent.DiscoveredSkill{
			Name:        invocation,
			Description: description,
			SourcePath:  skillFile,
		})
	}
	return found
}

// ScanFlatMarkdownDir reads *.md files directly under dir (no nesting), parses
// their frontmatter for `description:`, and derives the invocation name from
// the filename (minus .md). Used by Claude Code for plugin/user commands and
// agents, whose frontmatter has no `name:` field. README.md is skipped.
func ScanFlatMarkdownDir(ctx context.Context, dir, pluginName string, invoke InvocationForm) []agent.DiscoveredSkill {
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
		data, err := os.ReadFile(filePath) //nolint:gosec // G304: filePath is built from a ReadDir walk under HOME, not user input
		if err != nil {
			continue
		}
		_, description, parseErr := ParseSkillFrontmatter(data)
		if parseErr != nil {
			logging.Debug(ctx, "skill discovery: skipping malformed command/agent",
				slog.String("path", filePath), slog.String("error", parseErr.Error()))
			continue
		}
		invocation := invoke(baseName, pluginName)
		if !Matches(invocation, description) {
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

// ParseSkillFrontmatter extracts `name:` and `description:` from a minimal YAML
// frontmatter block — the tiny subset these SKILL.md / command / agent files
// use. Surrounding double-quotes are trimmed so `description: "foo"` returns
// `foo`.
func ParseSkillFrontmatter(data []byte) (name, description string, err error) {
	s := string(data)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return "", "", errors.New("no frontmatter delimiter")
	}
	body := strings.TrimPrefix(strings.TrimPrefix(s, "---\r\n"), "---\n")
	end := strings.Index(body, "\n---")
	if end < 0 {
		return "", "", errors.New("no closing frontmatter delimiter")
	}
	for _, line := range strings.Split(body[:end], "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "name:"):
			name = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "name:")), `"`)
		case strings.HasPrefix(line, "description:"):
			description = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "description:")), `"`)
		}
	}
	return name, description, nil
}
