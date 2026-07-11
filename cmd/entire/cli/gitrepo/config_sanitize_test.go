package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v6/memfs"
)

// Regression for #778: a repo whose .git/config carries git 2.29+ negative
// (exclusion) fetch refspecs must still open. go-git's refspec parser rejects
// the '^' form, so before the config sanitizer every entire command failed with
// "malformed refspec, separators are wrong".
func TestOpenPath_ToleratesNegativeRefspecs(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	runGit("remote", "add", "origin", "git@github.com:example/example.git")

	// Append negative fetch refspecs (valid for git, unparseable by go-git).
	cfgPath := filepath.Join(dir, ".git", "config")
	f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\tfetch = ^refs/heads/excluded\n\tfetch = ^refs/heads/other\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	repo, err := OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath with negative refspecs should succeed, got: %v", err)
	}

	// The positive refspec must survive; the negatives must be gone.
	cfg, err := repo.Config()
	if err != nil {
		t.Fatalf("repo.Config(): %v", err)
	}
	origin, ok := cfg.Remotes["origin"]
	if !ok {
		t.Fatal("origin remote missing after sanitize")
	}
	joined := strings.Join(remoteFetchStrings(origin.Fetch), " ")
	if !strings.Contains(joined, "refs/heads/*:refs/remotes/origin/*") {
		t.Errorf("positive refspec dropped; got %q", joined)
	}
	if strings.Contains(joined, "^") {
		t.Errorf("negative refspec should have been stripped; got %q", joined)
	}

	// The on-disk config must be untouched (native git still needs the negatives).
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "^refs/heads/excluded") {
		t.Error("on-disk config was modified; negative refspec removed from the real file")
	}
}

func remoteFetchStrings[T any](specs []T) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		if str, ok := any(s).(interface{ String() string }); ok {
			out = append(out, str.String())
		}
	}
	return out
}

func TestSanitizedConfig(t *testing.T) {
	tests := []struct {
		name     string
		config   string
		wantOK   bool
		contains []string // substrings that must remain
		absent   []string // substrings that must be gone
	}{
		{
			name:   "strips negative fetch refspecs, keeps the rest",
			config: "[remote \"origin\"]\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n\tfetch = ^refs/heads/excluded\n\turl = git@github.com:x/y.git\n",
			wantOK: true,
			contains: []string{
				"fetch = +refs/heads/*:refs/remotes/origin/*",
				"url = git@github.com:x/y.git",
			},
			absent: []string{"^refs/heads/excluded"},
		},
		{
			name:   "no negative refspecs — unchanged",
			config: "[remote \"origin\"]\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n",
			wantOK: false,
		},
		{
			name:     "caret outside a fetch line is preserved",
			config:   "[remote \"origin\"]\n\turl = https://example.com/weird^path\n\tfetch = ^refs/heads/x\n",
			wantOK:   true,
			contains: []string{"url = https://example.com/weird^path"},
			absent:   []string{"fetch = ^refs/heads/x"},
		},
		{
			name:   "no caret at all — fast path, unchanged",
			config: "[core]\n\tbare = false\n",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mem := memfs.New()
			f, err := mem.Create(configFilePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte(tc.config)); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()

			fs := &configSanitizeFS{Filesystem: mem}
			got, ok := fs.sanitizedConfig()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got=%q)", ok, tc.wantOK, got)
			}
			if !ok {
				return
			}
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("sanitized config missing %q\n%s", want, got)
				}
			}
			for _, no := range tc.absent {
				if strings.Contains(got, no) {
					t.Errorf("sanitized config still contains %q\n%s", no, got)
				}
			}
		})
	}
}

// Regression for the finding on config_sanitize.go:85 — a config that exceeds
// maxConfigReadBytes must not fall back to the raw file, because a negative
// fetch refspec past the cap would still reach go-git's parser unstripped and
// reintroduce #778 for oversized configs.
func TestSanitizedConfig_OverCap(t *testing.T) {
	// Pad the config comfortably past the cap with an innocuous comment line,
	// then place a negative fetch refspec near the end, well within the
	// visible (capped) prefix.
	padding := strings.Repeat("# padding line to exceed the read cap\n", (maxConfigReadBytes/38)+1000)
	config := "[remote \"origin\"]\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n" +
		"\tfetch = ^refs/heads/excluded\n" + padding

	if len(config) <= maxConfigReadBytes {
		t.Fatalf("test config (%d bytes) does not exceed maxConfigReadBytes (%d)", len(config), maxConfigReadBytes)
	}

	mem := memfs.New()
	f, err := mem.Create(configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(config)); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	fs := &configSanitizeFS{Filesystem: mem}
	got, ok := fs.sanitizedConfig()
	if !ok {
		t.Fatal("sanitizedConfig should sanitize an over-cap file instead of falling back to the raw file")
	}
	if strings.Contains(got, "fetch = ^refs/heads/excluded") {
		t.Errorf("negative refspec should have been stripped from an over-cap config; got %q", got)
	}
	if !strings.Contains(got, "fetch = +refs/heads/*:refs/remotes/origin/*") {
		t.Error("positive refspec near the start of an over-cap config should survive")
	}
}

// Regression: even when an over-cap config has no negative refspec within
// the visible prefix, ok must still be true — content past the cap is
// unseen and could hold one, so the caller must never fall back to the raw
// file for a truncated read.
func TestSanitizedConfig_OverCap_NoCaretInPrefix(t *testing.T) {
	config := "[core]\n\tbare = false\n" + strings.Repeat("# padding line to exceed the read cap\n", (maxConfigReadBytes/38)+1000)

	if len(config) <= maxConfigReadBytes {
		t.Fatalf("test config (%d bytes) does not exceed maxConfigReadBytes (%d)", len(config), maxConfigReadBytes)
	}

	mem := memfs.New()
	f, err := mem.Create(configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(config)); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	fs := &configSanitizeFS{Filesystem: mem}
	_, ok := fs.sanitizedConfig()
	if !ok {
		t.Fatal("sanitizedConfig should report ok=true for a truncated read even without a visible negative refspec")
	}
}
