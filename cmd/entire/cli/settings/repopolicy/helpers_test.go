package repopolicy

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func newPolicyRepo(t *testing.T) (string, Repository) {
	t.Helper()
	root := t.TempDir()
	initPolicyRepo(t, root)
	writePolicyFile(t, root, "README.md", "test\n")
	runPolicyGit(t, root, "add", "README.md")
	runPolicyGit(t, root, "commit", "--no-gpg-sign", "-m", "initial")
	repository, err := ResolveRepositoryAt(t.Context(), root)
	if err != nil {
		t.Fatalf("ResolveRepositoryAt: %v", err)
	}
	return root, repository
}

func policyAt(t *testing.T, root string) RepoPolicy {
	t.Helper()
	t.Chdir(root)
	policy, err := ClassifyRepoPolicy(t.Context())
	if err != nil {
		t.Fatalf("ClassifyRepoPolicy: %v", err)
	}
	return policy
}

func setPolicyGlobal(t *testing.T, body string) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	if body != "" {
		writePolicyFile(t, configDir, UserSettingsFileName, body)
	}
}

func runPolicyGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func initPolicyRepo(t *testing.T, root string) {
	t.Helper()
	runPolicyGit(t, root, "init")
	runPolicyGit(t, root, "config", "user.name", "Entire Test")
	runPolicyGit(t, root, "config", "user.email", "test@entire.invalid")
	runPolicyGit(t, root, "config", "commit.gpgsign", "false")
}

func writePolicyFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
