package execx

import (
	"os"
	"path/filepath"
)

// PathScanDirs returns the $PATH entries that may be scanned for executables:
// absolute directories only, in $PATH order.
//
// Every scanner in the CLI must go through this rather than splitting $PATH
// itself. A relative entry resolves against the process's working directory,
// which for a git hook or an agent-invoked command is whatever directory the
// caller happened to be in — usually a repository someone else wrote. A file
// committed to that repository would then be a binary Entire executes.
//
// Go's own resolver already refuses the worst case: exec.LookPath returns
// ErrDot for a match found through a "." entry, and exec.Command re-checks a
// separator-free Path. Neither protection reaches a scanner that resolves by
// filepath.Glob, because a globbed match arrives as a path WITH separators and
// never passes through LookPath at all. Dropping the entry at the source is
// what makes the rule hold for every scanner, whatever it resolves with.
//
// Empty entries are dropped for the same reason: POSIX reads "" as the current
// directory, so it is the "." case spelled differently.
func PathScanDirs() []string {
	entries := filepath.SplitList(os.Getenv("PATH"))
	dirs := make([]string, 0, len(entries))
	for _, dir := range entries {
		if !filepath.IsAbs(dir) {
			continue
		}
		dirs = append(dirs, dir)
	}
	return dirs
}
