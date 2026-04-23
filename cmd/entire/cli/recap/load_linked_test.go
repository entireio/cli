package recap

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestLookupLinkedCommits_ByTrailer(t *testing.T) {
	repo := newIsolatedRepo(t)
	// Create two commits: one linked, one not.
	testutil.WriteFile(t, repo, "a.txt", "a")
	testutil.GitAdd(t, repo, "a.txt")
	testutil.GitCommitWithMsg(t, repo, "feat: add a\n\nEntire-Checkpoint: aa11bb22cc33\n")

	testutil.WriteFile(t, repo, "b.txt", "b")
	testutil.GitAdd(t, repo, "b.txt")
	testutil.GitCommitWithMsg(t, repo, "feat: add b\n")

	m := LookupLinkedCommits(ctx(), []string{"aa11bb22cc33", "ff99ee88"})
	if len(m["aa11bb22cc33"]) != 1 {
		t.Errorf("expected one SHA for aa11bb22cc33, got %v", m["aa11bb22cc33"])
	}
	if got := m["aa11bb22cc33"][0]; len(got) < 7 {
		t.Errorf("expected SHA of at least 7 chars, got %q", got)
	}
	if len(m["ff99ee88"]) != 0 {
		t.Errorf("expected no SHAs for ff99ee88, got %v", m["ff99ee88"])
	}
}
