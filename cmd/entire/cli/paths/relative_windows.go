//go:build windows

package paths

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// msysDrivePrefix matches MSYS/Git-Bash-style absolute paths like /c/ or /D/.
// Git for Windows executes hooks through MSYS2 bash, which converts Windows paths
// (C:\Users\...) to Unix-style (/c/Users/...) in tool output and transcripts.
var msysDrivePrefix = regexp.MustCompile(`^/([a-zA-Z])/`)

// NormalizeMSYSPath converts MSYS/Git-Bash paths to Windows paths.
// Handles two MSYS conventions:
//   - Drive paths: /c/Users/... → C:/Users/...
//   - Virtual dirs: /tmp/... → <TEMP>/... (MSYS2 maps /tmp to the Windows temp dir)
//
// Returns the input unchanged if the path doesn't match any known MSYS pattern.
func NormalizeMSYSPath(p string) string {
	if m := msysDrivePrefix.FindStringSubmatch(p); m != nil {
		return strings.ToUpper(m[1]) + ":/" + p[3:]
	}
	// MSYS2 maps /tmp to the Windows temp directory.
	if strings.HasPrefix(p, "/tmp/") {
		if tmp := os.TempDir(); tmp != "" {
			return filepath.Join(tmp, p[5:])
		}
	}
	return p
}

// ToRelativePath converts an absolute path to relative.
// Returns empty string if the path is outside the working directory.
//
// On Windows, transcript-extracted paths may arrive in Unix formats from
// MSYS2/Git Bash (/c/Users/..., /tmp/...) or agent sandboxes (/home/user/...).
// These are normalized via NormalizeMSYSPath, and any remaining Unix-style
// paths that the OS can't resolve are dropped.
func ToRelativePath(absPath, cwd string) string {
	absPath = NormalizeMSYSPath(absPath)

	// After MSYS normalization, a path starting with "/" that the OS still
	// doesn't recognize as absolute is an unconvertible Unix path (e.g.,
	// /home/user/... from a container/sandbox). Filter it out.
	if strings.HasPrefix(absPath, "/") && !filepath.IsAbs(absPath) {
		return ""
	}

	if !filepath.IsAbs(absPath) {
		return absPath
	}
	relPath, err := filepath.Rel(cwd, absPath)
	if err != nil || strings.HasPrefix(relPath, "..") {
		return ""
	}

	return relPath
}
