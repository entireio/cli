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

// cloneRefUnderTest is passed verbatim to `git clone`, which is what keeps this
// test off the network: a full entire:// URL needs no control-plane lookup to
// resolve, so the parent reaches the exec without authenticating anything.
const cloneRefUnderTest = "entire://cluster.example/et/proj/repo"

// TestRepoClone_ContextFlagReachesGit pins the COR-1630 fix across the process
// boundary the bug lived on: `entire --context X repo clone entire://…` must
// hand X to the `git clone` it spawns as ENTIRE_CONTEXT, because git runs the
// `entire` remote helper as a separate process that selects a login from the
// saved contexts by itself. The in-process unit tests assert os.Getenv in the
// same process; this one reads what an actual child received.
//
// The child it reads is specifically the `clone` invocation — see
// writeEnvDumpingGit for why that scoping is the whole test. A saved context
// named "staging" is required because the parent refuses an unknown --context
// before spawning anything, which the third case pins as well.
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
		name         string
		args         []string
		wantCloneRun bool
		wantContext  string // the ENTIRE_CONTEXT value the clone must have seen
		wantStderr   string
	}{
		{
			name:         "--context reaches git as ENTIRE_CONTEXT",
			args:         []string{"--context", "staging"},
			wantCloneRun: true,
			wantContext:  "staging",
		},
		{
			name:         "no flag exports nothing",
			wantCloneRun: true,
		},
		{
			name:       "unknown --context is refused before git runs",
			args:       []string{"--context", "typo"},
			wantStderr: `--context selected login context "typo"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			binDir := t.TempDir()
			outDir := t.TempDir()
			argvLog := filepath.Join(outDir, "git-argv")
			envDump := filepath.Join(outDir, "clone-env")
			writeEnvDumpingGit(t, binDir, argvLog, envDump)

			args := append(append([]string{}, tt.args...), "repo", "clone", cloneRefUnderTest)
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
			invocations := readLines(t, argvLog)

			if !tt.wantCloneRun {
				assert.NotContains(t, invocations, "clone "+cloneRefUnderTest,
					"the parent must refuse the flag before spawning the clone")
				require.NoFileExists(t, envDump)
				require.Error(t, runErr, "the parent must exit non-zero")
				assert.Contains(t, stderr.String(), tt.wantStderr)
				return
			}
			require.Contains(t, invocations, "clone "+cloneRefUnderTest,
				"stub git never ran the clone; invocations=%q stderr:\n%s", invocations, stderr.String())
			require.NoError(t, runErr, "stub git exits 0, so the clone must succeed; stderr:\n%s", stderr.String())

			// Assert on the ENTIRE_CONTEXT lines alone: the dump is the child's
			// whole environment, and a failure that printed it would put the
			// machine's environment — a developer's shell, or a CI runner's
			// secrets — in the test log.
			got := grepEnv(t, envDump, contexts.EnvContextVar)
			if tt.wantContext == "" {
				assert.Empty(t, got, "no flag must export nothing to the clone")
				return
			}
			assert.Equal(t, []string{contexts.EnvContextVar + "=" + tt.wantContext}, got)
		})
	}
}

// writeEnvDumpingGit writes a stub `git` into dir that appends every
// invocation's arguments to argvLog, records the environment to envDump for the
// `clone` invocation only, and exits 0 — so a test can see exactly what the CLI
// handed the child without running real git or touching the network.
//
// Scoping the dump to `clone` is load-bearing, not tidiness. `entire repo
// clone` runs `git rev-parse --show-toplevel` before the clone and twice more
// after it, so a stub that wrote one file on every invocation would leave the
// test reading a TRAILING rev-parse's environment. Those inherit through a
// different path, so the assertion passed even with ENTIRE_CONTEXT deliberately
// stripped at the clone exec — the regression this test exists to catch.
func writeEnvDumpingGit(t *testing.T, dir, argvLog, envDump string) {
	t.Helper()
	body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nif [ \"$1\" = clone ]; then env > %q; fi\nexit 0\n",
		argvLog, envDump)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(body), 0o755))
}

// readLines returns path's non-empty lines, or nil when the stub never wrote it.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// grepEnv returns the `NAME=…` lines for name from an `env` dump, so an
// assertion never carries the rest of the environment into a failure message.
func grepEnv(t *testing.T, path, name string) []string {
	t.Helper()
	var out []string
	for _, l := range readLines(t, path) {
		if strings.HasPrefix(l, name+"=") {
			out = append(out, l)
		}
	}
	return out
}
