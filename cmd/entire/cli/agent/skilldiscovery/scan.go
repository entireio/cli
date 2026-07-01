package skilldiscovery

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// ScanSkillsDir reads each skill subdirectory's SKILL.md under dir, parses its
// YAML frontmatter, and emits a DiscoveredSkill for every entry whose
// invocation name is review-adjacent per Matches. It is shared by all agents
// whose on-disk skill layout is <dir>/<name>/SKILL.md (Claude Code plugin/user
// skills, Antigravity global/shared/project skills).
//
// Contract (mirrors SkillDiscoverer): a missing/unreadable dir returns nil,
// not an error; malformed individual SKILL.md files are skipped with a Debug
// log. pluginName, when non-empty, produces "/plugin:name" invocations.
func ScanSkillsDir(ctx context.Context, dir, pluginName string) []agent.DiscoveredSkill {
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
		data, err := os.ReadFile(skillFile) //nolint:gosec // G304: skillFile is constructed from a ReadDir walk, not user input
		if err != nil {
			continue
		}
		name, description, parseErr := ParseFrontmatter(data)
		if parseErr != nil {
			logging.Debug(ctx, "skill discovery: skipping malformed SKILL.md",
				slog.String("path", skillFile), slog.String("error", parseErr.Error()))
			continue
		}
		if name == "" {
			name = skillEntry.Name()
		}
		invocation := InvocationName(name, pluginName)
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

// Dedupe collapses entries sharing an invocation Name, keeping the first
// occurrence. Callers order their scans by precedence (e.g. global before
// shared) so the preferred source wins.
func Dedupe(in []agent.DiscoveredSkill) []agent.DiscoveredSkill {
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

// InvocationName builds the slash-prefixed invocation form. Plugin-prefixed
// names use "/plugin:name"; bare names use "/name".
func InvocationName(name, pluginName string) string {
	if pluginName == "" {
		return "/" + name
	}
	return "/" + pluginName + ":" + name
}

// ParseFrontmatter extracts `name:` and `description:` from a minimal YAML
// frontmatter block. Purpose-built for the tiny subset of YAML these SKILL.md /
// command / agent files actually use. Surrounding double-quotes are trimmed so
// `description: "foo bar"` returns `foo bar`.
func ParseFrontmatter(data []byte) (name, description string, err error) {
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
