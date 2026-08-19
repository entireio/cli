package cursor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkProjectDir creates <usersRoot>/<user>/.cursor/projects/<name>[/agent-transcripts].
// When withTranscripts is true, agent-transcripts gets one child entry whose
// mtime is set to evidenceAge before fixedNow — the caller controls whether
// that lands inside or outside the evidence window.
func mkProjectDir(t *testing.T, usersRoot, user, name string, withTranscripts bool, fixedNow time.Time, evidenceAge time.Duration) {
	t.Helper()
	dir := filepath.Join(usersRoot, user, ".cursor", "projects", name)
	if withTranscripts {
		dir = filepath.Join(dir, "agent-transcripts")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !withTranscripts {
		return
	}
	evidence := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(evidence, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := fixedNow.Add(-evidenceAge)
	if err := os.Chtimes(evidence, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// mkEmptyTranscripts creates an agent-transcripts dir with no entries at all.
func mkEmptyTranscripts(t *testing.T, usersRoot, user, name string) {
	t.Helper()
	dir := filepath.Join(usersRoot, user, ".cursor", "projects", name, "agent-transcripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDetectUNCProjectDirs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	usersRoot := t.TempDir()

	// Real observed fingerprint for \\wsl$\Ubuntu\root\probe-repo with agent use.
	mkProjectDir(t, usersRoot, "peyton", "wsl-Ubuntu-root-probe-repo", true, now, time.Hour)
	// wsl.localhost spelling, also used.
	mkProjectDir(t, usersRoot, "peyton", "wsl-localhost-Ubuntu-root-probe-repo", true, now, time.Hour)
	// Lowercase Explorer spelling of the same fingerprint — must match
	// case-insensitively. A distinct user dir avoids colliding with the
	// mixed-case directory above on case-insensitive filesystems (default
	// macOS APFS).
	mkProjectDir(t, usersRoot, "explorer-user", "wsl-ubuntu-root-probe-repo", true, now, time.Hour)
	// Same repo but never used by an agent: browsed only, must NOT match.
	mkProjectDir(t, usersRoot, "other", "wsl-Ubuntu-root-probe-repo", false, now, 0)
	// A different repo: must NOT match.
	mkProjectDir(t, usersRoot, "peyton", "wsl-Ubuntu-root-other-repo", true, now, time.Hour)
	// A native Windows project: must NOT match.
	mkProjectDir(t, usersRoot, "peyton", "C--code-probe-repo", true, now, time.Hour)
	// Matching name, but agent-transcripts is empty: no evidence, must NOT match.
	mkEmptyTranscripts(t, usersRoot, "empty-evidence", "wsl-Ubuntu-root-probe-repo")
	// Matching name, but the only evidence is 30 days old: stale, must NOT match.
	mkProjectDir(t, usersRoot, "stale", "wsl-Ubuntu-root-probe-repo", true, now, 30*24*time.Hour)

	got := DetectUNCProjectDirs(usersRoot, "Ubuntu", "/root/probe-repo", now)
	if len(got) != 3 {
		t.Fatalf("matches = %v, want the three fresh-evidence wsl-spelling dirs", got)
	}
	for _, m := range got {
		base := filepath.Base(m)
		switch base {
		case "wsl-Ubuntu-root-probe-repo", "wsl-localhost-Ubuntu-root-probe-repo", "wsl-ubuntu-root-probe-repo":
		default:
			t.Errorf("unexpected match %q", m)
		}
	}

	if got := DetectUNCProjectDirs(usersRoot, "", "/root/probe-repo", now); got != nil {
		t.Errorf("empty distro must detect nothing, got %v", got)
	}
	if got := DetectUNCProjectDirs(filepath.Join(usersRoot, "missing"), "Ubuntu", "/root/probe-repo", now); got != nil {
		t.Errorf("missing users root must detect nothing, got %v", got)
	}
}

// TestDetectUNCProjectDirs_VersionedDistro pins that the distro name is
// sanitized the same way the repo path is: "Ubuntu-22.04" must match the
// "Ubuntu-22-04" fragment Cursor's own UNC-path transform produces, not the
// raw "Ubuntu-22.04" string.
func TestDetectUNCProjectDirs_VersionedDistro(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	usersRoot := t.TempDir()

	mkProjectDir(t, usersRoot, "peyton", "wsl-Ubuntu-22-04-root-probe-repo", true, now, time.Hour)

	got := DetectUNCProjectDirs(usersRoot, "Ubuntu-22.04", "/root/probe-repo", now)
	if len(got) != 1 {
		t.Fatalf("matches = %v, want the one versioned-distro dir", got)
	}
	if filepath.Base(got[0]) != "wsl-Ubuntu-22-04-root-probe-repo" {
		t.Errorf("unexpected match %q", got[0])
	}
}
