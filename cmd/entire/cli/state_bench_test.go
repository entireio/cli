package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/benchutil"
	"github.com/stretchr/testify/require"
)

func BenchmarkFilterToUncommittedFiles(b *testing.B) {
	for _, crlf := range []bool{false, true} {
		for _, count := range []int{1, 100} {
			b.Run(fmt.Sprintf("CRLF_%t/Files_%d", crlf, count), func(b *testing.B) {
				br := benchutil.NewBenchRepo(b, benchutil.RepoOpts{FileCount: count})
				b.Chdir(br.Dir)
				cfg, err := br.Repo.Config()
				require.NoError(b, err)
				cfg.Core.AutoCRLF = "true"
				require.NoError(b, br.Repo.SetConfig(cfg))
				files := make([]string, count)
				for i := range count {
					files[i] = fmt.Sprintf("src/file_%03d.go", i)
					if crlf {
						br.WriteFile(b, files[i], strings.ReplaceAll(benchutil.GenerateGoFile(i, 100), "\n", "\r\n"))
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if remaining := filterToUncommittedFiles(b.Context(), files, br.Dir); len(remaining) != 0 {
						b.Fatalf("committed files retained: %v", remaining)
					}
				}
			})
		}
	}
}
