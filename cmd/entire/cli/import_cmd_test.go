package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestImportClaudeCode_DryRunReportsCounts(t *testing.T) {
	// Not parallel: uses t.Chdir for CWD-based repo/worktree resolution.
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "init")
	t.Chdir(repoDir)

	claudeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(claudeDir, "s.jsonl"),
		[]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newImportCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"claude-code", "--path", claudeDir, "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v (out=%q)", err, out.String())
	}
	if !strings.Contains(out.String(), "Would import 1") {
		t.Fatalf("dry-run summary missing count: %q", out.String())
	}
}
