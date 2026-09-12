package paths

import "github.com/entireio/cli/cmd/entire/cli/internal/worktreeid"

// WorktreeIDHashLength is the number of hex characters HashWorktreeID
// returns. checkpoint.WorktreeIDHashLength aliases it for package locality;
// that mirror currently has no consumers of its own — nothing parses shadow
// branch names by hash length.
const WorktreeIDHashLength = worktreeid.HashLength

// HashWorktreeID returns a short stable hash of a worktree identifier (a
// GetWorktreeID result; "" for the main worktree). It is the per-worktree
// namespace key used both in shadow branch names and invisible runtime paths.
func HashWorktreeID(worktreeID string) string {
	return worktreeid.Hash(worktreeID)
}

// GetWorktreeID returns the internal Git worktree identifier for path. The
// implementation is shared with repository policy through a cycle-safe leaf.
func GetWorktreeID(worktreePath string) (string, error) {
	return worktreeid.Get(worktreePath) //nolint:wrapcheck // compatibility facade preserves the established error contract
}
