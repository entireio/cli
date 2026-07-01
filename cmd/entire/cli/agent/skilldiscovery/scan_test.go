package skilldiscovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeSkillMD(t *testing.T, root, name, contents string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestScanSkillsDir_FindsReviewSkillsAndSkipsRest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkillMD(t, root, "code-review", "---\nname: code-review\ndescription: Review a PR.\n---\nbody")
	writeSkillMD(t, root, "formatter", "---\nname: formatter\ndescription: Format code.\n---\nbody")
	writeSkillMD(t, root, "broken", "no frontmatter here")

	got := ScanSkillsDir(context.Background(), root, "")
	if len(got) != 1 {
		t.Fatalf("got %d skills, want 1: %+v", len(got), got)
	}
	if got[0].Name != "/code-review" {
		t.Errorf("Name = %q, want /code-review", got[0].Name)
	}
	if got[0].Description != "Review a PR." {
		t.Errorf("Description = %q, want %q", got[0].Description, "Review a PR.")
	}
	if got[0].SourcePath == "" {
		t.Error("SourcePath should be populated")
	}
}

func TestScanSkillsDir_FallsBackToDirNameWhenNoNameField(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkillMD(t, root, "security-audit", "---\ndescription: Audit deps.\n---\nbody")

	got := ScanSkillsDir(context.Background(), root, "")
	if len(got) != 1 || got[0].Name != "/security-audit" {
		t.Fatalf("want /security-audit from dir name, got %+v", got)
	}
}

func TestScanSkillsDir_PluginPrefix(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkillMD(t, root, "hunter", "---\nname: hunter\ndescription: x.\n---\nbody")

	got := ScanSkillsDir(context.Background(), root, "pr-review-toolkit")
	if len(got) != 1 || got[0].Name != "/pr-review-toolkit:hunter" {
		t.Fatalf("want /pr-review-toolkit:hunter (matches via plugin prefix), got %+v", got)
	}
}

func TestScanSkillsDir_MissingDirReturnsNil(t *testing.T) {
	t.Parallel()
	got := ScanSkillsDir(context.Background(), filepath.Join(t.TempDir(), "nope"), "")
	if got != nil {
		t.Errorf("want nil for missing dir, got %+v", got)
	}
}
