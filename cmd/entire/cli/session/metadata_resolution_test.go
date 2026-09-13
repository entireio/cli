package session

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestStateStoreConstructionGitSubprocesses(t *testing.T) {
	// CWD, discovery caches, and Trace2 configuration are process-global.
	for _, tc := range []struct {
		name         string
		explicitRoot bool
		warmRoot     bool
	}{
		{name: "cwd"},
		{name: "discovered_root", warmRoot: true},
		{name: "explicit_root", explicitRoot: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			testutil.InitRepo(t, root)
			t.Chdir(root)
			paths.ClearWorktreeRootCache()
			ClearGitCommonDirCache()
			t.Cleanup(paths.ClearWorktreeRootCache)
			t.Cleanup(ClearGitCommonDirCache)
			ctx := context.Background()
			if tc.warmRoot {
				_, err := paths.WorktreeRoot(ctx)
				require.NoError(t, err)
			}
			tracePath := filepath.Join(t.TempDir(), "git-trace.jsonl")
			t.Setenv("GIT_TRACE2_EVENT", tracePath)

			var store *StateStore
			var err error
			if tc.explicitRoot {
				store, err = NewStateStoreForWorktree(ctx, root)
			} else {
				store, err = NewStateStore(ctx)
			}
			require.NoError(t, err)
			require.NotNil(t, store)
			commands := sessionTraceCommands(t, tracePath)
			t.Logf("constructor Git subprocesses: %v", commands)
			var want [][]string
			if !tc.explicitRoot && !tc.warmRoot {
				want = [][]string{{"rev-parse", "--show-toplevel"}}
			}
			require.Equal(t, want, commands)

			testutil.RunGit(t, root, "rev-parse", "--git-common-dir")
			observed := sessionTraceCommands(t, tracePath)
			require.Equal(t, append(commands, []string{"rev-parse", "--git-common-dir"}), observed,
				"the positive control must record an additional real Git process")
		})
	}
}

func TestStateStoreFromCWDObservesMetadataDamageAndRepair(t *testing.T) {
	// CWD and the worktree-root cache are process-global.
	root := t.TempDir()
	testutil.InitRepo(t, root)
	t.Chdir(root)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	ctx := context.Background()
	store, err := NewStateStore(ctx)
	require.NoError(t, err)
	state := &State{SessionID: "cwd-repair-session", Kind: KindImported}
	require.NoError(t, store.Save(ctx, state))

	commonFile := filepath.Join(root, ".git", "commondir")
	require.NoError(t, os.WriteFile(commonFile, []byte("missing\n"), 0o600))
	store, err = NewStateStore(ctx)
	require.Error(t, err)
	require.Nil(t, store)

	require.NoError(t, os.Remove(commonFile))
	store, err = NewStateStore(ctx)
	require.NoError(t, err)
	loaded, err := store.Load(ctx, state.SessionID)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Equal(t, state.SessionID, loaded.SessionID)
}

func TestStateStoreForLinkedWorktreeSharesSessionStorage(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	linkedRoot := filepath.Join(tmp, "linked")
	testutil.InitRepo(t, mainRoot)
	testutil.WriteFile(t, mainRoot, "initial.txt", "initial\n")
	testutil.GitAdd(t, mainRoot, "initial.txt")
	testutil.GitCommit(t, mainRoot, "initial")
	testutil.RunGit(t, mainRoot, "worktree", "add", "-b", "linked", linkedRoot)

	ctx := context.Background()
	linkedStore, err := NewStateStoreForWorktree(ctx, linkedRoot)
	require.NoError(t, err)
	state := &State{SessionID: "shared-session", Kind: KindImported}
	require.NoError(t, linkedStore.Save(ctx, state))

	mainStore, err := NewStateStoreForWorktree(ctx, mainRoot)
	require.NoError(t, err)
	loaded, err := mainStore.Load(ctx, state.SessionID)
	require.NoError(t, err)
	require.Equal(t, state.SessionID, loaded.SessionID)
	require.FileExists(t, filepath.Join(mainRoot, ".git", SessionStateDirName, state.SessionID+".json"))
	require.NoError(t, mainStore.Clear(ctx, state.SessionID))
	loaded, err = linkedStore.Load(ctx, state.SessionID)
	require.NoError(t, err)
	require.Nil(t, loaded)
}

func TestStateStoreForWorktreeRejectsBrokenMetadataAndObservesRepair(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	testutil.InitRepo(t, root)
	ctx := context.Background()
	store, err := NewStateStoreForWorktree(ctx, root)
	require.NoError(t, err)
	state := &State{SessionID: "repair-session", Kind: KindImported}
	require.NoError(t, store.Save(ctx, state))

	commonFile := filepath.Join(root, ".git", "commondir")
	require.NoError(t, os.WriteFile(commonFile, []byte("missing\n"), 0o600))
	store, err = NewStateStoreForWorktree(ctx, root)
	require.Error(t, err)
	require.Nil(t, store)

	require.NoError(t, os.Remove(commonFile))
	store, err = NewStateStoreForWorktree(ctx, root)
	require.NoError(t, err)
	loaded, err := store.Load(ctx, state.SessionID)
	require.NoError(t, err)
	require.Equal(t, state.SessionID, loaded.SessionID)
}

func TestStateStoreForWorktreeRejectsInvalidRoots(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"empty", "missing", "not_a_directory", "absent_metadata", "malformed_metadata"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			switch name {
			case "empty":
				root = ""
			case "missing":
				root = filepath.Join(root, "missing")
			case "not_a_directory":
				root = filepath.Join(root, "file")
				require.NoError(t, os.WriteFile(root, nil, 0o600))
			case "malformed_metadata":
				require.NoError(t, os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir:"), 0o600))
			}
			store, err := NewStateStoreForWorktree(context.Background(), root)
			require.Error(t, err)
			require.Nil(t, store)
		})
	}
}

func TestStateStoreForWorktreeIgnoresRepositoryOverrides(t *testing.T) {
	// Repository override variables are process-global.
	root := t.TempDir()
	other := t.TempDir()
	testutil.InitRepo(t, root)
	testutil.InitRepo(t, other)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)

	ctx := context.Background()
	store, err := NewStateStoreForWorktree(ctx, root)
	require.NoError(t, err)
	state := &State{SessionID: "explicit-repository", Kind: KindImported}
	require.NoError(t, store.Save(ctx, state))
	require.FileExists(t, filepath.Join(root, ".git", SessionStateDirName, state.SessionID+".json"))
	require.NoDirExists(t, filepath.Join(other, ".git", SessionStateDirName),
		"inherited Git selectors must not redirect the session write")
}

func sessionTraceCommands(t *testing.T, tracePath string) [][]string {
	t.Helper()
	file, err := os.Open(tracePath)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	defer file.Close()
	var commands [][]string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &event))
		if event.Event == "start" {
			require.NotEmpty(t, event.Argv)
			commands = append(commands, event.Argv[1:])
		}
	}
	require.NoError(t, scanner.Err())
	return commands
}
