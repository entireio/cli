package cursor

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// uncEvidenceWindow bounds how old a project dir's most recent agent
// activity may be and still count as evidence of a live UNC-mode session.
// Without a recency bound, a fingerprint left over from before the user
// switched to WSL-remote mode would warn forever.
const uncEvidenceWindow = 14 * 24 * time.Hour

// DetectUNCProjectDirs finds Windows-side Cursor project directories showing
// that Cursor IDE opened this WSL repo via a \\wsl$-style UNC path — a mode in
// which Cursor executes no hooks, so Entire tracking silently never starts.
// usersRoot is the Windows user-profile root as seen from WSL (/mnt/c/Users);
// distro is the WSL distro name (WSL_DISTRO_NAME); now is the reference time
// evidence recency is measured against (production callers pass time.Now()).
// A directory counts only when its name exactly matches (case-insensitively)
// one of the two observed UNC spellings ("wsl-<distro>-<sanitized>" for
// \\wsl$, "wsl-localhost-<distro>-<sanitized>" for \\wsl.localhost) and its
// agent-transcripts subdirectory has at least one entry modified within
// uncEvidenceWindow of now — browsing alone never created that directory in
// testing; an agent session always did, and a stale one shouldn't warn
// forever. Returns nil when nothing matches or the environment doesn't apply.
//
// Every profile under usersRoot is scanned because the WSL username need not
// match the Windows one. Known gaps (silent false negatives): assumes
// Windows is on C: with the default WSL automount; drive-letter mappings of
// \\wsl$ (rather than the default automount path) are missed.
func DetectUNCProjectDirs(usersRoot, distro, repoRoot string, now time.Time) []string {
	if distro == "" {
		return nil
	}
	names := UNCProjectDirNames(distro, repoRoot)
	users, err := os.ReadDir(usersRoot)
	if err != nil {
		return nil
	}
	var out []string
	for _, u := range users {
		// IsDir also skips profile-root junction symlinks, keeping the walk loop-safe.
		if !u.IsDir() {
			continue
		}
		projects := filepath.Join(usersRoot, u.Name(), ".cursor", "projects")
		entries, err := os.ReadDir(projects)
		if err != nil {
			continue
		}
		for _, e := range entries {
			// Exact names only (case-insensitive): the two observed UNC spellings.
			n := e.Name()
			if !e.IsDir() || (!strings.EqualFold(n, names[0]) && !strings.EqualFold(n, names[1])) {
				continue
			}
			dir := filepath.Join(projects, e.Name())
			if hasRecentEvidence(filepath.Join(dir, "agent-transcripts"), now) {
				out = append(out, dir)
			}
		}
	}
	return out
}

// UNCProjectDirNames returns the Windows-side Cursor project-directory names
// this repo produces when opened over UNC — one per observed spelling.
func UNCProjectDirNames(distro, repoRoot string) []string {
	suffix := sanitizePathForCursor(distro) + "-" + sanitizePathForCursor(repoRoot)
	return []string{"wsl-" + suffix, "wsl-localhost-" + suffix}
}

// hasRecentEvidence reports whether transcriptsDir exists and contains at
// least one entry modified within uncEvidenceWindow of now.
func hasRecentEvidence(transcriptsDir string, now time.Time) bool {
	entries, err := os.ReadDir(transcriptsDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if age := now.Sub(info.ModTime()); age >= 0 && age <= uncEvidenceWindow {
			return true
		}
	}
	return false
}
