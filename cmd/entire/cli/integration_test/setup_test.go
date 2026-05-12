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

	moduleRoot := findModuleRoot()
	// -tags=authfilestore enables the file-backed auth store backend so the
	// subprocess can opt into ENTIRE_TEST_AUTH_STORE_FILE instead of touching
	// the real OS keychain. Production builds omit this tag.
	buildCmd := exec.Command("go", "build", "-tags=authfilestore", "-o", testBinaryPath, ".")
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
