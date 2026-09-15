//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/entireclient/contexts"
)

// TestRepoClone_ContextFlagReachesGit pins the COR-1630 fix across the process
// boundary the bug lived on: `entire --context X repo clone entire://…` must
// hand X to the `git clone` it spawns as ENTIRE_CONTEXT, because git runs the
// `entire` remote helper as a separate process that selects a login from the
// saved contexts by itself. The in-process unit tests assert os.Getenv in the
// same process; this one reads what an actual child received.
//
// No network: a stub `git` first on PATH dumps its environment and exits 0,
// and a verbatim entire:// URL is passed straight to `git clone` with no
// control-plane lookup. A saved context named "staging" is required because
// the parent refuses an unknown --context before spawning anything — which the
// third case pins as well.
//
// This also fails if a future change sets cmd.Env at the clone exec site
// without carrying ENTIRE_CONTEXT — the one way "every child inherits it" can
// quietly stop being true.
func TestRepoClone_ContextFlagReachesGit(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsGOOS {
		t.Skip("stub-git shell-script harness only runs on Unix (see writePluginScript)")
	}

	cfgDir := t.TempDir()
	require.NoError(t, contexts.Save(cfgDir, &contexts.File{
		Contexts: []*contexts.Context{{
			Name: "staging", CoreURL: "https://us.auth.partial.to", Handle: "me", KeychainService: "kc:staging",
		}},
	}))

	tests := []struct {
		name        string
		args        []string
		wantGitRun  bool
		wantEnvLine string // required in git's environment when non-empty
		wantAbsent  bool   // ENTIRE_CONTEXT must not appear in git's environment
		wantStderr  string
	}{
		{
			name:        "--context reaches git as ENTIRE_CONTEXT",
			args:        []string{"--context", "staging"},
			wantGitRun:  true,
			wantEnvLine: contexts.EnvContextVar + "=staging",
		},
		{
			name:       "no flag exports nothing",
			wantGitRun: true,
			wantAbsent: true,
		},
		{
			name:       "unknown --context is refused before git runs",
			args:       []string{"--context", "typo"},
			wantGitRun: false,
			wantStderr: `--context selected login context "typo"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			binDir := t.TempDir()
			envDump := filepath.Join(t.TempDir(), "git-env")
			writeEnvDumpingGit(t, binDir, envDump)

			args := append(append([]string{}, tt.args...), "repo", "clone", "entire://cluster.example/et/proj/repo")
			cmd := execx.NonInteractive(t.Context(), getTestBinary(), args...)
			cmd.Dir = t.TempDir()
			// Later entries win in os/exec's environment dedup, so these override
			// the harness-wide PATH and ENTIRE_CONFIG_DIR that GitIsolatedEnv
			// carries from os.Environ().
			cmd.Env = append(testutil.GitIsolatedEnv(),
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"ENTIRE_CONFIG_DIR="+cfgDir,
			)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			runErr := cmd.Run()

			dump, readErr := os.ReadFile(envDump)
			if !tt.wantGitRun {
				require.ErrorIs(t, readErr, os.ErrNotExist, "git must not have been spawned; stderr:\n%s", stderr.String())
				require.Error(t, runErr, "the parent must exit non-zero")
				assert.Contains(t, stderr.String(), tt.wantStderr)
				return
			}
			require.NoError(t, readErr, "stub git never ran; stderr:\n%s", stderr.String())
			require.NoError(t, runErr, "stub git exits 0, so the clone must succeed; stderr:\n%s", stderr.String())
			lines := strings.Split(string(dump), "\n")
			if tt.wantEnvLine != "" {
				assert.Contains(t, lines, tt.wantEnvLine)
			}
			if tt.wantAbsent {
				for _, l := range lines {
					assert.False(t, strings.HasPrefix(l, contexts.EnvContextVar+"="), "unexpected %s in git's environment", l)
				}
			}
		})
	}
}

// writeEnvDumpingGit writes a stub `git` into dir that records its environment
// to envDump and exits 0, so a test can see exactly what the CLI handed the
// child without running real git or touching the network.
func writeEnvDumpingGit(t *testing.T, dir, envDump string) {
	t.Helper()
	body := fmt.Sprintf("#!/bin/sh\nenv > %q\nexit 0\n", envDump)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(body), 0o755))
}
