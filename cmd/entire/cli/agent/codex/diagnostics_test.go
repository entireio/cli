package codex

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const diagnosticEntireHooksJSON = `{"hooks":{"Stop":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex stop","timeout":30}]}]}}`
const testWindowsOS = "windows"

func TestInspectHookDiagnostics_LinkedWorktreeSeparatesOwnedAndDiscoveredPaths(t *testing.T) {
	t.Parallel()
	mainRoot, linkedRoot := setupDiagnosticLinkedWorktree(t)
	writeDiagnosticHooks(t, linkedRoot, diagnosticEntireHooksJSON)

	diagnostics := inspectHookDiagnosticsAt(context.Background(), linkedRoot)

	require.Equal(t, HookDiscoveryResolved, diagnostics.Discovery.State)
	require.True(t, diagnostics.PathsDiffer())
	require.Equal(t, canonicalHooksPath(t, linkedRoot), diagnostics.WorktreeHooks.Path())
	require.Equal(t, canonicalHooksPath(t, mainRoot), diagnostics.Discovery.DiscoveredHooks.Path())
	require.Equal(t, HookFileEntire, diagnostics.Worktree.State)
	require.Equal(t, HookFileAbsent, diagnostics.Discovered.State)
}

func TestInspectHookDiagnostics_InvalidDiscoveredConfiguration(t *testing.T) {
	t.Parallel()

	t.Run("JSON", func(t *testing.T) {
		t.Parallel()
		mainRoot, linkedRoot := setupDiagnosticLinkedWorktree(t)
		writeDiagnosticHooks(t, mainRoot, `{"hooks":`)
		require.NoError(t, os.MkdirAll(filepath.Join(linkedRoot, ".codex"), 0o750))

		diagnostics := inspectHookDiagnosticsAt(context.Background(), linkedRoot)

		require.Equal(t, HookFileMalformed, diagnostics.Discovered.State)
		require.ErrorContains(t, diagnostics.Discovered.Err, canonicalHooksPath(t, mainRoot))
		require.ErrorContains(t, diagnostics.Discovered.Err, "failed to parse existing hooks.json")
	})

	t.Run("file type", func(t *testing.T) {
		t.Parallel()
		mainRoot, linkedRoot := setupDiagnosticLinkedWorktree(t)
		require.NoError(t, os.MkdirAll(canonicalHooksPath(t, mainRoot), 0o750))
		require.NoError(t, os.MkdirAll(filepath.Join(linkedRoot, ".codex"), 0o750))

		diagnostics := inspectHookDiagnosticsAt(context.Background(), linkedRoot)

		require.Equal(t, HookFileUnavailable, diagnostics.Discovered.State)
		require.ErrorContains(t, diagnostics.Discovered.Err, canonicalHooksPath(t, mainRoot))
		require.Error(t, diagnostics.Discovered.Err)
	})

	t.Run("bounded read", func(t *testing.T) {
		t.Parallel()
		mainRoot, linkedRoot := setupDiagnosticLinkedWorktree(t)
		writeDiagnosticHooks(t, mainRoot, strings.Repeat("x", maxHooksFileBytes+1))
		require.NoError(t, os.MkdirAll(filepath.Join(linkedRoot, ".codex"), 0o750))

		diagnostics := inspectHookDiagnosticsAt(context.Background(), linkedRoot)

		require.Equal(t, HookFileUnavailable, diagnostics.Discovered.State)
		require.ErrorContains(t, diagnostics.Discovered.Err, canonicalHooksPath(t, mainRoot))
		require.ErrorContains(t, diagnostics.Discovered.Err, "exceeds 1048576 bytes")
	})

	t.Run("containment", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == testWindowsOS {
			t.Skip("symlink creation is not generally available on Windows")
		}
		mainRoot, linkedRoot := setupDiagnosticLinkedWorktree(t)
		redirected := filepath.Join(t.TempDir(), "redirected")
		require.NoError(t, os.MkdirAll(redirected, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(redirected, HooksFileName), []byte(diagnosticEntireHooksJSON), 0o600))
		require.NoError(t, os.Symlink(redirected, filepath.Join(mainRoot, ".codex")))
		require.NoError(t, os.MkdirAll(filepath.Join(linkedRoot, ".codex"), 0o750))

		diagnostics := inspectHookDiagnosticsAt(context.Background(), linkedRoot)

		require.Equal(t, HookFileUnavailable, diagnostics.Discovered.State)
		require.Error(t, diagnostics.Discovered.Err)
	})
}

func TestInspectHookDiagnostics_MissingCurrentWorktreeProjectLayer(t *testing.T) {
	t.Parallel()
	mainRoot, linkedRoot := setupDiagnosticLinkedWorktree(t)
	writeDiagnosticHooks(t, mainRoot, diagnosticEntireHooksJSON)

	diagnostics := inspectHookDiagnosticsAt(context.Background(), linkedRoot)

	require.Equal(t, HookFileEntire, diagnostics.Discovered.State)
	require.False(t, diagnostics.Discovery.ProjectLayerExists())
	require.Equal(t, HookFileAbsent, diagnostics.Worktree.State)
}

func TestInspectHookDiagnostics_InvalidCurrentWorktreeWithHealthyDiscovery(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		arrange        func(t *testing.T, mainRoot, linkedRoot string)
		wantError      string
		wantValidLayer bool
	}{
		"redirected project directory": {
			arrange: func(t *testing.T, mainRoot, linkedRoot string) {
				if runtime.GOOS == testWindowsOS {
					t.Skip("directory symlinks require privileges on Windows")
				}
				require.NoError(t, os.Symlink(filepath.Join(mainRoot, ".codex"), filepath.Join(linkedRoot, ".codex")))
			},
			wantError: "not a directory",
		},
		"non-directory project path": {
			arrange: func(t *testing.T, _, linkedRoot string) {
				require.NoError(t, os.WriteFile(filepath.Join(linkedRoot, ".codex"), []byte("not a directory"), 0o600))
			},
			wantError: "not a directory",
		},
		"symlinked hooks file": {
			arrange: func(t *testing.T, mainRoot, linkedRoot string) {
				if runtime.GOOS == testWindowsOS {
					t.Skip("file symlinks require privileges on Windows")
				}
				require.NoError(t, os.Mkdir(filepath.Join(linkedRoot, ".codex"), 0o750))
				require.NoError(t, os.Symlink(
					filepath.Join(mainRoot, ".codex", HooksFileName),
					filepath.Join(linkedRoot, ".codex", HooksFileName),
				))
			},
			wantError:      "hooks.json",
			wantValidLayer: true,
		},
		"oversized hooks file": {
			arrange: func(t *testing.T, _, linkedRoot string) {
				writeDiagnosticHooks(t, linkedRoot, strings.Repeat("x", maxHooksFileBytes+1))
			},
			wantError:      "exceeds 1048576 bytes",
			wantValidLayer: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mainRoot, linkedRoot := setupDiagnosticLinkedWorktree(t)
			writeDiagnosticHooks(t, mainRoot, diagnosticEntireHooksJSON)
			tt.arrange(t, mainRoot, linkedRoot)

			diagnostics := inspectHookDiagnosticsAt(context.Background(), linkedRoot)

			require.Equal(t, HookFileEntire, diagnostics.Discovered.State)
			require.Equal(t, HookFileUnavailable, diagnostics.Worktree.State)
			require.ErrorContains(t, diagnostics.Worktree.Err, tt.wantError)
			require.Equal(t, tt.wantValidLayer, diagnostics.Discovery.ProjectLayerExists())
		})
	}
}

func TestInspectHookDiagnosticsLightweight_RejectsRedirectedProjectLayer(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == testWindowsOS {
		t.Skip("directory symlinks require privileges on Windows")
	}
	mainRoot, linkedRoot := setupDiagnosticLinkedWorktree(t)
	writeDiagnosticHooks(t, mainRoot, diagnosticEntireHooksJSON)
	require.NoError(t, os.Symlink(filepath.Join(mainRoot, ".codex"), filepath.Join(linkedRoot, ".codex")))

	discovery := resolveHookDiscovery(linkedRoot)
	worktreeHooks, err := resolveWorktreeHooksPath(linkedRoot)
	require.NoError(t, err)
	diagnostics := finishHookDiagnostics(
		context.Background(),
		HookDiagnostics{Discovery: discovery},
		worktreeHooks,
		nil,
		true,
	)

	require.Equal(t, HookFileUnavailable, diagnostics.Worktree.State)
	require.ErrorContains(t, diagnostics.Worktree.Err, "not a directory")
	require.Equal(t, HookFileEntire, diagnostics.Discovered.State)
}

func setupDiagnosticLinkedWorktree(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	linkedRoot := filepath.Join(tmp, "linked")
	initCommittedRepo(t, mainRoot)
	runGit(t, mainRoot, "worktree", "add", "-b", "diagnostics", linkedRoot)
	return mainRoot, linkedRoot
}

func writeDiagnosticHooks(t *testing.T, root, contents string) {
	t.Helper()
	projectDir := filepath.Join(root, ".codex")
	require.NoError(t, os.MkdirAll(projectDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, HooksFileName), []byte(contents), 0o600))
}
