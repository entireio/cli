package codex_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
)

// Compile-time pin: CodexAgent must satisfy SkillDiscoverer.
var _ agent.SkillDiscoverer = (*codex.CodexAgent)(nil)

// withFakeHome points HOME at a temp dir so discovery walks an empty,
// controlled ~/.codex tree. Uses t.Setenv, so callers must NOT t.Parallel.
func withFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "") // hermetic: a dev shell's CODEX_HOME must not leak in
	return home
}

// writeSkill creates <root>/<name>/SKILL.md with the given frontmatter name
// and description.
func writeSkill(t *testing.T, root, dir, name, description string) {
	t.Helper()
	skillDir := filepath.Join(root, dir)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func discover(t *testing.T) []agent.DiscoveredSkill {
	t.Helper()
	skills, err := (&codex.CodexAgent{}).DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return skills
}

func nameOf(skills []agent.DiscoveredSkill, want string) bool {
	for _, s := range skills {
		if s.Name == want {
			return true
		}
	}
	return false
}

func TestCodexAgent_DiscoverReviewSkills_NoSkillsReturnsNilNil(t *testing.T) {
	// Cannot t.Parallel — uses t.Setenv.
	withFakeHome(t)
	if skills := discover(t); skills != nil {
		t.Errorf("skills = %v, want nil", skills)
	}
}

func TestCodexAgent_DiscoverReviewSkills_FindsUserSkillInDollarForm(t *testing.T) {
	home := withFakeHome(t)
	writeSkill(t, filepath.Join(home, ".codex", "skills"), "code-reviewer", "code-reviewer",
		"Review code changes with an emphasis on correctness.")

	skills := discover(t)
	if len(skills) != 1 {
		t.Fatalf("skills count = %d, want 1: %+v", len(skills), skills)
	}
	if skills[0].Name != "$code-reviewer" {
		t.Errorf("Name = %q, want $code-reviewer", skills[0].Name)
	}
}

func TestCodexAgent_DiscoverReviewSkills_FindsPluginSkillNamespaced(t *testing.T) {
	home := withFakeHome(t)
	// Opaque (non-semver) version dir, like codex's content-hash versions.
	writeSkill(t,
		filepath.Join(home, ".codex", "plugins", "cache", "openai-curated", "github", "fef63ecf", "skills"),
		"gh-review", "gh-review", "Review a GitHub pull request.")

	skills := discover(t)
	if !nameOf(skills, "$github:gh-review") {
		t.Errorf("missing $github:gh-review; got %+v", skills)
	}
}

func TestCodexAgent_DiscoverReviewSkills_FindsSuperpowersSkill(t *testing.T) {
	home := withFakeHome(t)
	writeSkill(t, filepath.Join(home, ".codex", "superpowers", "skills"),
		"receiving-code-review", "receiving-code-review", "Receive code review feedback.")

	skills := discover(t)
	if !nameOf(skills, "$superpowers:receiving-code-review") {
		t.Errorf("missing $superpowers:receiving-code-review; got %+v", skills)
	}
}

func TestCodexAgent_DiscoverReviewSkills_SkipsNonReviewSkill(t *testing.T) {
	home := withFakeHome(t)
	skillsRoot := filepath.Join(home, ".codex", "skills")
	writeSkill(t, skillsRoot, "code-reviewer", "code-reviewer", "Review code changes.")
	// "committer" has no review keyword in its name → filtered by Matches.
	writeSkill(t, skillsRoot, "committer", "committer", "Prepare clear commit messages.")

	skills := discover(t)
	if len(skills) != 1 || skills[0].Name != "$code-reviewer" {
		t.Errorf("want only $code-reviewer; got %+v", skills)
	}
}

// TestCodexAgent_DiscoverReviewSkills_HonorsCodexHome pins discovery to the
// agent's canonical home resolution: the rest of the codex agent resolves its
// config tree through resolveCodexHome (which honors CODEX_HOME), so skills
// installed under a custom codex home must be discoverable too — otherwise
// saved $skills fail spawn-time validation as "not installed" even though
// codex itself finds and runs them.
func TestCodexAgent_DiscoverReviewSkills_HonorsCodexHome(t *testing.T) {
	// Cannot t.Parallel — uses t.Setenv.
	withFakeHome(t) // HOME points at an empty dir; the skill lives elsewhere
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	writeSkill(t, codexHome, "skills/code-review", "code-review", "Reviews code.")

	if !nameOf(discover(t), "$code-review") {
		t.Fatal("skill under CODEX_HOME not discovered — discovery must use resolveCodexHome, not ~/.codex")
	}
}
