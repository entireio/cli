package cli

import (
	"context"
	"fmt"
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

// Native --branch expands checkout history. The library's literal validator
// cannot replace this repository-dependent behavior.
func TestValidateBranchName_PreviousCheckout(t *testing.T) {
	gitenv.IsolateRepository(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	testutil.RunGit(t, dir, "commit", "--allow-empty", "--no-gpg-sign", "-m", "initial")
	testutil.RunGit(t, dir, "checkout", "-b", "other")
	require.NoError(t, ValidateBranchName(t.Context(), "@{-1}"))
	for _, name := range []string{
		"@{-1}", "@{-2}", "@{-0}", "@{-1}/topic", "@{-1}.lock", "@{-1}..topic",
		"@{upstream}", "other@{upstream}", "@{push}", "other@{0}", "@{-", "a@{b",
	} {
		oracle := execx.NonInteractive(t.Context(), "git", "check-ref-format", "--branch", name)
		oracle.Env = testutil.GitIsolatedEnv()
		require.Equal(t, oracle.Run() == nil, ValidateBranchName(t.Context(), name) == nil, "expression %q", name)
	}
}

func TestValidateBranchName_LiteralsDoNotNeedGit(t *testing.T) {
	gitenv.IsolateRepository(t)
	t.Chdir(t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{"main", "@", "topic/@/name", "feature/nested", "refs/heads/HEAD", "HEAD/topic"} {
		require.NoError(t, ValidateBranchName(t.Context(), name), "literal %q must not start Git", name)
	}
	for _, name := range []string{"HEAD", "-topic", "a..b", "a.lock/b", "a\x00b"} {
		require.EqualError(t, ValidateBranchName(t.Context(), name), fmt.Sprintf("invalid branch name %q", name))
	}
	// Expressions still require native Git; absence is not a reason to accept
	// unresolved syntax as a literal branch name.
	require.Error(t, ValidateBranchName(t.Context(), "@{-1}"))
}

func TestValidateBranchName_Canceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, name := range []string{"main", "-topic", "@{-1}"} {
		require.EqualError(t, ValidateBranchName(ctx, name), fmt.Sprintf("invalid branch name %q", name))
	}
}

func TestValidateBranchName_ASCIIParity(t *testing.T) {
	gitenv.IsolateRepository(t)
	t.Chdir(t.TempDir())
	for char := range 128 {
		for _, name := range []string{string(rune(char)) + "topic", "to" + string(rune(char)) + "pic", "topic" + string(rune(char))} {
			oracle := execx.NonInteractive(t.Context(), "git", "check-ref-format", "--branch", name)
			oracle.Env = testutil.GitIsolatedEnv()
			require.Equal(t, oracle.Run() == nil, ValidateBranchName(t.Context(), name) == nil, "literal %q", name)
		}
	}
}
