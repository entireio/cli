package checkpoint

import (
	"fmt"
	"strconv"
	"strings"

	"entire.io/cli/cmd/entire/cli/paths"
)

// ShadowBranchNameForCommitWithSuffix returns the shadow branch name for a base commit
// with a numeric suffix. This is the new format for shadow branches that provides
// isolation between sessions or dismissed work.
// Format: entire/<hash[:7]>-<suffix>
// Example: entire/abc1234-1, entire/abc1234-2
func ShadowBranchNameForCommitWithSuffix(baseCommit string, suffix int) string {
	base := ShadowBranchNameForCommit(baseCommit)
	return fmt.Sprintf("%s-%d", base, suffix)
}

// ParseShadowBranchName parses a shadow branch name and returns the base commit hash
// and suffix. Returns ok=false if the branch is not a shadow branch.
//
// Handles both formats:
//   - Legacy: "entire/<hash>" -> (hash, 0, true)
//   - Suffixed: "entire/<hash>-N" -> (hash, N, true)
//
// Returns ok=false for:
//   - Non-shadow branches (e.g., "main", "feature/test")
//   - The sessions branch (e.g., "entire/sessions")
func ParseShadowBranchName(branchName string) (baseCommit string, suffix int, ok bool) {
	// Must start with the shadow branch prefix
	if !strings.HasPrefix(branchName, ShadowBranchPrefix) {
		return "", 0, false
	}

	// Extract the part after "entire/"
	rest := strings.TrimPrefix(branchName, ShadowBranchPrefix)

	// Skip the sessions branch
	if rest == "sessions" || rest == strings.TrimPrefix(paths.MetadataBranchName, ShadowBranchPrefix) {
		return "", 0, false
	}

	// Check for suffix format: <hash>-<number>
	lastDash := strings.LastIndex(rest, "-")
	if lastDash > 0 && lastDash < len(rest)-1 {
		possibleSuffix := rest[lastDash+1:]
		// Try to parse as number
		if num, err := strconv.Atoi(possibleSuffix); err == nil && num > 0 {
			// Valid suffix format
			return rest[:lastDash], num, true
		}
	}

	// Legacy format (no suffix or invalid suffix format)
	return rest, 0, true
}

// ListShadowBranchNamesForCommit returns a list of all possible shadow branch names
// for a given base commit, including the legacy format and suffixed formats up to maxSuffix.
// This is useful for finding all shadow branches associated with a base commit.
func ListShadowBranchNamesForCommit(baseCommit string, maxSuffix int) []string {
	names := make([]string, 0, maxSuffix+1)

	// Include legacy format first
	names = append(names, ShadowBranchNameForCommit(baseCommit))

	// Add suffixed formats
	for i := 1; i <= maxSuffix; i++ {
		names = append(names, ShadowBranchNameForCommitWithSuffix(baseCommit, i))
	}

	return names
}
