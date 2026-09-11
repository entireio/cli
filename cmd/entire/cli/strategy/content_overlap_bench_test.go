package strategy

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/benchutil"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// BenchmarkWorktreeMatchesCommitted separates the usual filtered-hash match
// from the legacy CRLF exception. Repo setup and commits are outside the timer.
func BenchmarkWorktreeMatchesCommitted(b *testing.B) {
	for _, legacy := range []bool{false, true} {
		for _, count := range []int{1, 100} {
			b.Run(fmt.Sprintf("Legacy_%t/Files_%d", legacy, count), func(b *testing.B) {
				br := benchutil.NewBenchRepo(b, benchutil.RepoOpts{FileCount: count})
				runGit := func(args ...string) {
					b.Helper()
					cmd := exec.CommandContext(b.Context(), "git", args...)
					cmd.Dir = br.Dir
					cmd.Env = gitrepo.EnvWithoutRepoOverrides()
					out, err := cmd.CombinedOutput()
					require.NoError(b, err, "%s", out)
				}
				runGit("config", "core.autocrlf", "true")
				for i := range count {
					br.WriteFile(b, fmt.Sprintf("src/file_%03d.go", i),
						strings.ReplaceAll(benchutil.GenerateGoFile(i, 100), "\n", "\r\n"))
				}
				if legacy {
					runGit("-c", "core.autocrlf=false", "add", "--", "src")
					runGit("-c", "user.name=Benchmark", "-c", "user.email=bench@example.com",
						"-c", "core.hooksPath=/dev/null", "commit", "--no-gpg-sign", "-m", "CRLF blobs")
				}
				head, err := br.Repo.Head()
				require.NoError(b, err)
				commit, err := br.Repo.CommitObject(head.Hash())
				require.NoError(b, err)
				files := make(map[string]*object.File, count)
				for i := range count {
					path := fmt.Sprintf("src/file_%03d.go", i)
					files[path], err = commit.File(path)
					require.NoError(b, err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					matches := WorktreeMatchesCommitted(b.Context(), br.Dir, files)
					for path := range files {
						if !matches[path] {
							b.Fatalf("%s should match the commit", path)
						}
					}
				}
			})
		}
	}
}
