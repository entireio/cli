//go:build e2e

package e2e

import (
	"regexp"
	"strings"
	"testing"
)

// AssertFileContains checks that a file contains the expected substring.
func AssertFileContains(t *testing.T, env *TestEnv, path, expected string) {
	t.Helper()

	content := env.ReadFile(path)
	if !strings.Contains(content, expected) {
		t.Errorf("File %s does not contain expected string.\nExpected substring: %q\nActual content:\n%s",
			path, expected, content)
	}
}

// AssertFileMatches checks that a file content matches a regex pattern.
func AssertFileMatches(t *testing.T, env *TestEnv, path, pattern string) {
	t.Helper()

	content := env.ReadFile(path)
	matched, err := regexp.MatchString(pattern, content)
	if err != nil {
		t.Fatalf("Invalid regex pattern %q: %v", pattern, err)
	}
	if !matched {
		t.Errorf("File %s does not match pattern.\nPattern: %q\nActual content:\n%s",
			path, pattern, content)
	}
}

// AssertHelloWorldProgram verifies a Go file is a valid hello world program.
// Uses flexible matching to handle agent variations.
func AssertHelloWorldProgram(t *testing.T, env *TestEnv, path string) {
	t.Helper()

	content := env.ReadFile(path)

	// Check for package main
	if !strings.Contains(content, "package main") {
		t.Errorf("File %s missing 'package main'", path)
	}

	// Check for main function (flexible whitespace)
	mainFuncPattern := regexp.MustCompile(`func\s+main\s*\(\s*\)`)
	if !mainFuncPattern.MatchString(content) {
		t.Errorf("File %s missing main function", path)
	}

	// Check for Hello, World! (case insensitive)
	helloPattern := regexp.MustCompile(`(?i)hello.+world`)
	if !helloPattern.MatchString(content) {
		t.Errorf("File %s missing 'Hello, World!' output", path)
	}
}

// AssertCalculatorFunctions verifies calc.go has the expected functions.
func AssertCalculatorFunctions(t *testing.T, env *TestEnv, path string, functions ...string) {
	t.Helper()

	content := env.ReadFile(path)

	for _, fn := range functions {
		// Check for function definition (flexible whitespace)
		pattern := regexp.MustCompile(`func\s+` + regexp.QuoteMeta(fn) + `\s*\(`)
		if !pattern.MatchString(content) {
			t.Errorf("File %s missing function %s", path, fn)
		}
	}
}

// AssertRewindPointExists checks that at least one rewind point exists.
func AssertRewindPointExists(t *testing.T, env *TestEnv) {
	t.Helper()

	points := env.GetRewindPoints()
	if len(points) == 0 {
		t.Error("Expected at least one rewind point, but none exist")
	}
}

// AssertRewindPointCount checks that the expected number of rewind points exist.
func AssertRewindPointCount(t *testing.T, env *TestEnv, expected int) {
	t.Helper()

	points := env.GetRewindPoints()
	if len(points) != expected {
		t.Errorf("Expected %d rewind points, got %d", expected, len(points))
		for i, p := range points {
			t.Logf("  Point %d: ID=%s, Message=%s", i, p.ID, p.Message)
		}
	}
}

// AssertRewindPointCountAtLeast checks that at least the expected number of rewind points exist.
func AssertRewindPointCountAtLeast(t *testing.T, env *TestEnv, minimum int) {
	t.Helper()

	points := env.GetRewindPoints()
	if len(points) < minimum {
		t.Errorf("Expected at least %d rewind points, got %d", minimum, len(points))
		for i, p := range points {
			t.Logf("  Point %d: ID=%s, Message=%s", i, p.ID, p.Message)
		}
	}
}

// AssertCheckpointExists checks that a checkpoint trailer exists in commit history.
func AssertCheckpointExists(t *testing.T, env *TestEnv) {
	t.Helper()

	checkpointID := env.GetLatestCheckpointIDFromHistory()
	if checkpointID == "" {
		t.Error("Expected checkpoint trailer in commit history, but none found")
	}
}

// AssertBranchExists checks that a branch exists.
func AssertBranchExists(t *testing.T, env *TestEnv, branchName string) {
	t.Helper()

	if !env.BranchExists(branchName) {
		t.Errorf("Expected branch %s to exist, but it doesn't", branchName)
	}
}

// AssertAgentSuccess checks that an agent result indicates success.
func AssertAgentSuccess(t *testing.T, result *AgentResult, err error) {
	t.Helper()

	if err != nil {
		stderr := ""
		if result != nil {
			stderr = result.Stderr
		}
		t.Errorf("Agent failed with error: %v\nStderr: %s", err, stderr)
	}
	if result != nil && result.ExitCode != 0 {
		t.Errorf("Agent exited with code %d\nStdout: %s\nStderr: %s",
			result.ExitCode, result.Stdout, result.Stderr)
	}
}

// AssertExpectedFilesExist checks that all expected files from a prompt template exist.
func AssertExpectedFilesExist(t *testing.T, env *TestEnv, prompt PromptTemplate) {
	t.Helper()

	for _, file := range prompt.ExpectedFiles {
		if !env.FileExists(file) {
			t.Errorf("Expected file %s to exist after prompt %s, but it doesn't", file, prompt.Name)
		}
	}
}
