package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestPrepareHookPolicyMigratesVerifiedLegacyActivation(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	testutil.WriteFile(t, root, "README.md", "test\n")
	testutil.GitAdd(t, root, "README.md")
	testutil.GitCommit(t, root, "initial")
	testutil.WriteFile(t, root, ".entire/settings.json", `{"enabled":true}`)
	t.Chdir(root)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

	repository, err := repopolicy.ResolveRepository(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(repository.GitCommonDir, "entire-sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	record := `{"phase":"active","worktree_path":` + strconv.Quote(repository.WorktreeRoot) + `,"worktree_id":` + strconv.Quote(repository.WorktreeID) + `}`
	if err := os.WriteFile(filepath.Join(sessions, "legacy.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}

	_, policy, err := prepareHookPolicy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Active || policy.ActivationSource != repopolicy.ActivationLocal {
		t.Fatalf("policy = %+v, want migrated local activation", policy)
	}
}
