package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

const userOnlyHooksFixture = `{
  "custom": "kept",
  "hooks": {
    "PreToolUse": [{"matcher": "^Bash$", "hooks": [{"type": "command", "command": "./user-guard.sh", "timeout": 5}]}],
    "SessionStart": [{"matcher": null, "hooks": [{"type": "command", "command": "./user-start.sh", "timeout": 10}]}]
  }
}
`

func setupLinkedWorktreeForInstall(t *testing.T) (mainRoot, linkedRoot string) {
	t.Helper()
	tmp := t.TempDir()
	mainRoot = filepath.Join(tmp, "main")
	linkedRoot = filepath.Join(tmp, "linked")
	initCommittedRepo(t, mainRoot)
	runGit(t, mainRoot, "worktree", "add", "-b", "feature", linkedRoot)
	return canonicalPathForTest(t, mainRoot), canonicalPathForTest(t, linkedRoot)
}

func writeProjectHooks(t *testing.T, root, content string) string {
	t.Helper()
	path := filepath.Join(root, ".codex", HooksFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestInstallDiscoveredHooks_LinkedWorktreeCreatesPrimaryHooks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mainRoot, linkedRoot := setupLinkedWorktreeForInstall(t)

	target := resolveHookDiscovery(linkedRoot).DiscoveredHooks
	require.Equal(t, filepath.Join(mainRoot, ".codex", HooksFileName), target.Path())

	count, err := installDiscoveredHooksFrom(ctx, linkedRoot, target)
	require.NoError(t, err)
	require.Equal(t, len(managedHooks), count)

	inspection := inspectHookConfigAt(ctx, target.Path())
	require.Equal(t, HookFileEntire, inspection.State)
	require.True(t, inspection.Current)
	require.NoFileExists(t, filepath.Join(linkedRoot, ".codex", HooksFileName),
		"installing into the discovered root must not touch the current worktree")
}

func TestInstallDiscoveredHooks_MergesIntoExistingUserHooks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mainRoot, linkedRoot := setupLinkedWorktreeForInstall(t)
	primaryPath := writeProjectHooks(t, mainRoot, userOnlyHooksFixture)

	target := resolveHookDiscovery(linkedRoot).DiscoveredHooks
	_, err := installDiscoveredHooksFrom(ctx, linkedRoot, target)
	require.NoError(t, err)

	data, err := os.ReadFile(primaryPath)
	require.NoError(t, err)
	var file struct {
		Custom string                    `json:"custom"`
		Hooks  map[string][]MatcherGroup `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &file))
	require.Equal(t, "kept", file.Custom)
	require.True(t, groupsHaveCommand(file.Hooks["PreToolUse"], "./user-guard.sh"))
	require.True(t, groupsHaveCommand(file.Hooks["SessionStart"], "./user-start.sh"))
	require.True(t, hasEntireHook(file.Hooks["SessionStart"]))
	require.True(t, inspectHookConfigAt(ctx, primaryPath).Current)
}

func TestInstallDiscoveredHooks_UpdatesOutdatedEntireHooks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mainRoot, linkedRoot := setupLinkedWorktreeForInstall(t)
	primaryPath := writeProjectHooks(t, mainRoot, `{"hooks":{
  "SessionStart":[{"matcher":null,"hooks":[{"type":"command","command":"entire hooks codex session-start"}]}]
}}`)
	require.False(t, inspectHookConfigAt(ctx, primaryPath).Current)

	_, err := installDiscoveredHooksFrom(ctx, linkedRoot, resolveHookDiscovery(linkedRoot).DiscoveredHooks)
	require.NoError(t, err)
	require.True(t, inspectHookConfigAt(ctx, primaryPath).Current)
}

func TestInstallDiscoveredHooks_BareLayoutWritesLayoutRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	featureRoot := setupBareWorktreeLayout(t)
	layoutRoot := canonicalPathForTest(t, filepath.Dir(featureRoot))

	target := resolveHookDiscovery(featureRoot).DiscoveredHooks
	require.False(t, target.RootIsCheckout())
	_, err := installDiscoveredHooksFrom(ctx, featureRoot, target)
	require.NoError(t, err)
	require.True(t, inspectHookConfigAt(ctx, filepath.Join(layoutRoot, ".codex", HooksFileName)).Current)
}

func TestInstallDiscoveredHooks_RefusesMalformedFile(t *testing.T) {
	t.Parallel()
	mainRoot, linkedRoot := setupLinkedWorktreeForInstall(t)
	primaryPath := writeProjectHooks(t, mainRoot, "{")

	_, err := installDiscoveredHooksFrom(context.Background(), linkedRoot, resolveHookDiscovery(linkedRoot).DiscoveredHooks)
	require.Error(t, err)
	data, readErr := os.ReadFile(primaryPath)
	require.NoError(t, readErr)
	require.Equal(t, "{", string(data))
}

func TestInstallDiscoveredHooks_RefusesSymlinks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}

	t.Run("symlinked .codex directory", func(t *testing.T) {
		t.Parallel()
		mainRoot, linkedRoot := setupLinkedWorktreeForInstall(t)
		elsewhere := t.TempDir()
		require.NoError(t, os.Symlink(elsewhere, filepath.Join(mainRoot, ".codex")))

		_, err := installDiscoveredHooksFrom(context.Background(), linkedRoot, resolveHookDiscovery(linkedRoot).DiscoveredHooks)
		require.Error(t, err)
		require.NoFileExists(t, filepath.Join(elsewhere, HooksFileName))
	})

	t.Run("symlinked hooks.json", func(t *testing.T) {
		t.Parallel()
		mainRoot, linkedRoot := setupLinkedWorktreeForInstall(t)
		elsewhere := filepath.Join(t.TempDir(), "hooks.json")
		require.NoError(t, os.WriteFile(elsewhere, []byte(userOnlyHooksFixture), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(mainRoot, ".codex"), 0o750))
		require.NoError(t, os.Symlink(elsewhere, filepath.Join(mainRoot, ".codex", HooksFileName)))

		_, err := installDiscoveredHooksFrom(context.Background(), linkedRoot, resolveHookDiscovery(linkedRoot).DiscoveredHooks)
		require.Error(t, err)
		data, readErr := os.ReadFile(elsewhere)
		require.NoError(t, readErr)
		require.JSONEq(t, userOnlyHooksFixture, string(data))
	})
}

func TestInstallDiscoveredHooks_RefusesChangedOrUnresolvedDiscovery(t *testing.T) {
	t.Parallel()

	t.Run("discovery changed since consent", func(t *testing.T) {
		t.Parallel()
		_, linkedRoot := setupLinkedWorktreeForInstall(t)
		otherMain, otherLinked := setupLinkedWorktreeForInstall(t)
		consented := resolveHookDiscovery(otherLinked).DiscoveredHooks

		_, err := installDiscoveredHooksFrom(context.Background(), linkedRoot, consented)
		require.ErrorContains(t, err, "changed")
		require.NoDirExists(t, filepath.Join(otherMain, ".codex"))
	})

	t.Run("unresolved discovery", func(t *testing.T) {
		t.Parallel()
		notARepo := canonicalPathForTest(t, t.TempDir())

		_, err := installDiscoveredHooksFrom(context.Background(), notARepo, DiscoveredHooksPath{})
		require.Error(t, err)
		require.NoDirExists(t, filepath.Join(notARepo, ".codex"))
	})

	t.Run("zero target", func(t *testing.T) {
		t.Parallel()
		mainRoot, linkedRoot := setupLinkedWorktreeForInstall(t)

		_, err := installDiscoveredHooksFrom(context.Background(), linkedRoot, DiscoveredHooksPath{})
		require.Error(t, err)
		require.NoDirExists(t, filepath.Join(mainRoot, ".codex"))
	})
}

func TestDiscoveredHooksPath_RootIsCheckout(t *testing.T) {
	t.Parallel()

	t.Run("conventional linked worktree discovers a checkout", func(t *testing.T) {
		t.Parallel()
		_, linkedRoot := setupLinkedWorktreeForInstall(t)
		require.True(t, resolveHookDiscovery(linkedRoot).DiscoveredHooks.RootIsCheckout())
	})

	t.Run("bare layout root is not a checkout", func(t *testing.T) {
		t.Parallel()
		require.False(t, resolveHookDiscovery(setupBareWorktreeLayout(t)).DiscoveredHooks.RootIsCheckout())
	})
}

func TestHookDiagnostics_DiscoveredInstallCandidate(t *testing.T) {
	t.Parallel()
	worktree := WorktreeHooksPath{path: "/repo-feature/.codex/hooks.json", worktreeRoot: "/repo-feature"}
	primary := DiscoveredHooksPath{path: "/repo-main/.codex/hooks.json", root: "/repo-main", rootIsCheckout: true}
	same := DiscoveredHooksPath{path: worktree.path, root: worktree.worktreeRoot, rootIsCheckout: true}
	storageParent := DiscoveredHooksPath{path: "/gitdirs/.codex/hooks.json", root: "/gitdirs", separateGitDirParent: true}

	tests := []struct {
		name       string
		state      HookDiscoveryState
		discovered DiscoveredHooksPath
		inspection HookConfigInspection
		want       bool
	}{
		{name: "absent primary", state: HookDiscoveryResolved, discovered: primary, inspection: HookConfigInspection{State: HookFileAbsent}, want: true},
		{name: "user-only primary", state: HookDiscoveryResolved, discovered: primary, inspection: HookConfigInspection{State: HookFileUserOnly}, want: true},
		{name: "outdated primary", state: HookDiscoveryResolved, discovered: primary, inspection: HookConfigInspection{State: HookFileEntire}, want: true},
		{name: "current primary", state: HookDiscoveryResolved, discovered: primary, inspection: HookConfigInspection{State: HookFileEntire, Current: true}},
		{name: "malformed primary", state: HookDiscoveryResolved, discovered: primary, inspection: HookConfigInspection{State: HookFileMalformed}},
		{name: "unavailable primary", state: HookDiscoveryResolved, discovered: primary, inspection: HookConfigInspection{State: HookFileUnavailable}},
		{name: "same path", state: HookDiscoveryResolved, discovered: same, inspection: HookConfigInspection{State: HookFileAbsent}},
		{name: "separate Git directory storage parent", state: HookDiscoveryResolved, discovered: storageParent, inspection: HookConfigInspection{State: HookFileAbsent}},
		{name: "unresolved", state: HookDiscoveryUnresolved, inspection: HookConfigInspection{State: HookFileUnavailable}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			diagnostics := HookDiagnostics{
				Discovery:     HookDiscovery{State: tt.state, DiscoveredHooks: tt.discovered},
				WorktreeHooks: worktree,
				Discovered:    tt.inspection,
			}
			target, ok := diagnostics.DiscoveredInstallCandidate()
			require.Equal(t, tt.want, ok)
			if tt.want {
				require.Equal(t, tt.discovered, target)
			}
		})
	}
}

func TestResolveHookDiscovery_NormalAndSubmoduleCheckoutsHaveNoCandidate(t *testing.T) {
	t.Parallel()
	normal := filepath.Join(t.TempDir(), "repo")
	initCommittedRepo(t, normal)
	ordinarySubmodule, linkedSubmodule := setupSubmoduleWorktrees(t)

	for _, root := range []string{normal, ordinarySubmodule, linkedSubmodule} {
		worktreeHooks, err := resolveWorktreeHooksPath(root)
		require.NoError(t, err)
		diagnostics := HookDiagnostics{
			Discovery:     resolveHookDiscovery(root),
			WorktreeHooks: worktreeHooks,
			Discovered:    HookConfigInspection{State: HookFileAbsent},
		}
		_, ok := diagnostics.DiscoveredInstallCandidate()
		require.False(t, ok, root)
	}
}

func groupsHaveCommand(groups []MatcherGroup, command string) bool {
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if hook.Command == command {
				return true
			}
		}
	}
	return false
}

// A linked worktree of a repository with a separate Git directory discovers
// the storage parent, which may hold other repositories' storage: never offered.
func TestResolveHookDiscovery_SeparateGitDirWorktreeHasNoCandidate(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	storageRoot := filepath.Join(tmp, "gitdirs", "main.git")
	require.NoError(t, os.MkdirAll(filepath.Dir(storageRoot), 0o755))
	runGitWithDir(t, tmp, "init", "--separate-git-dir", storageRoot, mainRoot)
	runGitWithDir(t, mainRoot, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	linked := filepath.Join(tmp, "linked")
	runGitWithDir(t, mainRoot, "worktree", "add", "-b", "feature", linked)

	worktreeHooks, err := resolveWorktreeHooksPath(linked)
	require.NoError(t, err)
	discovery := resolveHookDiscovery(linked)
	diagnostics := HookDiagnostics{Discovery: discovery, WorktreeHooks: worktreeHooks, Discovered: HookConfigInspection{State: HookFileAbsent}}
	_, ok := diagnostics.DiscoveredInstallCandidate()
	require.False(t, ok, "discovered %s", discovery.DiscoveredHooks.Path())
}

// A hooks file holding JSON null is user-only and offered; installing into it
// must not panic on the nil top-level map.
func TestInstallDiscoveredHooks_NullHooksFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mainRoot, linkedRoot := setupLinkedWorktreeForInstall(t)
	primaryPath := writeProjectHooks(t, mainRoot, "null\n")

	_, err := installDiscoveredHooksFrom(ctx, linkedRoot, resolveHookDiscovery(linkedRoot).DiscoveredHooks)
	require.NoError(t, err)
	require.True(t, inspectHookConfigAt(ctx, primaryPath).Current)
}

// User hooks in the discovered file keep every field Entire does not model.
func TestInstallDiscoveredHooks_PreservesUserHookFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mainRoot, linkedRoot := setupLinkedWorktreeForInstall(t)
	primaryPath := writeProjectHooks(t, mainRoot, `{"hooks":{"SessionStart":[
  {"matcher":"startup","note":"group-extra","hooks":[
    {"type":"command","command":"./user-start.sh","timeoutSec":12,"async":true,"statusMessage":"Starting"}
  ]}
]}}`)

	_, err := installDiscoveredHooksFrom(ctx, linkedRoot, resolveHookDiscovery(linkedRoot).DiscoveredHooks)
	require.NoError(t, err)

	data, err := os.ReadFile(primaryPath)
	require.NoError(t, err)
	var file struct {
		Hooks map[string][]map[string]json.RawMessage `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &file))
	var userGroup map[string]json.RawMessage
	for _, group := range file.Hooks["SessionStart"] {
		if string(group["matcher"]) == `"startup"` {
			userGroup = group
		}
	}
	require.NotNil(t, userGroup, "user matcher group missing: %s", data)
	require.JSONEq(t, `"group-extra"`, string(userGroup["note"]))
	require.JSONEq(t,
		`[{"type":"command","command":"./user-start.sh","timeoutSec":12,"async":true,"statusMessage":"Starting"}]`,
		string(userGroup["hooks"]))
	require.True(t, inspectHookConfigAt(ctx, primaryPath).Current)
}

// Duplicate, otherwise-current Entire hooks are not current; the install must
// collapse them so the file becomes current and is not offered again.
func TestInstallDiscoveredHooks_CollapsesDuplicateEntireHooks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mainRoot, linkedRoot := setupLinkedWorktreeForInstall(t)
	primaryPath := filepath.Join(mainRoot, ".codex", HooksFileName)
	target := resolveHookDiscovery(linkedRoot).DiscoveredHooks
	_, err := installDiscoveredHooksFrom(ctx, linkedRoot, target)
	require.NoError(t, err)

	data, err := os.ReadFile(primaryPath)
	require.NoError(t, err)
	var file map[string]map[string][]MatcherGroup
	require.NoError(t, json.Unmarshal(data, &file))
	for event, groups := range file["hooks"] {
		file["hooks"][event] = append(groups, groups...)
	}
	duplicated, err := json.Marshal(file)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(primaryPath, duplicated, 0o600))
	inspection := inspectHookConfigAt(ctx, primaryPath)
	require.Equal(t, HookFileEntire, inspection.State)
	require.False(t, inspection.Current)

	count, err := installDiscoveredHooksFrom(ctx, linkedRoot, target)
	require.NoError(t, err)
	require.Positive(t, count)
	require.True(t, inspectHookConfigAt(ctx, primaryPath).Current)
}
