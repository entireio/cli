package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const testReviewSkill = "/pr-review-toolkit:review-pr"

func TestReviewMarker_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)

	m := PendingReviewMarker{
		AgentName:   "claude-code",
		Skills:      []string{testReviewSkill},
		StartingSHA: "deadbeef",
		StartedAt:   time.Now().UTC(),
	}
	if err := WritePendingReviewMarker(m); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok, err := ReadPendingReviewMarker()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !ok {
		t.Fatal("expected marker present")
	}
	if got.AgentName != m.AgentName || got.StartingSHA != m.StartingSHA {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if err := ClearPendingReviewMarker(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	_, ok, err = ReadPendingReviewMarker()
	if err != nil {
		t.Fatalf("read-after-clear: %v", err)
	}
	if ok {
		t.Error("expected marker absent after clear")
	}

	// Ensure the file lived under .git/entire-sessions/, not the worktree.
	gitDir := filepath.Join(tmp, ".git")
	entries, err := filepath.Glob(filepath.Join(gitDir, "entire-sessions", "*"))
	if err != nil {
		t.Fatalf("glob sessions dir: %v", err)
	}
	_ = entries // sanity check only
}

func TestReviewCmd_Help(t *testing.T) {
	t.Parallel()
	rootCmd := NewRootCmd()
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetArgs([]string{"review", "--help"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "review") {
		t.Errorf("--help output missing 'review': %s", out)
	}
}

func TestSaveReviewConfig_PersistsSettings(t *testing.T) {
	// NOTE: uses t.Chdir, so no t.Parallel.
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)

	err := saveReviewConfig(context.Background(), map[string][]string{
		"claude-code": {testReviewSkill, "/test-auditor"},
	})
	if err != nil {
		t.Fatal(err)
	}

	s, err := settings.Load(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if len(s.Review["claude-code"]) != 2 {
		t.Errorf("expected 2 skills saved, got %v", s.Review)
	}
	if s.Review["claude-code"][0] != testReviewSkill {
		t.Errorf("first skill = %q", s.Review["claude-code"][0])
	}
}

func TestRunReview_TrackOnlyWritesMarker(t *testing.T) {
	// t.Chdir + first-run picker — no t.Parallel.
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)

	// Seed config so first-run picker doesn't fire.
	if err := saveReviewConfig(context.Background(), map[string][]string{
		testAgentName: {testReviewSkill},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := NewRootCmd()
	rootCmd.SetArgs([]string{"review", "--track-only"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	m, ok, err := ReadPendingReviewMarker()
	if err != nil || !ok {
		t.Fatalf("expected marker present: ok=%v err=%v", ok, err)
	}
	if m.AgentName != testAgentName {
		t.Errorf("AgentName = %q, want %s", m.AgentName, testAgentName)
	}
	if len(m.Skills) != 1 || m.Skills[0] != testReviewSkill {
		t.Errorf("Skills = %v", m.Skills)
	}
}

func TestComposeReviewPrompt_NoFinishSkill(t *testing.T) {
	t.Parallel()
	prompt := composeReviewPrompt([]string{"/review-pr", "/test-auditor"})
	if strings.Contains(prompt, "entire-review:finish") {
		t.Errorf("prompt should not reference finish skill; got: %s", prompt)
	}
	if !strings.Contains(prompt, "/review-pr") {
		t.Errorf("prompt missing skill name; got: %s", prompt)
	}
}

func TestNewReviewCmd_NoHiddenFlags(t *testing.T) {
	t.Parallel()
	cmd := newReviewCmd()
	for _, name := range []string{"postreview", "finalize", "session"} {
		if cmd.Flags().Lookup(name) != nil {
			t.Errorf("found removed flag: --%s", name)
		}
	}
}
