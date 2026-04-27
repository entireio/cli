package redact

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBetterleaksDoesNotPoisonGitEnvironment(t *testing.T) {
	t.Parallel()

	// Regression coverage for betterleaks v1.1.1: importing the detector set
	// git isolation variables process-wide, and later git subprocesses could
	// no longer read user credential helpers from ~/.gitconfig.
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), ".."))

	tmpDir := t.TempDir()
	goMod := `module betterleaksenvcheck

go 1.26.2

require github.com/entireio/cli v0.0.0

replace github.com/entireio/cli => ` + filepath.ToSlash(repoRoot) + `
`
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	goSum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum"))
	if err != nil {
		t.Fatalf("read go.sum: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "go.sum"), goSum, 0o644); err != nil {
		t.Fatalf("write go.sum: %v", err)
	}

	mainGo := `package main

import (
	"fmt"
	"os"

	"github.com/entireio/cli/redact"
)

func main() {
	_ = redact.String("key=AKIAYRWQG5EJLPZLBYNP")
	for _, name := range []string{
		"GIT_CONFIG_GLOBAL",
		"GIT_CONFIG_NOSYSTEM",
		"GIT_CONFIG_SYSTEM",
		"GIT_NO_REPLACE_OBJECTS",
		"GIT_TERMINAL_PROMPT",
	} {
		if value, ok := os.LookupEnv(name); ok {
			fmt.Printf("%s=%s\n", name, value)
		}
	}
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(mainGo), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "go", "run", "-mod=mod", ".")
	cmd.Dir = tmpDir
	cmd.Env = envWithoutGitIsolation()

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run env check failed: %v\n%s", err, output)
	}
	if strings.TrimSpace(string(output)) != "" {
		t.Fatalf("redact import polluted git environment:\n%s", output)
	}
}

func envWithoutGitIsolation() []string {
	blocked := map[string]struct{}{
		"GIT_CONFIG_GLOBAL":      {},
		"GIT_CONFIG_NOSYSTEM":    {},
		"GIT_CONFIG_SYSTEM":      {},
		"GIT_NO_REPLACE_OBJECTS": {},
		"GIT_TERMINAL_PROMPT":    {},
	}

	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, ok := blocked[name]; ok {
			continue
		}
		if name == "GOWORK" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GOWORK=off")
}
