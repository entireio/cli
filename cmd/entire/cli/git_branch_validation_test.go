package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

// Pin literal branch-name behavior before replacing check-ref-format. Running
// outside a repository ensures these cases do not depend on checkout history.
func TestValidateBranchName_LiteralParity(t *testing.T) {
	// Subtests change CWD and therefore cannot run in parallel.
	for _, tc := range []struct {
		name  string
		valid bool
	}{
		{"main", true}, {"feature/topic", true}, {"café", true},
		{"refs/heads/HEAD", true}, {"@", true},
		{"", false}, {"HEAD", false}, {"-topic", false}, {"--all", false},
		{"a..b", false}, {"a@{b", false}, {"a b", false},
		{"a\\b", false}, {"a:b", false}, {"a?b", false}, {"a*b", false},
		{"a[b", false}, {"a~b", false}, {"a^b", false},
		{".topic", false}, {"topic.", false}, {"topic.lock", false},
		{"a/.hidden", false}, {"a.lock/b", false},
		{"a//b", false}, {"/topic", false}, {"topic/", false},
		{"a\nb", false}, {"a\tb", false}, {"a\x00b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitenv.IsolateRepository(t)
			t.Chdir(t.TempDir())
			oracle := execx.NonInteractive(t.Context(), "git", "check-ref-format", "--branch", tc.name)
			oracle.Env = testutil.GitIsolatedEnv()
			require.Equal(t, tc.valid, oracle.Run() == nil, "native Git baseline")
			require.Equal(t, tc.valid, ValidateBranchName(t.Context(), tc.name) == nil)
		})
	}
}

// Native --branch expands checkout history. This is intentionally separate
// from literal validation: a future pure validator must make this compatibility
// decision explicitly rather than silently changing it in a mechanical port.
func TestValidateBranchName_PreviousCheckout(t *testing.T) {
	gitenv.IsolateRepository(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	testutil.RunGit(t, dir, "commit", "--allow-empty", "--no-gpg-sign", "-m", "initial")
	testutil.RunGit(t, dir, "checkout", "-b", "other")
	require.NoError(t, ValidateBranchName(t.Context(), "@{-1}"))
}
