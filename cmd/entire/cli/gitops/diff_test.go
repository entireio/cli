package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

// initTestRepo creates a temp git repo and returns its path.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	run("init", "-b", "main")
	run("config", "commit.gpgsign", "false")

	return dir
}

// commitHash returns the HEAD commit hash.
func commitHash(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD failed: %v", err)
	}
	return string(out[:len(out)-1]) // trim newline
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitAdd(t *testing.T, dir string, files ...string) {
	t.Helper()
	args := append([]string{"add"}, files...)
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v\n%s", err, out)
	}
}

func gitCommit(t *testing.T, dir, msg string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "commit", "-m", msg, "--allow-empty")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v\n%s", err, out)
	}
}

func TestDiffTreeFiles_NormalCommit(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	writeFile(t, dir, "file1.go", "package main\n")
	gitAdd(t, dir, "file1.go")
	gitCommit(t, dir, "initial")
	commit1 := commitHash(t, dir)

	writeFile(t, dir, "file1.go", "package main\n\nfunc main() {}\n")
	writeFile(t, dir, "file2.go", "package util\n")
	gitAdd(t, dir, "file1.go", "file2.go")
	gitCommit(t, dir, "second")
	commit2 := commitHash(t, dir)

	result, err := DiffTreeFiles(context.Background(), dir, commit1, commit2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 changed files, got %d: %v", len(result), result)
	}
	if _, ok := result["file1.go"]; !ok {
		t.Error("expected file1.go in result")
	}
	if _, ok := result["file2.go"]; !ok {
		t.Error("expected file2.go in result")
	}
}

func TestDiffTreeFiles_InitialCommit(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	writeFile(t, dir, "a.go", "package a\n")
	writeFile(t, dir, "b.go", "package b\n")
	gitAdd(t, dir, "a.go", "b.go")
	gitCommit(t, dir, "initial")
	commit := commitHash(t, dir)

	result, err := DiffTreeFiles(context.Background(), dir, "", commit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(result), result)
	}
	if _, ok := result["a.go"]; !ok {
		t.Error("expected a.go in result")
	}
	if _, ok := result["b.go"]; !ok {
		t.Error("expected b.go in result")
	}
}

func TestDiffTreeFileList_MultiCommitRange(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	writeFile(t, dir, "base.go", "package base\n")
	gitAdd(t, dir, "base.go")
	gitCommit(t, dir, "initial")
	commit1 := commitHash(t, dir)

	writeFile(t, dir, "new.go", "package new\n")
	gitAdd(t, dir, "new.go")
	gitCommit(t, dir, "add new")

	writeFile(t, dir, "base.go", "package base\n\nfunc init() {}\n")
	writeFile(t, dir, "third.go", "package third\n")
	gitAdd(t, dir, "base.go", "third.go")
	gitCommit(t, dir, "third")
	commit3 := commitHash(t, dir)

	files, err := DiffTreeFileList(context.Background(), dir, commit1, commit3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sort.Strings(files)

	if len(files) != 3 {
		t.Fatalf("expected 3 changed files, got %d: %v", len(files), files)
	}
	expected := []string{"base.go", "new.go", "third.go"}
	for i, f := range expected {
		if files[i] != f {
			t.Errorf("files[%d] = %s, want %s", i, files[i], f)
		}
	}
}

func TestDiffTreeFiles_DeletedFile(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	writeFile(t, dir, "keep.go", "package keep\n")
	writeFile(t, dir, "delete.go", "package delete\n")
	gitAdd(t, dir, "keep.go", "delete.go")
	gitCommit(t, dir, "initial")
	commit1 := commitHash(t, dir)

	cmd := exec.CommandContext(context.Background(), "git", "rm", "delete.go")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git rm failed: %v\n%s", err, out)
	}
	gitCommit(t, dir, "remove file")
	commit2 := commitHash(t, dir)

	result, err := DiffTreeFiles(context.Background(), dir, commit1, commit2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 changed file, got %d: %v", len(result), result)
	}
	if _, ok := result["delete.go"]; !ok {
		t.Error("expected delete.go in result")
	}
}

func TestDiffTreeFiles_NoChanges(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	writeFile(t, dir, "file.go", "package main\n")
	gitAdd(t, dir, "file.go")
	gitCommit(t, dir, "initial")
	commit := commitHash(t, dir)

	result, err := DiffTreeFiles(context.Background(), dir, commit, commit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected 0 changed files, got %d: %v", len(result), result)
	}
}

func TestDiffTreeFiles_SubdirectoryFiles(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	writeFile(t, dir, "root.go", "package root\n")
	gitAdd(t, dir, "root.go")
	gitCommit(t, dir, "initial")
	commit1 := commitHash(t, dir)

	writeFile(t, dir, "src/pkg/deep.go", "package deep\n")
	gitAdd(t, dir, "src/pkg/deep.go")
	gitCommit(t, dir, "add deep file")
	commit2 := commitHash(t, dir)

	result, err := DiffTreeFiles(context.Background(), dir, commit1, commit2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 changed file, got %d: %v", len(result), result)
	}
	if _, ok := result["src/pkg/deep.go"]; !ok {
		t.Error("expected src/pkg/deep.go in result")
	}
}

func TestDiffTreeFiles_Rename(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	writeFile(t, dir, "original.go", "package original\n")
	gitAdd(t, dir, "original.go")
	gitCommit(t, dir, "initial")
	commit1 := commitHash(t, dir)

	// Rename via git mv
	cmd := exec.CommandContext(context.Background(), "git", "mv", "original.go", "renamed.go")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git mv failed: %v\n%s", err, out)
	}
	gitCommit(t, dir, "rename file")
	commit2 := commitHash(t, dir)

	result, err := DiffTreeFiles(context.Background(), dir, commit1, commit2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Without -M flag, rename shows as D+A — both old and new names should appear
	if _, ok := result["original.go"]; !ok {
		t.Error("expected original.go in result")
	}
	if _, ok := result["renamed.go"]; !ok {
		t.Error("expected renamed.go in result")
	}
}

func TestDiffTreeFiles_InvalidDir(t *testing.T) {
	t.Parallel()

	_, err := DiffTreeFiles(context.Background(), t.TempDir(), "abc123", "def456")
	if err == nil {
		t.Error("expected error for non-git directory")
	}
}

func TestParseDiffTreeOutput(t *testing.T) {
	t.Parallel()

	t.Run("empty output", func(t *testing.T) {
		t.Parallel()
		result := parseDiffTreeOutput(nil)
		if result != nil {
			t.Errorf("expected nil, got %v", result)
		}
	})

	t.Run("single modified file", func(t *testing.T) {
		t.Parallel()
		data := []byte(":100644 100644 abc1234 def5678 M\x00file1.go\x00")
		result := parseDiffTreeOutput(data)
		if len(result) != 1 || result[0] != "file1.go" {
			t.Errorf("expected [file1.go], got %v", result)
		}
	})

	t.Run("multiple files", func(t *testing.T) {
		t.Parallel()
		data := []byte(":100644 100644 abc1234 def5678 M\x00file1.go\x00:100644 000000 abc1234 0000000 D\x00file2.go\x00:000000 100644 0000000 abc1234 A\x00file3.go\x00")
		result := parseDiffTreeOutput(data)
		if len(result) != 3 {
			t.Fatalf("expected 3 files, got %d: %v", len(result), result)
		}
		sort.Strings(result)
		expected := []string{"file1.go", "file2.go", "file3.go"}
		for i, f := range expected {
			if result[i] != f {
				t.Errorf("result[%d] = %s, want %s", i, result[i], f)
			}
		}
	})

	t.Run("rename", func(t *testing.T) {
		t.Parallel()
		// R/C status only appears with -M/-C flags (defensive handling)
		data := []byte(":100644 100644 abc1234 def5678 R100\x00old.go\x00new.go\x00")
		result := parseDiffTreeOutput(data)
		if len(result) != 2 {
			t.Fatalf("expected 2 files (old + new), got %d: %v", len(result), result)
		}
		sort.Strings(result)
		if result[0] != "new.go" || result[1] != "old.go" {
			t.Errorf("expected [new.go, old.go], got %v", result)
		}
	})

	t.Run("copy", func(t *testing.T) {
		t.Parallel()
		data := []byte(":100644 100644 abc1234 abc1234 C100\x00src.go\x00dst.go\x00")
		result := parseDiffTreeOutput(data)
		if len(result) != 2 {
			t.Fatalf("expected 2 files (src + dst), got %d: %v", len(result), result)
		}
		sort.Strings(result)
		if result[0] != "dst.go" || result[1] != "src.go" {
			t.Errorf("expected [dst.go, src.go], got %v", result)
		}
	})

	t.Run("rename mixed with modify", func(t *testing.T) {
		t.Parallel()
		data := []byte(":100644 100644 abc1234 def5678 R100\x00old.go\x00new.go\x00:100644 100644 abc1234 def5678 M\x00other.go\x00")
		result := parseDiffTreeOutput(data)
		if len(result) != 3 {
			t.Fatalf("expected 3 files, got %d: %v", len(result), result)
		}
		sort.Strings(result)
		expected := []string{"new.go", "old.go", "other.go"}
		for i, f := range expected {
			if result[i] != f {
				t.Errorf("result[%d] = %s, want %s", i, result[i], f)
			}
		}
	})
}

func TestExtractStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected byte
	}{
		{"modify", ":100644 100644 abc1234 def5678 M", 'M'},
		{"add", ":000000 100644 0000000 abc1234 A", 'A'},
		{"delete", ":100644 000000 abc1234 0000000 D", 'D'},
		{"rename with score", ":100644 100644 abc1234 def5678 R100", 'R'},
		{"copy with score", ":100644 100644 abc1234 abc1234 C075", 'C'},
		{"type change", ":100644 120000 abc1234 def5678 T", 'T'},
		{"empty string", "", 0},
		{"too few fields", ":100644 100644 abc1234", 0},
		{"whitespace only", "   ", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractStatus(tt.input)
			if got != tt.expected {
				t.Errorf("extractStatus(%q) = %c (%d), want %c (%d)", tt.input, got, got, tt.expected, tt.expected)
			}
		})
	}
}
