package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"
)

func TestDirectoryWithinWorktree(t *testing.T) {
	t.Parallel()

	root := string(filepath.Separator) + filepath.Join("repo", "worktree")

	tests := []struct {
		name      string
		candidate string
		root      string
		want      bool
	}{
		{"exact match", root, root, true},
		{"below root", filepath.Join(root, "pkg", "sub"), root, true},
		{"trailing slash on candidate", root + string(filepath.Separator), root, true},
		{"sibling directory", filepath.Join(filepath.Dir(root), "other-worktree"), root, false},
		{"parent of root", filepath.Dir(root), root, false},
		{"empty candidate", "", root, false},
		{"prefix but not a path boundary", root + "-other", root, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := directoryWithinWorktree(tt.candidate, tt.root)
			if got != tt.want {
				t.Errorf("directoryWithinWorktree(%q, %q) = %v, want %v", tt.candidate, tt.root, got, tt.want)
			}
		})
	}
}

func TestEpochMillisToTime(t *testing.T) {
	t.Parallel()

	if got := epochMillisToTime(0); !got.IsZero() {
		t.Errorf("epochMillisToTime(0) = %v, want zero time", got)
	}
	if got := epochMillisToTime(-5); !got.IsZero() {
		t.Errorf("epochMillisToTime(-5) = %v, want zero time", got)
	}
	want := time.UnixMilli(1767225660000)
	if got := epochMillisToTime(1767225660000); !got.Equal(want) {
		t.Errorf("epochMillisToTime(1767225660000) = %v, want %v", got, want)
	}
}

// stubOpenCodeBinary puts a fake `opencode` executable on PATH that ignores
// its arguments and writes stdout to stdoutBody. Used to exercise the real
// runOpenCodeSessionList/ListNativeSessions subprocess call path, the same
// technique cli_commands_test.go uses for `opencode export`.
func stubOpenCodeBinary(t *testing.T, stdoutBody string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub opencode is a shell script")
	}
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "fixture.json"), []byte(stdoutBody), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ndir=$(dirname \"$0\")\ncat \"$dir/fixture.json\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "opencode"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// dirname/cat need the real PATH too — only "opencode" itself is stubbed.
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestListNativeSessions_ScopesToWorktreeAndParsesFields is a real (non
// hand-mocked) test of the production discovery function: it stubs the
// `opencode` binary to answer `session list --format json` with a fixture
// covering an in-scope session, a below-root session, and an out-of-scope
// session, then calls the actual OpenCodeAgent.ListNativeSessions and checks
// the real parsed+filtered result (entireio/cli#1992).
func TestListNativeSessions_ScopesToWorktreeAndParsesFields(t *testing.T) {
	// No t.Parallel: t.Setenv (via stubOpenCodeBinary).
	root := t.TempDir()

	type entry struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Directory string `json:"directory"`
		UpdatedAt int64  `json:"updatedAt"`
	}
	now := time.Now()
	fixture := []entry{
		{ID: "ses_in_root", Title: "docs: example", Directory: root, UpdatedAt: now.Add(-1 * time.Hour).UnixMilli()},
		{ID: "ses_in_subdir", Title: "fix bug in parser", Directory: filepath.Join(root, "pkg", "sub"), UpdatedAt: now.Add(-2 * time.Hour).UnixMilli()},
		{ID: "ses_outside", Title: "unrelated project", Directory: t.TempDir(), UpdatedAt: now.UnixMilli()},
		{ID: "", Title: "no id, must be skipped", Directory: root, UpdatedAt: now.UnixMilli()},
	}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	stubOpenCodeBinary(t, string(raw))

	a := &OpenCodeAgent{}
	got, err := a.ListNativeSessions(context.Background(), root)
	if err != nil {
		t.Fatalf("ListNativeSessions failed: %v", err)
	}

	sort.Slice(got, func(i, j int) bool { return got[i].SessionID < got[j].SessionID })

	if len(got) != 2 {
		t.Fatalf("ListNativeSessions returned %d entries, want 2 (in-root and in-subdir): %+v", len(got), got)
	}
	if got[0].SessionID != "ses_in_root" || got[0].Title != "docs: example" || got[0].Directory != root {
		t.Errorf("unexpected first entry: %+v", got[0])
	}
	if got[1].SessionID != "ses_in_subdir" || got[1].Title != "fix bug in parser" {
		t.Errorf("unexpected second entry: %+v", got[1])
	}
	if got[0].UpdatedAt.IsZero() || got[1].UpdatedAt.IsZero() {
		t.Errorf("expected UpdatedAt to be parsed from updatedAt, got: %+v / %+v", got[0], got[1])
	}
}

func TestRunOpenCodeSessionList_MissingBinary(t *testing.T) {
	// No t.Parallel: t.Setenv.
	t.Setenv("PATH", "")

	_, err := runOpenCodeSessionList(context.Background())
	if err == nil {
		t.Fatal("expected an error with no opencode on PATH")
	}
	var execErr *exec.Error
	if !errors.As(err, &execErr) {
		t.Errorf("expected the underlying error to be classified from exec.ErrNotFound, got: %v", err)
	}
}

func TestRunOpenCodeSessionList_InvalidJSON(t *testing.T) {
	// No t.Parallel: t.Setenv (via stubOpenCodeBinary).
	stubOpenCodeBinary(t, "not json")

	_, err := runOpenCodeSessionList(context.Background())
	if err == nil {
		t.Fatal("expected a parse error for non-JSON output")
	}
}
