package gitrepo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseWorktreeBranches(t *testing.T) {
	t.Parallel()
	porcelain := "worktree /repo/main\nHEAD 1111111111111111111111111111111111111111\nbranch refs/heads/main\n\n" +
		"worktree /repo/wt-detached\nHEAD 2222222222222222222222222222222222222222\ndetached\n\n" +
		"worktree /repo/wt-feat\r\nHEAD 3333333333333333333333333333333333333333\r\nbranch refs/heads/feat/x\r\n\r\n" +
		"branch refs/heads/orphan-without-worktree-line\n"

	assert.Equal(t, []WorktreeBranch{
		{Path: "/repo/main", Branch: "main"},
		{Path: "/repo/wt-feat", Branch: "feat/x"},
	}, ParseWorktreeBranches(porcelain))
	assert.Empty(t, ParseWorktreeBranches(""))
}

func TestParseWorktreePaths(t *testing.T) {
	t.Parallel()
	porcelain := "worktree /repo/bare.git\nbare\n\n" +
		"worktree /repo/main\nHEAD 1111111111111111111111111111111111111111\nbranch refs/heads/main\n\n" +
		"worktree /repo/wt-detached\nHEAD 2222222222222222222222222222222222222222\ndetached\n\n" +
		"worktree /repo/wt-feat\r\nHEAD 3333333333333333333333333333333333333333\r\nbranch refs/heads/feat/x\r\n" +
		"worktree /repo/wt-prunable\nHEAD 4444444444444444444444444444444444444444\ndetached\nprunable gitdir file points to non-existent location\n"

	assert.Equal(t, []string{"/repo/main", "/repo/wt-detached", "/repo/wt-feat", "/repo/wt-prunable"}, ParseWorktreePaths(porcelain))
	assert.Empty(t, ParseWorktreePaths(""))
}
