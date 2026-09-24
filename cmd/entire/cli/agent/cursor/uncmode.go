package cursor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// UNCEvidenceWindow bounds how old evidence may be and still count as fresh.
const UNCEvidenceWindow = 14 * 24 * time.Hour

// DetectUNCProjectDirs finds Windows-side Cursor project directories showing
// that Cursor IDE opened this WSL repo via a \\wsl$-style UNC path, a mode
// with zero hooks. usersRoot is the Windows user-profile root as seen from
// WSL; distro is WSL_DISTRO_NAME; now is the reference time for evidence
// recency (production callers pass time.Now()). Matches a name against
// UNCProjectDirNames (case-insensitive) with a recent agent-transcripts
// entry; nil when nothing applies.
//
// Known gaps (silent false negatives): drive-letter mappings of \\wsl$;
// colliding sanitized names of different paths (inherited from Cursor's
// naming); every Windows profile is scanned because the WSL username need
// not match the Windows one.
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
		// Skips NTFS junction stubs (All Users, Default User), which surface as symlinks under /mnt/c.
		if !u.IsDir() {
			continue
		}
		projects := filepath.Join(usersRoot, u.Name(), ".cursor", "projects")
		entries, err := os.ReadDir(projects)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if !e.IsDir() || !slices.ContainsFunc(names, func(s string) bool { return strings.EqualFold(n, s) }) {
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
// least one entry modified within UNCEvidenceWindow of now.
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
		// Tolerate bounded skew between the Windows filesystem clock and the WSL
		// clock (WSL2 lags the host after sleep); far-future bogus mtimes still
		// never count.
		if age := now.Sub(info.ModTime()); age >= -24*time.Hour && age <= UNCEvidenceWindow {
			return true
		}
	}
	return false
}
