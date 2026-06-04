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
