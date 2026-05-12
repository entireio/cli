package pi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestEncodeRepoPathForPi(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
	}{
		{"/Users/foo/repo", "--Users-foo-repo--"},
		{"/Users/foo/repo/", "--Users-foo-repo--"}, // trailing separator stripped
		{"/private/var/folders/2y/T/e2e-repo-1", "--private-var-folders-2y-T-e2e-repo-1--"},
		// Windows: git rev-parse --show-toplevel returns forward slashes
		// regardless of platform — must encode the same way on every OS.
		{`C:/Users/foo/repo`, `--C:-Users-foo-repo--`},
		{`C:/Users/foo/repo/`, `--C:-Users-foo-repo--`},
		// Native Windows separators (in case a caller passes them through).
		{`C:\Users\foo\repo`, `--C:-Users-foo-repo--`},
		{`C:\Users\foo\repo\`, `--C:-Users-foo-repo--`},
		{"", ""},
	}
	for _, tt := range tests {
		got := encodeRepoPathForPi(tt.in)
		if got != tt.want {
			t.Errorf("encodeRepoPathForPi(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGetSessionDir_HonorsTestOverride(t *testing.T) {
	override := t.TempDir()
	t.Setenv(piSessionDirEnvVar, override)

	dir, err := (&PiAgent{}).GetSessionDir("/Users/foo/repo")
	if err != nil {
		t.Fatal(err)
	}
	if dir != override {
		t.Errorf("GetSessionDir = %q, want override %q", dir, override)
	}
}

func TestGetSessionDir_HonorsPiHomeOverride(t *testing.T) {
	piHome := t.TempDir()
	t.Setenv(piHomeEnvVar, piHome)
	t.Setenv(piSessionDirEnvVar, "")

	dir, err := (&PiAgent{}).GetSessionDir("/Users/foo/repo")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(piHome, "sessions", "--Users-foo-repo--")
	if dir != want {
		t.Errorf("GetSessionDir = %q, want %q", dir, want)
	}
}

func TestGetSessionBaseDir_HonorsPiHomeOverride(t *testing.T) {
	piHome := t.TempDir()
	t.Setenv(piHomeEnvVar, piHome)

	base, err := (&PiAgent{}).GetSessionBaseDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(piHome, "sessions")
	if base != want {
		t.Errorf("GetSessionBaseDir = %q, want %q", base, want)
	}
}

// PiAgent must implement SessionBaseDirProvider so attach's
// cross-project fallback (searchTranscriptInProjectDirs) can scan
// sibling project subdirs of ~/.pi/agent/sessions/ when the session
// was started from a different cwd than the current worktree root.
func TestPiAgent_ImplementsSessionBaseDirProvider(t *testing.T) {
	t.Parallel()
	if _, ok := agent.AsSessionBaseDirProvider(NewPiAgent()); !ok {
		t.Fatal("expected pi to implement SessionBaseDirProvider")
	}
}

func TestResolveSessionFile_AbsolutePathPassthrough(t *testing.T) {
	t.Parallel()
	abs := "/tmp/2026-01-01T00-00-00-000Z_abc123.jsonl"
	got := (&PiAgent{}).ResolveSessionFile("/ignored", abs)
	if got != abs {
		t.Errorf("absolute path: got %q, want %q", got, abs)
	}
}

func TestResolveSessionFile_GlobsBySessionID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := "sess-xyz"

	older := filepath.Join(dir, "2026-01-01T00-00-00-000Z_"+id+".jsonl")
	newer := filepath.Join(dir, "2026-06-15T12-00-00-000Z_"+id+".jsonl")
	for _, p := range []string{older, newer} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Unrelated file with a different session ID — must not match.
	unrelated := filepath.Join(dir, "2026-06-15T12-00-00-000Z_other.jsonl")
	if err := os.WriteFile(unrelated, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := (&PiAgent{}).ResolveSessionFile(dir, id)
	if got != newer {
		t.Errorf("ResolveSessionFile picked %q, want most recent %q", got, newer)
	}
}

func TestResolveSessionFile_NoMatchFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := "missing-id"

	got := (&PiAgent{}).ResolveSessionFile(dir, id)
	want := filepath.Join(dir, id+".jsonl")
	if got != want {
		t.Errorf("ResolveSessionFile fallback = %q, want %q", got, want)
	}
	// The fallback must be a non-existent path so callers' stat checks fail cleanly.
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Errorf("expected stat to report not-exist for fallback path, got err=%v", err)
	}
}

func TestResolveSessionFile_NoMatch_NoSessionDir(t *testing.T) {
	t.Parallel()
	got := (&PiAgent{}).ResolveSessionFile("", "sess-xyz")
	if got != "sess-xyz" {
		t.Errorf("ResolveSessionFile with empty dir = %q, want %q", got, "sess-xyz")
	}
}

func TestFindPiSessionByID_EmptyInputs(t *testing.T) {
	t.Parallel()
	if got := findPiSessionByID("", "id"); got != "" {
		t.Errorf("empty dir: got %q, want \"\"", got)
	}
	if got := findPiSessionByID(t.TempDir(), ""); got != "" {
		t.Errorf("empty id: got %q, want \"\"", got)
	}
}
