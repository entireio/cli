//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// opfAttackPayload writes a marker and exits non-zero. Exiting non-zero is
// deliberate: a real OPF binary emits span JSON, so a failure here proves the
// process ran without needing to fake that protocol.
func opfAttackPayload(marker string) string {
	return "#!/bin/sh\ntouch " + marker + "\nexit 1\n"
}

// opfSettingsBlock enables OPF with an explicit command, auto-running at
// pre-push (prompt_default "always" also matches the non-TTY auto-run path
// these tests run under).
func opfSettingsBlock(command string) map[string]any {
	return map[string]any{
		"openai_privacy_filter": map[string]any{
			"enabled":        true,
			"prompt_default": "always",
			"categories":     map[string]any{"private_person": true},
			"command":        command,
		},
	}
}

// setupOPFAttack stages the attacker's payload inside the repo and returns the
// marker path (outside the repo, so committing the repo cannot include it).
func setupOPFAttack(t *testing.T, env *TestEnv) (marker, command string) {
	t.Helper()
	marker = filepath.Join(t.TempDir(), "PWNED")
	command = "./.entire/opf"
	payload := filepath.Join(env.RepoDir, ".entire", "opf")
	if err := os.WriteFile(payload, []byte(opfAttackPayload(marker)), 0o755); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	return marker, command
}

// A pull request can carry both a payload and a .entire/settings.json naming
// it, because that file is version-controlled. Pushing must not execute it.
//
// This test also exercises a path no unit test reaches: the real binary builds
// its logger, resolving the log level through settings.Load. When this gate
// logged from inside the loader, that re-entered the logger's non-reentrant
// RWMutex and hung every hook. The level is now resolved in the cli package
// before the logger exists, so the logging package never calls out while
// holding a lock — but this test is what surfaced the hang, and a recurrence
// still shows up here as a timeout.
//
// The push is expected to FAIL: with the command correctly ignored, OPF falls
// back to resolving "opf" on $PATH, which is absent here, and the pre-push
// rewrite fails closed rather than pushing content OPF never scanned. What
// matters is which binary was reached — asserted via the marker.
func TestOPFCommandTrust_CommittedCommandIsNotExecuted(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()

	marker, command := setupOPFAttack(t, env)
	env.PatchSettings(map[string]any{"redaction": opfSettingsBlock(command)})

	// The PR shape: both files committed.
	env.GitAdd(".entire/settings.json", ".entire/opf")
	env.GitCommit("Adjust redaction settings")

	_ = createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")

	err := env.GitPushWithHooksAllowError("origin", "HEAD")

	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("payload from a committed settings.json was EXECUTED during push")
	}
	if err == nil {
		return // OPF resolved some other way; the security property still held.
	}
	if out := err.Error(); strings.Contains(out, ".entire/opf") && !strings.Contains(out, "\"opf\"") {
		t.Errorf("push failure should name the $PATH fallback, not the attacker command: %v", err)
	}
}

// Positive control for the test above: the SAME payload and command, reached
// through an untracked .entire/settings.local.json, DOES run. Without this the
// negative test could pass vacuously — OPF never invoked at all would look
// identical to OPF invoking a safe binary.
func TestOPFCommandTrust_UntrackedLocalCommandIsExecuted(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()

	marker, command := setupOPFAttack(t, env)

	// Not committed, and .entire/.gitignore already excludes it: this is the
	// developer's own machine-local choice.
	env.WriteFile(".entire/settings.local.json",
		`{"redaction":{"openai_privacy_filter":{"enabled":true,"prompt_default":"always",`+
			`"categories":{"private_person":true},"command":"`+command+`"}}}`)

	env.GitAdd(".entire/opf")
	env.GitCommit("Add local opf shim")

	_ = createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")

	// The push is expected to fail: the payload exits non-zero, so OPF fails
	// closed. Reaching the binary at all is the point here.
	if pushErr := env.GitPushWithHooksAllowError("origin", "HEAD"); pushErr == nil {
		t.Log("push succeeded; OPF still resolved the local command")
	}

	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatal("a developer-owned local command must still be honored; " +
			"if this fails the negative test above proves nothing")
	}
}

// The filename is not the boundary. Committing .entire/settings.local.json
// (which .gitignore does not prevent once tracked) delivers the same payload
// through a pull request, so the whole layer must be ignored.
func TestOPFCommandTrust_CommittedLocalFileIsNotExecuted(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()

	marker, command := setupOPFAttack(t, env)
	env.WriteFile(".entire/settings.local.json",
		`{"redaction":{"openai_privacy_filter":{"enabled":true,"prompt_default":"always",`+
			`"categories":{"private_person":true},"command":"`+command+`"}}}`)

	// Force-add past .entire/.gitignore, exactly as an attacker would.
	testutil.GitAddForce(t, env.RepoDir, ".entire/settings.local.json", ".entire/opf")
	env.GitCommit("Add local settings")

	_ = createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")

	if pushErr := env.GitPushWithHooksAllowError("origin", "HEAD"); pushErr == nil {
		t.Log("push succeeded")
	}

	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("payload from a COMMITTED settings.local.json was EXECUTED during push")
	}
}
