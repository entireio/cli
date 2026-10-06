package gitrepo

import "strings"

// WorktreeBranch is one worktree from `git worktree list --porcelain` that has
// a branch checked out.
type WorktreeBranch struct {
	// Path is the worktree path exactly as git printed it.
	Path string
	// Branch is the short branch name (refs/heads/ stripped).
	Branch string
}

// ParseWorktreeBranches returns the worktrees in `git worktree list
// --porcelain` output that have a branch checked out, in output order.
//
// Each worktree is a block beginning with a `worktree <path>` line and
// separated by a blank line; a `branch <ref>` line only appears for
// non-detached worktrees. The path is reset at each block boundary and a
// branch line is only paired with a worktree line from the same block, so a
// detached worktree (no branch line) can never pair a branch with a stale path
// or report an empty one.
func ParseWorktreeBranches(porcelain string) []WorktreeBranch {
	var out []WorktreeBranch
	var curPath string
	for _, line := range strings.Split(porcelain, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case line == "":
			curPath = "" // block boundary
		case strings.HasPrefix(line, "worktree "):
			curPath = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch ") && curPath != "":
			ref := strings.TrimPrefix(line, "branch ")
			out = append(out, WorktreeBranch{Path: curPath, Branch: strings.TrimPrefix(ref, "refs/heads/")})
		}
	}
	return out
}
