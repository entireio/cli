//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMain builds the CLI binary once before running all tests.
func TestMain(m *testing.M) {
	// Build binary once to a temp directory
	tmpDir, err := os.MkdirTemp("", "entire-integration-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir for binary: %v\n", err)
		os.Exit(1)
	}

	testBinaryPath = filepath.Join(tmpDir, "entire")

	// Route every spawned CLI away from the developer's real ~/.config/entire
	// (contexts.json, version_check.json), ~/.cache/entire (discovery caches),
	// and OS keychain. testing.Testing() is false in the subprocess, so the
	// internal/testdirs fallback cannot protect it — isolation must come from
	// the environment, which children inherit because all integration env
	// building starts from os.Environ() (testutil.GitIsolatedEnv).
	isolation := map[string]string{
		"ENTIRE_CONFIG_DIR":           filepath.Join(tmpDir, "entire-config"),
		"XDG_CACHE_HOME":              filepath.Join(tmpDir, "entire-cache"),
		"ENTIRE_TOKEN_STORE":          "file",
		"ENTIRE_TOKEN_STORE_PATH":     filepath.Join(tmpDir, "entire-tokens.json"),
		"ENTIRE_TEST_AUTH_STORE_FILE": filepath.Join(tmpDir, "entire-auth-tokens.json"),
	}
	for k, v := range isolation {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "failed to set %s: %v\n", k, err)
			os.RemoveAll(tmpDir)
			os.Exit(1)
		}
	}

	moduleRoot := findModuleRoot()
	buildCmd := exec.Command("go", "build", "-o", testBinaryPath, ".")
	buildCmd.Dir = filepath.Join(moduleRoot, "cmd", "entire")

	buildOutput, err := buildCmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build CLI binary: %v\nOutput: %s\n", err, buildOutput)
		os.RemoveAll(tmpDir)
		os.Exit(1)
	}

	// Run tests
	code := m.Run()

	// Cleanup
	os.RemoveAll(tmpDir)
	os.Exit(code)
}
