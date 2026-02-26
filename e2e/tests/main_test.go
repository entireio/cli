//go:build e2e

package tests

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/cli/e2e/agents"
	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
)

func TestMain(m *testing.M) {
	runDir := os.Getenv("E2E_ARTIFACT_DIR")
	if runDir == "" {
		_, file, _, _ := runtime.Caller(0)
		testutil.ArtifactRoot = filepath.Join(filepath.Dir(file), "..", "artifacts")
		runDir = testutil.ArtifactRunDir()
	}
	_ = os.MkdirAll(runDir, 0o755)
	testutil.SetRunDir(runDir)

	// Resolve the entire binary (builds from source if E2E_ENTIRE_BIN is unset).
	entireBin := entire.BinPath()

	os.Setenv("PATH", filepath.Dir(entireBin)+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Preflight: verify required dependencies before running any tests.
	var missing []string
	for _, bin := range []string{"git", "tmux"} {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	for _, a := range agents.All() {
		if _, err := exec.LookPath(a.Binary()); err != nil {
			missing = append(missing, a.Binary())
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "preflight: missing required binaries: %v\n", missing)
		os.Exit(1)
	}

	version := "unknown"
	if out, err := exec.Command(entireBin, "version").Output(); err == nil {
		version = string(out)
		_ = os.WriteFile(filepath.Join(runDir, "entire-version.txt"), out, 0o644)
	}

	fmt.Fprintf(os.Stderr, "entire binary:  %s\n", entireBin)
	fmt.Fprintf(os.Stderr, "entire version: %s", version)
	fmt.Fprintf(os.Stderr, "artifact dir:   %s\n", runDir)

	// Don't look at user's Git config, ignore everything except the project-local Git settings.
	// This avoids oddball configs in ~/.gitconfig messing with our E2E tests.
	os.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")

	os.Exit(m.Run())
}
