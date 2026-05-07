package claudecode_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
)

// Compile-time pin: ClaudeCodeAgent must satisfy SkillDiscoverer.
var _ agent.SkillDiscoverer = (*claudecode.ClaudeCodeAgent)(nil)

func withFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestDiscoverReviewSkills_NoPluginsDirReturnsNilNil(t *testing.T) {
	// Cannot t.Parallel — uses t.Setenv.
	withFakeHome(t)

	a := &claudecode.ClaudeCodeAgent{}
	skills, err := a.DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skills != nil {
		t.Errorf("skills = %v, want nil", skills)
	}
}

func TestDiscoverReviewSkills_FindsPluginReviewSkill(t *testing.T) {
	home := withFakeHome(t)
	skillDir := filepath.Join(home, ".claude", "plugins", "cache",
		"fake-market", "pr-review-toolkit", "0.1.0", "skills", "review-pr")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `---
name: review-pr
description: Full PR review
---

Review the PR.
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &claudecode.ClaudeCodeAgent{}
	skills, err := a.DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("skills count = %d, want 1", len(skills))
	}
	if skills[0].Name != "/pr-review-toolkit:review-pr" {
		t.Errorf("skills[0].Name = %q, want /pr-review-toolkit:review-pr", skills[0].Name)
	}
	if skills[0].Description != "Full PR review" {
		t.Errorf("skills[0].Description = %q", skills[0].Description)
	}
}

func TestDiscoverReviewSkills_SkipsNonReviewSkill(t *testing.T) {
	home := withFakeHome(t)
	skillDir := filepath.Join(home, ".claude", "plugins", "cache",
		"fake-market", "unrelated-plugin", "1.0.0", "skills", "format-code")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `---
name: format-code
description: Reformat code according to project style
---
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &claudecode.ClaudeCodeAgent{}
	skills, err := a.DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skills) != 0 {
		t.Errorf("non-review skill should be skipped, got %+v", skills)
	}
}

func TestDiscoverReviewSkills_MalformedSkillSkipped(t *testing.T) {
	home := withFakeHome(t)
	goodDir := filepath.Join(home, ".claude", "plugins", "cache",
		"fake-market", "good-plugin", "1.0.0", "skills", "review-pr")
	badDir := filepath.Join(home, ".claude", "plugins", "cache",
		"fake-market", "bad-plugin", "1.0.0", "skills", "audit")
	if err := os.MkdirAll(goodDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goodDir, "SKILL.md"),
		[]byte("---\nname: review-pr\ndescription: PR review\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Malformed: no closing frontmatter delimiter.
	if err := os.WriteFile(filepath.Join(badDir, "SKILL.md"),
		[]byte("---\nname: audit\ndescription: uh oh"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &claudecode.ClaudeCodeAgent{}
	skills, err := a.DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatalf("malformed SKILL.md should not abort discovery; got err=%v", err)
	}
	if len(skills) != 1 {
		t.Errorf("good skill should still appear, got %+v", skills)
	}
}

// TestDiscoverReviewSkills_FindsPluginCommand covers the pr-review-toolkit
// shape: slash commands live as flat *.md files under <plugin>/commands/,
// with description in YAML frontmatter and the invocation name derived
// from the filename (no `name:` field).
func TestDiscoverReviewSkills_FindsPluginCommand(t *testing.T) {
	home := withFakeHome(t)
	cmdDir := filepath.Join(home, ".claude", "plugins", "cache",
		"fake-market", "pr-review-toolkit", "unknown", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Frontmatter with quoted description and no `name:` field — exactly
	// the shape pr-review-toolkit ships (verified on-disk).
	content := `---
description: "Comprehensive PR review using specialized agents"
argument-hint: "[review-aspects]"
allowed-tools: ["Bash", "Read"]
---

# Comprehensive PR Review
`
	if err := os.WriteFile(filepath.Join(cmdDir, "review-pr.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &claudecode.ClaudeCodeAgent{}
	skills, err := a.DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 plugin command, got %d: %+v", len(skills), skills)
	}
	if skills[0].Name != "/pr-review-toolkit:review-pr" {
		t.Errorf("Name = %q, want /pr-review-toolkit:review-pr", skills[0].Name)
	}
	if skills[0].Description != "Comprehensive PR review using specialized agents" {
		t.Errorf("Description = %q", skills[0].Description)
	}
}

// TestDiscoverReviewSkills_FindsPluginAgent covers the same shape for
// <plugin>/agents/ — pr-review-toolkit ships subagents (code-reviewer,
// silent-failure-hunter, etc.) there. Same flat .md file format.
func TestDiscoverReviewSkills_FindsPluginAgent(t *testing.T) {
	home := withFakeHome(t)
	agentDir := filepath.Join(home, ".claude", "plugins", "cache",
		"fake-market", "pr-review-toolkit", "unknown", "agents")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `---
description: "Audit code against project style"
---
`
	if err := os.WriteFile(filepath.Join(agentDir, "code-reviewer.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &claudecode.ClaudeCodeAgent{}
	skills, err := a.DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 plugin agent, got %d: %+v", len(skills), skills)
	}
	if skills[0].Name != "/pr-review-toolkit:code-reviewer" {
		t.Errorf("Name = %q, want /pr-review-toolkit:code-reviewer", skills[0].Name)
	}
}

// TestDiscoverReviewSkills_SkipsNonReviewCommand verifies that commands
// whose filename doesn't match keywords (and whose plugin prefix doesn't
// either) are dropped by the name-only Matches filter.
func TestDiscoverReviewSkills_SkipsNonReviewCommand(t *testing.T) {
	home := withFakeHome(t)
	cmdDir := filepath.Join(home, ".claude", "plugins", "cache",
		"fake-market", "unrelated-plugin", "1.0.0", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `---
description: "Format your code nicely"
---
`
	if err := os.WriteFile(filepath.Join(cmdDir, "format.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &claudecode.ClaudeCodeAgent{}
	skills, err := a.DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 0 {
		t.Errorf("unrelated command should be skipped, got %+v", skills)
	}
}

// TestDiscoverReviewSkills_SkipsReadme verifies README.md files sitting
// alongside commands/agents don't get parsed as skills (pr-review-toolkit
// and several other plugins ship a README.md next to commands/).
func TestDiscoverReviewSkills_SkipsReadme(t *testing.T) {
	home := withFakeHome(t)
	cmdDir := filepath.Join(home, ".claude", "plugins", "cache",
		"fake-market", "pr-review-toolkit", "unknown", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// README has no frontmatter and would otherwise trigger parse errors.
	if err := os.WriteFile(filepath.Join(cmdDir, "README.md"),
		[]byte("# PR Review Toolkit\n\nSome prose.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &claudecode.ClaudeCodeAgent{}
	skills, err := a.DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 0 {
		t.Errorf("README.md should be skipped, got %+v", skills)
	}
}

// TestDiscoverReviewSkills_DedupesPluginVersions verifies that when a plugin
// has multiple version directories (common after an upgrade — old version
// isn't always cleaned up), only the latest version's skills appear once.
//
// Without dedupe, the picker would show every review skill twice with no
// way to tell the entries apart, and the prompt to the agent would list
// the same skill multiple times.
func TestDiscoverReviewSkills_DedupesPluginVersions(t *testing.T) {
	home := withFakeHome(t)
	old := filepath.Join(home, ".claude", "plugins", "cache",
		"fake-market", "pr-review-toolkit", "0.1.0", "skills", "review-pr")
	newer := filepath.Join(home, ".claude", "plugins", "cache",
		"fake-market", "pr-review-toolkit", "0.2.0", "skills", "review-pr")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newer, 0o755); err != nil {
		t.Fatal(err)
	}
	oldContent := "---\nname: review-pr\ndescription: Old review\n---\n"
	newContent := "---\nname: review-pr\ndescription: New review\n---\n"
	if err := os.WriteFile(filepath.Join(old, "SKILL.md"), []byte(oldContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newer, "SKILL.md"), []byte(newContent), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &claudecode.ClaudeCodeAgent{}
	skills, err := a.DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 deduped skill, got %d: %+v", len(skills), skills)
	}
	if skills[0].Description != "New review" {
		t.Errorf("Description = %q, want %q (latest version should win)", skills[0].Description, "New review")
	}
}

// TestDiscoverReviewSkills_NonSemverVersionFallback verifies that a plugin
// version dir like "unknown" (which pr-review-toolkit ships) is still
// scanned when no semver dirs are present.
func TestDiscoverReviewSkills_NonSemverVersionFallback(t *testing.T) {
	home := withFakeHome(t)
	skillDir := filepath.Join(home, ".claude", "plugins", "cache",
		"fake-market", "pr-review-toolkit", "unknown", "skills", "review-pr")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: review-pr\ndescription: Review\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &claudecode.ClaudeCodeAgent{}
	skills, err := a.DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill from non-semver version, got %d: %+v", len(skills), skills)
	}
}

// TestDiscoverReviewSkills_SemverWinsOverNonSemver verifies that when a
// plugin has both a semver-shaped version and a non-semver one (e.g.
// "0.2.0" alongside "unknown"), the semver dir is picked. This matches
// real upgrade flows where the old "unknown" stub remains alongside the
// installed semver version.
func TestDiscoverReviewSkills_SemverWinsOverNonSemver(t *testing.T) {
	home := withFakeHome(t)
	semverSkill := filepath.Join(home, ".claude", "plugins", "cache",
		"fake-market", "pr-review-toolkit", "0.2.0", "skills", "review-pr")
	unknownSkill := filepath.Join(home, ".claude", "plugins", "cache",
		"fake-market", "pr-review-toolkit", "unknown", "skills", "review-pr")
	if err := os.MkdirAll(semverSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unknownSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semverSkill, "SKILL.md"),
		[]byte("---\nname: review-pr\ndescription: From 0.2.0\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unknownSkill, "SKILL.md"),
		[]byte("---\nname: review-pr\ndescription: From unknown\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &claudecode.ClaudeCodeAgent{}
	skills, err := a.DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d: %+v", len(skills), skills)
	}
	if skills[0].Description != "From 0.2.0" {
		t.Errorf("Description = %q, want From 0.2.0 (semver should win)", skills[0].Description)
	}
}

func TestDiscoverReviewSkills_UserSkillsDir(t *testing.T) {
	home := withFakeHome(t)
	userSkillDir := filepath.Join(home, ".claude", "skills", "my-review")
	if err := os.MkdirAll(userSkillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userSkillDir, "SKILL.md"),
		[]byte("---\nname: my-review\ndescription: personal review skill\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &claudecode.ClaudeCodeAgent{}
	skills, err := a.DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 user skill, got %d", len(skills))
	}
	if skills[0].Name != "/my-review" {
		t.Errorf("user skill name = %q, want /my-review", skills[0].Name)
	}
}
