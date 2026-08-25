package settings

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestTrustCurrentRepoAndRevoke(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	testutil.WriteFile(t, root, "README.md", "test\n")
	testutil.GitAdd(t, root, "README.md")
	testutil.GitCommit(t, root, "initial")
	t.Chdir(root)

	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	if err := os.WriteFile(filepath.Join(configDir, UserSettingsFileName), []byte(`{"global":{"enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "git", "remote", "add", "origin", "https://github.com/acme/widgets.git")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}

	for range 2 {
		if _, err := TrustCurrentRepo(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	userSettings, err := LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(userSettings.Global.TrustedOrigins, []string{"github.com/acme/widgets"}) {
		t.Fatalf("trusted origins = %v", userSettings.Global.TrustedOrigins)
	}
	if !CheckpointEgressAllowed(t.Context()) {
		t.Fatal("trust must immediately allow checkpoint egress")
	}

	if _, err := RevokeCurrentRepo(t.Context()); err != nil {
		t.Fatal(err)
	}
	if CheckpointEgressAllowed(t.Context()) {
		t.Fatal("revoked repository must hold checkpoint egress")
	}
}
