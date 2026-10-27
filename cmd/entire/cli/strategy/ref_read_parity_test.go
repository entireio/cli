package strategy

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

func TestGitCommitRefExists_PeelingAndAbsence(t *testing.T) {
	gitenv.IsolateRepository(t)
	dir, _, head := initCountTestRepo(t)
	t.Chdir(dir)
	testutil.RunGit(t, dir, "tag", "--no-sign", "-a", "annotated", "-m", "annotated")
	testutil.RunGit(t, dir, "tag", "--no-sign", "-a", "nested", "annotated", "-m", "nested")
	blob := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD:f.txt"))
	tree := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD^{tree}"))
	testutil.RunGit(t, dir, "update-ref", "refs/tags/blob", blob)
	testutil.RunGit(t, dir, "update-ref", "refs/tags/tree", tree)
	testutil.RunGit(t, dir, "symbolic-ref", "refs/heads/alias", "HEAD")
	testutil.RunGit(t, dir, "symbolic-ref", "refs/heads/dangling", "refs/heads/absent")

	for _, packed := range []bool{false, true} {
		if packed {
			testutil.RunGit(t, dir, "pack-refs", "--all")
		}
		for _, tc := range []struct {
			ref    string
			exists bool
		}{
			{"HEAD", true}, {head, true}, {"HEAD~1", true},
			{"refs/heads/alias", true}, {"refs/tags/annotated", true}, {"refs/tags/nested", true},
			{"refs/tags/blob", false}, {"refs/tags/tree", false},
			{"refs/heads/absent", false}, {"refs/heads/dangling", false},
			{strings.Repeat("1", len(head)), false},
		} {
			oracle := execx.NonInteractive(t.Context(), "git", "rev-parse", "--verify", "--quiet", tc.ref+"^{commit}")
			oracle.Dir = dir
			oracle.Env = testutil.GitIsolatedEnv()
			require.Equal(t, tc.exists, oracle.Run() == nil, "native baseline: %s packed=%t", tc.ref, packed)
			require.Equal(t, tc.exists, gitCommitRefExists(t.Context(), tc.ref), "%s packed=%t", tc.ref, packed)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.False(t, gitCommitRefExists(ctx, "HEAD"))
}
