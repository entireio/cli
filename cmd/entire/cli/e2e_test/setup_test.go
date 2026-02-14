//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// testBinaryPath holds the path to the CLI binary built once in TestMain.
// All tests share this binary to avoid repeated builds.
var testBinaryPath string

// defaultAgent holds the agent to test with, determined in TestMain.
var defaultAgent string

// TestMain builds the CLI binary once and checks agent availability before running tests.
func TestMain(m *testing.M) {
	// Determine which agent to test with
	defaultAgent = os.Getenv("E2E_AGENT")
	if defaultAgent == "" {
		defaultAgent = AgentNameClaudeCode
	}

	// Check if the agent is available
	runner := NewAgentRunner(defaultAgent, AgentRunnerConfig{})
	available, err := runner.IsAvailable()
	if !available {
		fmt.Printf("Agent %s not available (%v), skipping E2E tests\n", defaultAgent, err)
		// Exit 0 to not fail CI when agent isn't configured
		os.Exit(0)
	}

	// Build binary once to a temp directory
	tmpDir, err := os.MkdirTemp("", "entire-e2e-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir for binary: %v\n", err)
		os.Exit(1)
	}

	testBinaryPath = filepath.Join(tmpDir, "entire")

	moduleRoot := findModuleRoot()
	ctx := context.Background()

	buildCmd := exec.CommandContext(ctx, "go", "build", "-o", testBinaryPath, ".")
	buildCmd.Dir = filepath.Join(moduleRoot, "cmd", "entire")

	buildOutput, err := buildCmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build CLI binary: %v\nOutput: %s\n", err, buildOutput)
		os.RemoveAll(tmpDir)
		os.Exit(1)
	}

	// Add binary to PATH so hooks can find it
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+origPath)

	// Run tests
	code := m.Run()

	// Cleanup
	os.Setenv("PATH", origPath)
	os.RemoveAll(tmpDir)
	os.Exit(code)
}

// getTestBinary returns the path to the shared test binary.
// It panics if TestMain hasn't run (testBinaryPath is empty).
func getTestBinary() string {
	if testBinaryPath == "" {
		panic("testBinaryPath not set - TestMain must run before tests")
	}
	return testBinaryPath
}

// findModuleRoot finds the Go module root by walking up from the current file.
func findModuleRoot() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("failed to get current file path via runtime.Caller")
	}
	dir := filepath.Dir(thisFile)

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("could not find go.mod starting from " + thisFile)
		}
		dir = parent
	}
}
