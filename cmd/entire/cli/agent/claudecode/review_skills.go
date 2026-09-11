package claudecode

// review_skills.go stages the review profile's configured skills so they remain
// available to a reviewer that loads no settings.
//
// The tension this resolves: suppressing the reviewed checkout's configuration
// means suppressing Claude's settings sources, and skills resolve through those
// sources. Keeping user settings instead would keep skills working, but would
// also run the user's own hooks with the reviewed checkout as the working
// directory — a hook invoking `npm run …`, `make …`, or any checkout-relative
// script then executes code from the branch under review. That is the same
// class of problem this isolation exists to close, so it is not an acceptable
// trade.
//
// Instead Entire copies exactly the skills the profile names into a plugin
// directory it owns, and points Claude at that with --plugin-dir. The user's
// choice of skill is honoured, and nothing else from their configuration is
// loaded: not hooks, not permission modes, not unrelated plugin content. Only
// the chosen skill is copied, even when it came from a larger plugin, because
// loading that plugin wholesale would reintroduce whatever hooks it defines.
//
// Invocations are rewritten to match, since a plugin-provided command is
// addressed as /<plugin>:<name>.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/skilldiscovery"
)

// stagedPluginName is the plugin the staged skills are published under. It
// appears in the rewritten invocation, so it is stable and Entire-owned.
const stagedPluginName = "entire-review"

// skillKindDir maps a discovered skill's on-disk parent directory to the
// layout position it must occupy in the staged plugin. Claude resolves each
// kind from its own directory, so a command copied into skills/ would not
// resolve.
var skillKindDirs = map[string]bool{"commands": true, "skills": true, "agents": true}

// stagedSkills is the result of staging: the directory to hand to
// --plugin-dir, and the invocation rewrites to apply to the prompt.
type stagedSkills struct {
	pluginDir string
	// rewrites maps the profile's configured invocation to the one that
	// addresses the staged copy, e.g. "/pr-review-toolkit:review-pr" ->
	// "/entire-review:pr-review-toolkit-review-pr".
	rewrites map[string]string
}

// apply returns skills with every staged invocation replaced. Unstaged entries
// pass through unchanged so a caller cannot silently drop one.
func (s stagedSkills) apply(skills []string) []string {
	if len(skills) == 0 {
		return skills
	}
	out := make([]string, 0, len(skills))
	for _, skill := range skills {
		if rewritten, ok := s.rewrites[skill]; ok {
			out = append(out, rewritten)
			continue
		}
		out = append(out, skill)
	}
	return out
}

// errReviewSkillUnavailable reports that a configured review skill could not be
// made available to an isolated reviewer. The review fails rather than running
// without the skill the user chose: a reviewer launched without its profile's
// skill does not review less thoroughly, it reports "Unknown command" and
// reviews nothing while still exiting successfully.
var errReviewSkillUnavailable = errors.New("configured review skill is unavailable")

// stageReviewSkills copies each configured skill into a temporary plugin
// directory owned by Entire and returns it with the invocation rewrites.
//
// Returns a zero stagedSkills and a nil cleanup when the profile configures no
// skills, which is the prompt-driven case (e.g. Pi-style profiles).
func stageReviewSkills(ctx context.Context, skills []string) (stagedSkills, func(), error) {
	// Curated builtins (e.g. /review) are Claude's own and resolve without any
	// settings source — Task 0 proved that under "". They have no on-disk
	// source to copy, so they pass through untouched; the picker exempts them
	// from discovery for the same reason (builtinNameSet in review/picker.go).
	builtins := map[string]struct{}{}
	for _, b := range skilldiscovery.CuratedBuiltinsFor(string(agent.AgentNameClaudeCode)) {
		builtins[b.Name] = struct{}{}
	}
	var toStage []string
	for _, skill := range skills {
		if _, ok := builtins[skill]; !ok {
			toStage = append(toStage, skill)
		}
	}
	if len(toStage) == 0 {
		return stagedSkills{}, nil, nil
	}

	sources, err := discoveredSkillSources(ctx)
	if err != nil {
		return stagedSkills{}, nil, err
	}

	root, err := os.MkdirTemp("", "entire-review-skills-*")
	if err != nil {
		return stagedSkills{}, nil, fmt.Errorf("create staged skill directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }

	if err := writePluginManifest(root); err != nil {
		cleanup()
		return stagedSkills{}, nil, err
	}

	staged := stagedSkills{pluginDir: root, rewrites: map[string]string{}}
	used := map[string]bool{}
	for _, skill := range toStage {
		source, ok := sources[skill]
		if !ok {
			cleanup()
			return stagedSkills{}, nil, fmt.Errorf("%w: %s is not installed", errReviewSkillUnavailable, skill)
		}
		base, err := stagedBaseName(skill)
		if err != nil {
			cleanup()
			return stagedSkills{}, nil, err
		}
		// Two configured invocations can flatten to the same base — e.g. "/a:b"
		// and "/a-b" both become "a-b". Give each its own name so the second
		// does not overwrite the first's staged file (and the reviewer does not
		// silently load the wrong skill). The loop also covers a natural base
		// that collides with a disambiguated one.
		unique := base
		for i := 2; used[unique]; i++ {
			unique = base + "-" + strconv.Itoa(i)
		}
		used[unique] = true

		invocation, err := stageOneSkill(root, skill, source, unique)
		if err != nil {
			cleanup()
			return stagedSkills{}, nil, err
		}
		staged.rewrites[skill] = invocation
	}
	return staged, cleanup, nil
}

// discoveredSkillSources maps each discovered invocation to its on-disk path.
func discoveredSkillSources(ctx context.Context) (map[string]string, error) {
	discovered, err := (&ClaudeCodeAgent{}).DiscoverReviewSkills(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: skill discovery failed: %w", errReviewSkillUnavailable, err)
	}
	sources := make(map[string]string, len(discovered))
	for _, skill := range discovered {
		if skill.SourcePath != "" {
			sources[skill.Name] = skill.SourcePath
		}
	}
	return sources, nil
}

// stageOneSkill copies one skill into the staged plugin and returns the
// invocation that addresses the copy.
//
// base is the unique, validated file-name segment the caller allocated for this
// invocation (see stageReviewSkills); stageOneSkill only copies the skill under
// it. Two invocations that flatten to the same base get distinct base values
// from the caller, so staged files never overwrite one another.
func stageOneSkill(root, invocation, sourcePath, base string) (string, error) {
	kind := filepath.Base(filepath.Dir(sourcePath))

	// A skills/ entry is a directory holding SKILL.md; commands/ and agents/
	// are flat markdown. Anything else is a layout this code has not seen.
	if kind == "SKILL.md" || strings.EqualFold(filepath.Base(sourcePath), "SKILL.md") {
		dir := filepath.Join(root, "skills", base)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("stage skill %s: %w", invocation, err)
		}
		if err := copyFile(sourcePath, filepath.Join(dir, "SKILL.md")); err != nil {
			return "", fmt.Errorf("stage skill %s: %w", invocation, err)
		}
		return "/" + stagedPluginName + ":" + base, nil
	}
	if !skillKindDirs[kind] {
		return "", fmt.Errorf("%w: %s has an unrecognised layout (%s)", errReviewSkillUnavailable, invocation, sourcePath)
	}
	dir := filepath.Join(root, kind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("stage skill %s: %w", invocation, err)
	}
	if err := copyFile(sourcePath, filepath.Join(dir, base+".md")); err != nil {
		return "", fmt.Errorf("stage skill %s: %w", invocation, err)
	}
	return "/" + stagedPluginName + ":" + base, nil
}

// stagedBaseName flattens an invocation into a single filename-safe path
// segment. The invocation has already matched a discovered skill, so a
// well-formed one is a plain name; anything that could still escape the
// staged directory (dot segments, separators) is refused rather than
// normalised, because the result becomes a write path under root.
func stagedBaseName(invocation string) (string, error) {
	base := strings.TrimPrefix(invocation, "/")
	base = strings.ReplaceAll(base, ":", "-")
	if base == "" || base == "." || base == ".." ||
		strings.ContainsAny(base, `/\`) || strings.Contains(base, "..") {
		return "", fmt.Errorf("%w: %s is not a plain skill name", errReviewSkillUnavailable, invocation)
	}
	return base, nil
}

// writePluginManifest writes the manifest that makes root loadable as a plugin.
func writePluginManifest(root string) error {
	dir := filepath.Join(root, ".claude-plugin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create staged plugin manifest dir: %w", err)
	}
	manifest := `{"name":"` + stagedPluginName + `","version":"1.0.0","description":"Review skills staged by Entire"}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		return fmt.Errorf("write staged plugin manifest: %w", err)
	}
	return nil
}

// copyFile copies a single regular file, 0600.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src) //nolint:gosec // src comes from Entire's own skill discovery under the user's config dir
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	// dst is root/<kind>/<base>[.md] where root is an Entire-created temp dir
	// and base was validated by stagedBaseName to be a single plain segment.
	if err := os.WriteFile(dst, data, 0o600); err != nil { //nolint:gosec // path is constructed under an Entire-owned MkdirTemp root from a validated segment
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}
