package settings

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
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

	revoked, err := RevokeCurrentRepo(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(revoked.OriginKeys, []string{"github.com/acme/widgets"}) {
		t.Fatalf("revoked identity = %+v, want the origin key that was removed", revoked)
	}
	if CheckpointEgressAllowed(t.Context()) {
		t.Fatal("revoked repository must hold checkpoint egress")
	}
}

// A revoke that cannot resolve the repo's identity must fail loud: with no
// origin keys the TrustedOrigins delete is a no-op, and a "✓ Revoked" over it
// would leave live trust the user believes is gone.
func TestRevokeCurrentRepo_IdentityErrorFailsLoud(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	testutil.WriteFile(t, root, "README.md", "test\n")
	testutil.GitAdd(t, root, "README.md")
	testutil.GitCommit(t, root, "initial")
	testutil.AddRemote(t, root, "origin", "https://github.com/acme/widgets.git")
	t.Chdir(root)
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	if err := os.WriteFile(filepath.Join(configDir, UserSettingsFileName), []byte(`{"global":{"enabled":true,"trusted_origins":["github.com/acme/widgets"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	previous := repopolicy.ResolveSyncRemote
	repopolicy.ResolveSyncRemote = func(context.Context, repopolicy.Repository) (repopolicy.SyncRemote, error) {
		return repopolicy.SyncRemote{}, errors.New("checkpoint_push_remote \"gone\" is not a configured git remote")
	}
	t.Cleanup(func() { repopolicy.ResolveSyncRemote = previous })

	id, err := RevokeCurrentRepo(t.Context())
	if err == nil {
		t.Fatal("revoke must fail when the identity cannot be resolved")
	}
	if id.OriginKeyed() || id.Path != "" {
		t.Fatalf("identity = %+v, want empty on a failed revoke", id)
	}
	userSettings, err := LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(userSettings.Global.TrustedOrigins, []string{"github.com/acme/widgets"}) {
		t.Fatalf("trusted origins = %v, want untouched on a failed revoke", userSettings.Global.TrustedOrigins)
	}
}
