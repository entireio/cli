package agentimport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/cursor"
)

// collidingRepos creates two distinct repo directories whose agent project-dir
// encodings are identical (…/acme/foo-bar and …/acme-foo/bar), returning
// (a, b).
func collidingRepos(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	a := filepath.Join(base, "acme", "foo-bar")
	b := filepath.Join(base, "acme-foo", "bar")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

// jsonLine marshals fields as one JSONL line.
func jsonLine(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sessionIDs(files []SessionFile) []string {
	ids := make([]string, 0, len(files))
	for _, f := range files {
		ids = append(ids, f.SessionID)
	}
	return ids
}

// TestRepoScopedDiscover_SkipsSessionsFromCollidingRepo is the T-291
// regression: a session recorded in repo B must not be imported into repo A
// just because both repos share one lossy-encoded project directory.
func TestRepoScopedDiscover_SkipsSessionsFromCollidingRepo(t *testing.T) {
	t.Parallel()
	a, b := collidingRepos(t)
	if claudecode.SanitizePathForClaude(a) != claudecode.SanitizePathForClaude(b) {
		t.Fatalf("fixture paths must collide: %q vs %q", a, b)
	}

	type fixture struct {
		name string
		// lines builds a transcript recording cwd ("" = no cwd recorded).
		lines func(cwd string) []string
		imp   Importer
		ids   func(stems ...string) []string
	}
	claudeLines := func(cwd string) []string {
		user := map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": "hi"}}
		if cwd != "" {
			user["cwd"] = cwd
		}
		// A leading summary line carries no cwd; discovery must look past it.
		return []string{`{"type":"summary","summary":"s"}`, jsonLine(t, user)}
	}
	piLines := func(cwd string) []string {
		header := map[string]any{"type": "session", "version": 3, "id": "s"}
		if cwd != "" {
			header["cwd"] = cwd
		}
		return []string{jsonLine(t, header)}
	}
	factoryLines := func(cwd string) []string {
		start := map[string]any{"type": "session_start", "id": "s", "version": 2}
		if cwd != "" {
			start["cwd"] = cwd
		}
		return []string{jsonLine(t, start)}
	}
	identity := func(stems ...string) []string { return stems }
	piIDs := func(stems ...string) []string {
		out := make([]string, len(stems))
		for i, s := range stems {
			out[i] = piSessionID(s)
		}
		return out
	}

	for _, fx := range []fixture{
		{"claude", claudeLines, claudeImporter{}, identity},
		{"pi", piLines, piImporter{}, piIDs},
		{"factory", factoryLines, factoryImporter{}, identity},
	} {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			// The subdirectory must exist so its path resolves through the same
			// symlinks as a (e.g. macOS /var -> /private/var).
			if err := os.MkdirAll(filepath.Join(a, "pkg"), 0o755); err != nil {
				t.Fatal(err)
			}
			stems := map[string]string{
				"2026-06-20T00-00-00-000Z_own":    a,
				"2026-06-20T00-00-00-000Z_subdir": filepath.Join(a, "pkg"),
				"2026-06-20T00-00-00-000Z_other":  b,
				"2026-06-20T00-00-00-000Z_nocwd":  "",
			}
			for stem, cwd := range stems {
				writeLines(t, filepath.Join(dir, stem+".jsonl"), fx.lines(cwd)...)
			}

			got, err := fx.imp.Discover(a, dir, time.Now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			want := fx.ids("2026-06-20T00-00-00-000Z_own", "2026-06-20T00-00-00-000Z_subdir")
			if ids := sessionIDs(got); !slices.Equal(ids, want) {
				t.Fatalf("discovered %v, want %v (sessions from %s or without a cwd must be skipped)", ids, want, b)
			}
		})
	}
}

func TestFirstRecordedCwd_UnboundedLineAndMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	long := filepath.Join(dir, "long.jsonl")
	// A first line past bufio.Scanner's default 64 KiB token limit must not
	// hide the cwd recorded after it.
	writeLines(t, long, `{"type":"summary","summary":"`+strings.Repeat("x", 200_000)+`"}`, `{"cwd":"/repo"}`)
	if got := firstRecordedCwd(long); got != "/repo" {
		t.Errorf("firstRecordedCwd(long) = %q, want /repo", got)
	}

	noNewline := filepath.Join(dir, "tail.jsonl")
	if err := os.WriteFile(noNewline, []byte(`{"cwd":"/tail"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := firstRecordedCwd(noNewline); got != "/tail" {
		t.Errorf("firstRecordedCwd(no trailing newline) = %q, want /tail", got)
	}

	if got := firstRecordedCwd(filepath.Join(dir, "missing.jsonl")); got != "" {
		t.Errorf("firstRecordedCwd(missing) = %q, want empty", got)
	}
}

func TestPathsWithEncoding_FindsCollidingDirectories(t *testing.T) {
	t.Parallel()
	a, b := collidingRepos(t)
	if cursor.SanitizePathForCursor(a) != cursor.SanitizePathForCursor(b) {
		t.Fatalf("fixture paths must collide: %q vs %q", a, b)
	}

	matches, unscanned := pathsWithEncoding(a, cursor.SanitizePathForCursor, cursorCollisionReadLimit)
	if unscanned != "" {
		t.Fatalf("walk should complete within the read limit, got unscanned %q", unscanned)
	}
	var sawA, sawB bool
	for _, m := range matches {
		sawA = sawA || samePath(m, a)
		sawB = sawB || samePath(m, b)
	}
	if !sawA || !sawB {
		t.Fatalf("matches = %v, want both %s and %s", matches, a, b)
	}

	if _, unscanned := pathsWithEncoding(a, cursor.SanitizePathForCursor, 1); unscanned == "" {
		t.Error("walk with a one-read budget should report incomplete")
	}
}

func TestCursorProjectOtherPath(t *testing.T) {
	t.Parallel()
	a, b := collidingRepos(t)
	// A repo whose encoding collides with nothing else on disk.
	lone := filepath.Join(t.TempDir(), "lone")
	if err := os.Mkdir(lone, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTrusted := func(t *testing.T, workspace string) string {
		t.Helper()
		projectDir := t.TempDir()
		writeLines(t, filepath.Join(projectDir, ".workspace-trusted"), jsonLine(t, map[string]any{"workspacePath": workspace}))
		return projectDir
	}

	t.Run("colliding directory on disk", func(t *testing.T) {
		t.Parallel()
		other, shared := cursorProjectOtherPath(a, t.TempDir(), cursorCollisionReadLimit)
		if !shared || !samePath(other, b) {
			t.Fatalf("got (%q, %v), want (%q, true)", other, shared, b)
		}
	})
	t.Run("workspace-trusted names another path", func(t *testing.T) {
		t.Parallel()
		other, shared := cursorProjectOtherPath(lone, writeTrusted(t, "/elsewhere"), cursorCollisionReadLimit)
		if !shared || other != "/elsewhere" {
			t.Fatalf("got (%q, %v), want (/elsewhere, true)", other, shared)
		}
	})
	t.Run("workspace-trusted names this repo", func(t *testing.T) {
		t.Parallel()
		if other, shared := cursorProjectOtherPath(lone, writeTrusted(t, lone), cursorCollisionReadLimit); shared {
			t.Fatalf("unexpected collision with %q", other)
		}
	})
	t.Run("unreadable workspace-trusted fails closed", func(t *testing.T) {
		t.Parallel()
		for _, content := range []string{"not json", `{"trustedAt":"x"}`} {
			projectDir := t.TempDir()
			writeLines(t, filepath.Join(projectDir, ".workspace-trusted"), content)
			if other, shared := cursorProjectOtherPath(lone, projectDir, cursorCollisionReadLimit); !shared {
				t.Errorf("workspace-trusted %q: got (%q, false), want shared", content, other)
			}
		}
	})
	t.Run("walk budget exhausted fails closed", func(t *testing.T) {
		t.Parallel()
		if _, shared := cursorProjectOtherPath(lone, t.TempDir(), 1); !shared {
			t.Fatal("an incomplete collision walk must be treated as shared")
		}
	})
}

// TestCursorDiscover_RefusesSharedProjectDir exercises the default-directory
// path (no --path override), which is where the collision check applies.
// Not parallel: GetSessionDir reads ENTIRE_TEST_CURSOR_PROJECT_DIR.
func TestCursorDiscover_RefusesSharedProjectDir(t *testing.T) {
	a, b := collidingRepos(t)
	transcripts := filepath.Join(t.TempDir(), "project", "agent-transcripts")
	if err := os.MkdirAll(transcripts, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(transcripts, "sess.jsonl"), `{"role":"user","message":{"content":"hi"}}`)
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", transcripts)

	_, err := cursorImporter{}.Discover(a, "", time.Now(), nil)
	if err == nil || !strings.Contains(err.Error(), "none were imported") {
		t.Fatalf("Discover with colliding %s: err = %v, want refusal", b, err)
	}

	// An explicit --path is the user's choice of directory; no collision check.
	got, err := cursorImporter{}.Discover(a, transcripts, time.Now(), nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("Discover with override: got %v, err %v; want 1 session", got, err)
	}

	// Once the colliding repo is gone the directory is unambiguous.
	if err := os.RemoveAll(b); err != nil {
		t.Fatal(err)
	}
	got, err = cursorImporter{}.Discover(a, "", time.Now(), nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("Discover without collision: got %v, err %v; want 1 session", got, err)
	}
}

// TestPathsWithEncoding_UnreadableDirectoryFailsClosed: a candidate directory
// that cannot be listed may hide a colliding workspace (e.g. a traverse-only
// parent), so the walk must report it rather than claim completeness.
func TestPathsWithEncoding_UnreadableDirectoryFailsClosed(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs POSIX permissions enforced for the current user")
	}
	base := t.TempDir()
	target := filepath.Join(base, "x-y", "z")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// base/x encodes to a prefix of base/x-y/z, so the walk must list it.
	decoy := filepath.Join(base, "x")
	if err := os.Mkdir(decoy, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(decoy, 0o755); err != nil {
			t.Error(err)
		}
	})

	_, unscanned := pathsWithEncoding(target, cursor.SanitizePathForCursor, cursorCollisionReadLimit)
	if !strings.Contains(unscanned, decoy) {
		t.Fatalf("unscanned = %q, want it to name %s", unscanned, decoy)
	}
	if other, shared := cursorProjectOtherPath(target, t.TempDir(), cursorCollisionReadLimit); !shared {
		t.Fatalf("an unreadable candidate directory must be treated as shared, got (%q, false)", other)
	}
}

// TestPathsWithEncoding_DanglingLinkIsConclusive: a dangling symlink cannot be
// a workspace, so it must not make the walk incomplete.
func TestPathsWithEncoding_DanglingLinkIsConclusive(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	target := filepath.Join(base, "x-y", "z")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "missing"), filepath.Join(base, "x")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, unscanned := pathsWithEncoding(target, cursor.SanitizePathForCursor, cursorCollisionReadLimit); unscanned != "" {
		t.Fatalf("unscanned = %q, want a complete walk", unscanned)
	}
}

func TestRepoMatches(t *testing.T) {
	t.Parallel()
	root := filepath.Join(string(filepath.Separator), "w", "repo")
	for _, tc := range []struct {
		cwd  string
		want bool
	}{
		{root, true},
		{filepath.Join(root, "pkg"), true},
		{filepath.Join(root, "..cache"), true},
		{filepath.Join(root, "..cache", "sub"), true},
		{filepath.Join(string(filepath.Separator), "w", "repo-sibling"), false},
		{filepath.Join(string(filepath.Separator), "w"), false},
		{string(filepath.Separator), false},
		{"relative/repo", false},
		{".", false},
		{"", false},
	} {
		if got := repoMatches(tc.cwd, root); got != tc.want {
			t.Errorf("repoMatches(%q, %q) = %v, want %v", tc.cwd, root, got, tc.want)
		}
	}
}

// TestRepoMatches_DeletedSubdirUnderSymlinkedRoot: a session recorded in a
// subdirectory that has since been deleted, through a symlinked spelling of
// the repo, must still match the repo.
func TestRepoMatches_DeletedSubdirUnderSymlinkedRoot(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "real", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(base, "real"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cwd := filepath.Join(link, "repo", "deleted", "pkg")
	for _, root := range []string{filepath.Join(link, "repo"), filepath.Join(base, "real", "repo")} {
		if !repoMatches(cwd, root) {
			t.Errorf("repoMatches(%q, %q) = false, want true", cwd, root)
		}
	}
	if !samePath(filepath.Join(link, "repo", "gone"), filepath.Join(base, "real", "repo", "gone")) {
		t.Error("samePath should resolve the symlinked ancestor of a missing path")
	}
	if repoMatches(filepath.Join(link, "other", "deleted"), filepath.Join(base, "real", "repo")) {
		t.Error("a missing path outside the repo must not match")
	}
}

// TestDiscoverSessionFiles_KeepRunsAfterCheapFilters: keep may open the
// transcript, so files excluded by the session filter or the lookback window
// must never reach it.
func TestDiscoverSessionFiles_KeepRunsAfterCheapFilters(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, id := range []string{"wanted", "filtered-out", "old"} {
		writeLines(t, filepath.Join(dir, id+".jsonl"), `{}`)
	}
	now := time.Now()
	old := now.AddDate(0, 0, -LookbackDays-1)
	if err := os.Chtimes(filepath.Join(dir, "old.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}

	var seen []string
	keep := func(path string) bool {
		seen = append(seen, filepath.Base(path))
		return true
	}
	got, err := discoverSessionFiles(dir, now, []string{"wanted", "old"},
		jsonlSessionResolver(identitySessionID), keep)
	if err != nil {
		t.Fatal(err)
	}
	if ids := sessionIDs(got); !slices.Equal(ids, []string{"wanted"}) {
		t.Fatalf("discovered %v, want [wanted]", ids)
	}
	if !slices.Equal(seen, []string{"wanted.jsonl"}) {
		t.Fatalf("keep saw %v, want only wanted.jsonl", seen)
	}
}

// TestRepoMatches_CaseInsensitiveSpelling: on a case-insensitive filesystem a
// differently-cased spelling of the repo is the same directory and must match.
func TestRepoMatches_CaseInsensitiveSpelling(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repo := filepath.Join(base, "Repo")
	if err := os.MkdirAll(filepath.Join(repo, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(base, "REPO")
	if _, err := os.Stat(upper); err != nil {
		t.Skip("filesystem is case-sensitive")
	}
	if !samePath(upper, repo) {
		t.Errorf("samePath(%q, %q) = false, want true", upper, repo)
	}
	for _, cwd := range []string{upper, filepath.Join(upper, "pkg"), filepath.Join(upper, "pkg", "deleted")} {
		if !repoMatches(cwd, repo) {
			t.Errorf("repoMatches(%q, %q) = false, want true", cwd, repo)
		}
	}
	if repoMatches(filepath.Join(base, "REPO-other"), repo) {
		t.Error("a different directory must not match")
	}
}
