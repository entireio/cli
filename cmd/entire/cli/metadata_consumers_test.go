package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/investigate"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

type metadataConsumerLayout struct {
	root, gitDir, commonDir, id string
}

func TestMetadataConsumersAgreeAcrossGitLayouts(t *testing.T) {
	// The consumers discover from CWD; each case owns the process-wide discovery state.
	testutil.IsolateGitConfigEnv(t)
	for _, layout := range []string{"ordinary", "linked", "dot-bare", "bare-storage", "separate", "submodule", "linked-submodule", "submodule-in-linked-superproject", "relative-pointer", "moved-registration", "alias"} {
		t.Run(layout, func(t *testing.T) {
			fixture := newMetadataConsumerLayout(t, layout)
			t.Chdir(fixture.root)
			paths.ClearWorktreeRootCache()
			t.Cleanup(paths.ClearWorktreeRootCache)
			metadata, err := gitrepo.ResolveWorktreeMetadata(fixture.root)
			require.NoError(t, err)
			require.Equal(t, fixture.id, metadata.WorktreeID)
			requireSameMetadataDirectory(t, fixture.gitDir, metadata.GitDir)
			requireSameMetadataDirectory(t, fixture.commonDir, metadata.CommonDir)
			require.Equal(t, checkpoint.HashWorktreeID(fixture.id), getCurrentWorktreeHash(t.Context()))
			branch := strings.TrimSpace(testutil.RunGit(t, fixture.root, "branch", "--show-current"))
			if branch == "" {
				branch = detachedHEADDisplay
			}
			require.Equal(t, branch, resolveWorktreeBranch(t.Context(), fixture.root))

			state := &session.State{SessionID: "metadata-agreement", Kind: session.KindImported, WorktreeID: fixture.id}
			store, err := session.NewStateStore(t.Context())
			require.NoError(t, err)
			require.NoError(t, store.Save(t.Context(), state))
			explicit, err := session.NewStateStoreForWorktree(t.Context(), fixture.root)
			require.NoError(t, err)
			loaded, err := explicit.Load(t.Context(), state.SessionID)
			require.NoError(t, err)
			require.Equal(t, state.WorktreeID, loaded.WorktreeID)
			require.FileExists(t, filepath.Join(fixture.commonDir, session.SessionStateDirName, state.SessionID+".json"))

			repo, err := gitrepo.OpenPath(fixture.root)
			require.NoError(t, err)
			defer repo.Close()
			queue, err := checkpoint.PushQueueForRepo(t.Context(), repo)
			require.NoError(t, err)
			ref := plumbing.ReferenceName("refs/entire/checkpoints/metadata-agreement")
			require.NoError(t, queue.Enqueue(ref))
			refs, err := checkpoint.NewPushQueue(fixture.commonDir).Drain()
			require.NoError(t, err)
			require.Equal(t, []plumbing.ReferenceName{ref}, refs)

			require.NoError(t, settings.ModifyClonePreferences(t.Context(), func(p *settings.ClonePreferences) error { p.ReviewDefaultProfile = "metadata-proof"; return nil }))
			prefsPath, err := settings.ClonePreferencesPath(t.Context())
			require.NoError(t, err)
			requireSameMetadataDirectory(t, fixture.commonDir, filepath.Dir(filepath.Dir(prefsPath)))
			require.FileExists(t, filepath.Join(fixture.commonDir, settings.ClonePreferencesFile))
			config, err := settings.Load(settings.WithWorktreeRoot(t.Context(), fixture.root))
			require.NoError(t, err)
			require.Equal(t, "metadata-proof", config.ReviewDefaultProfile)

			marker := review.PendingReviewMarker{AgentName: "codex", Prompt: "metadata agreement"}
			require.NoError(t, review.WritePendingReviewMarker(t.Context(), marker))
			gotMarker, found, err := review.ReadPendingReviewMarker(t.Context())
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, marker.Prompt, gotMarker.Prompt)

			runs, err := investigate.NewStateStore(t.Context())
			require.NoError(t, err)
			require.NoError(t, runs.WriteFindings("aabbccddeeff", []byte("shared findings")))
			require.FileExists(t, filepath.Join(fixture.commonDir, "entire-investigations", "aabbccddeeff", "findings.md"))
			manifests, err := investigate.NewLocalManifestStore(t.Context())
			require.NoError(t, err)
			manifest := investigate.LocalManifest{RunID: "aabbccddeeff", StartedAt: time.Now().UTC()}
			require.NoError(t, manifests.Write(t.Context(), manifest))
			require.FileExists(t, manifests.PathFor(manifest))
			requireSameMetadataDirectory(t, fixture.commonDir, filepath.Dir(filepath.Dir(filepath.Dir(manifests.PathFor(manifest)))))

			hint, err := trailEnablementScopeHintPath(t.Context(), state.SessionID)
			require.NoError(t, err)
			requireSameMetadataDirectory(t, fixture.commonDir, filepath.Dir(filepath.Dir(hint)))
			discovery := codex.ResolveHookDiscovery(t.Context())
			require.Equal(t, codex.HookDiscoveryResolved, discovery.State)
			require.NoError(t, discovery.Diagnostic)
		})
	}
}

func newMetadataConsumerLayout(t *testing.T, layout string) metadataConsumerLayout {
	t.Helper()
	if layout == "alias" && runtime.GOOS == windowsGOOS {
		t.Skip("symlink creation requires privileges on some Windows builders")
	}
	tmp := t.TempDir()
	main := filepath.Join(tmp, "main")
	testutil.InitRepo(t, main)
	testutil.WriteFile(t, main, "initial.txt", "initial\n")
	testutil.GitAdd(t, main, "initial.txt")
	testutil.GitCommit(t, main, "initial")
	f := metadataConsumerLayout{root: main, gitDir: filepath.Join(main, ".git"), commonDir: filepath.Join(main, ".git")}
	switch layout {
	case "ordinary":
		return f
	case "separate":
		storage := filepath.Join(tmp, "storage")
		testutil.RunGit(t, main, "init", "--separate-git-dir", storage)
		return metadataConsumerLayout{root: main, gitDir: storage, commonDir: storage}
	case "dot-bare", "bare-storage":
		name := "storage"
		if layout == "dot-bare" {
			name = ".bare"
		}
		storage := filepath.Join(tmp, name)
		testutil.RunGit(t, tmp, "clone", "--bare", main, storage)
		linked := filepath.Join(tmp, "linked")
		testutil.RunGit(t, tmp, "--git-dir", storage, "worktree", "add", "-b", "linked", linked)
		return metadataConsumerLayout{root: linked, gitDir: filepath.Join(storage, "worktrees", "linked"), commonDir: storage, id: "linked"}
	case "submodule", "linked-submodule", "submodule-in-linked-superproject":
		source := filepath.Join(tmp, "source")
		testutil.InitRepo(t, source)
		testutil.WriteFile(t, source, "module.txt", "module\n")
		testutil.GitAdd(t, source, "module.txt")
		testutil.GitCommit(t, source, "module")
		testutil.RunGit(t, main, "-c", "protocol.file.allow=always", "submodule", "add", source, "module")
		testutil.GitAdd(t, main, ".")
		testutil.GitCommit(t, main, "add module")
		module := filepath.Join(main, "module")
		storage := filepath.Join(main, ".git", "modules", "module")
		if layout == "linked-submodule" {
			linked := filepath.Join(tmp, "module-linked")
			testutil.RunGit(t, module, "worktree", "add", "-b", "module-linked", linked)
			return metadataConsumerLayout{root: linked, gitDir: filepath.Join(storage, "worktrees", "module-linked"), commonDir: storage, id: "module-linked"}
		}
		if layout == "submodule-in-linked-superproject" {
			super := filepath.Join(tmp, "super-linked")
			testutil.RunGit(t, main, "worktree", "add", "-b", "super-linked", super)
			testutil.RunGit(t, super, "-c", "protocol.file.allow=always", "submodule", "update", "--init")
			module = filepath.Join(super, "module")
			storage = filepath.Join(main, ".git", "worktrees", "super-linked", "modules", "module")
		}
		return metadataConsumerLayout{root: module, gitDir: storage, commonDir: storage}
	}
	linked := filepath.Join(tmp, "linked")
	testutil.RunGit(t, main, "worktree", "add", "-b", "linked", linked)
	f.root, f.gitDir, f.id = linked, filepath.Join(main, ".git", "worktrees", "linked"), "linked"
	switch layout {
	case "relative-pointer":
		rel, err := filepath.Rel(linked, f.gitDir)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+rel+"\n"), 0o600))
	case "moved-registration":
		moved := filepath.Join(tmp, "moved")
		testutil.RunGit(t, main, "worktree", "move", linked, moved)
		f.root = moved
	case "alias":
		alias := filepath.Join(tmp, "alias")
		require.NoError(t, os.Symlink(main, alias))
		f.gitDir, f.commonDir = filepath.Join(alias, ".git", "worktrees", "linked"), filepath.Join(alias, ".git")
		require.NoError(t, os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+f.gitDir+"\n"), 0o600))
	}
	return f
}

func requireSameMetadataDirectory(t *testing.T, expected, actual string) {
	t.Helper()
	a, err := os.Stat(expected)
	require.NoError(t, err)
	b, err := os.Stat(actual)
	require.NoError(t, err)
	require.True(t, os.SameFile(a, b), "%q and %q must name the same directory", expected, actual)
}

func TestMetadataConsumerFailurePoliciesAndRepair(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	f := newMetadataConsumerLayout(t, "linked")
	t.Chdir(f.root)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	require.NoError(t, settings.ModifyClonePreferences(t.Context(), func(p *settings.ClonePreferences) error { p.ReviewDefaultProfile = "preserved"; return nil }))
	require.NoError(t, review.WritePendingReviewMarker(t.Context(), review.PendingReviewMarker{Prompt: "preserved"}))
	_, err := gitdir.OpenForCurrentWorktree(t.Context())
	require.NoError(t, err)
	commonFile := filepath.Join(f.gitDir, "commondir")
	original, err := os.ReadFile(commonFile)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(commonFile, []byte("missing\n"), 0o600))

	_, err = settings.LoadClonePreferences(t.Context())
	require.Error(t, err)
	called := false
	require.Error(t, settings.ModifyClonePreferences(t.Context(), func(*settings.ClonePreferences) error { called = true; return nil }))
	require.False(t, called)
	_, err = investigate.NewStateStore(t.Context())
	require.Error(t, err)
	_, err = investigate.NewLocalManifestStore(t.Context())
	require.Error(t, err)
	require.Error(t, review.WritePendingReviewMarker(t.Context(), review.PendingReviewMarker{Prompt: "must not overwrite"}))
	_, err = trailEnablementScopeHintPath(t.Context(), "metadata-agreement")
	require.Error(t, err)
	var output bytes.Buffer
	require.Error(t, previewCurrentHead(t.Context(), &output))
	require.Empty(t, output.String())
	require.Empty(t, resolveWorktreeBranch(t.Context(), f.root))
	require.Empty(t, getCurrentWorktreeHash(t.Context()))
	config, err := settings.Load(settings.WithWorktreeRoot(t.Context(), f.root))
	require.NoError(t, err, "an unresolved optional preferences layer must not fail settings load")
	require.Empty(t, config.ReviewDefaultProfile)
	discovery := codex.ResolveHookDiscovery(t.Context())
	require.Equal(t, codex.HookDiscoveryUnresolved, discovery.State)
	require.Error(t, discovery.Diagnostic)
	previousSpawn := trailRefreshSpawn
	spawned := ""
	trailRefreshSpawn = func(root string) { spawned = root }
	t.Cleanup(func() { trailRefreshSpawn = previousSpawn })
	spawnDetachedTrailEnablementRefresh(t.Context())
	require.NotEmpty(t, spawned, "unresolved throttle metadata preserves optional refresh spawning")

	require.NoError(t, os.WriteFile(commonFile, original, 0o600))
	prefs, err := settings.LoadClonePreferences(t.Context())
	require.NoError(t, err)
	require.Equal(t, "preserved", prefs.ReviewDefaultProfile)
	marker, found, err := review.ReadPendingReviewMarker(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "preserved", marker.Prompt)
	require.NoError(t, review.ClearPendingReviewMarker(t.Context()))
	require.Equal(t, "linked", resolveWorktreeBranch(t.Context(), f.root))
	require.Equal(t, checkpoint.HashWorktreeID("linked"), getCurrentWorktreeHash(t.Context()))
	require.Equal(t, codex.HookDiscoveryResolved, codex.ResolveHookDiscovery(t.Context()).State)
}

func TestMetadataAliasesPreserveLexicalConsumerPaths(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	f := newMetadataConsumerLayout(t, "alias")
	t.Chdir(f.root)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	prefs, err := settings.ClonePreferencesPath(t.Context())
	require.NoError(t, err)
	require.Equal(t, filepath.Join(f.commonDir, settings.ClonePreferencesFile), prefs)
	hint, err := trailEnablementScopeHintPath(t.Context(), "lexical-session")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(f.commonDir, session.SessionStateDirName, "lexical-session.trail-scope.json"), hint)
	base, err := trailWorktreeBaseRoot(t.Context())
	require.NoError(t, err)
	require.Equal(t, filepath.Dir(f.commonDir), base)
	var stdout, stderr bytes.Buffer
	target := defaultTrailWorktreePath(base, "topic", 12)
	printTrailWorktreeLocation(&stdout, &stderr, "ready", target)
	require.Equal(t, target+"\n", stdout.String())
	alias := filepath.Join(t.TempDir(), "visible-alias")
	require.NoError(t, os.Symlink(f.root, alias))
	state := &session.State{SessionID: "lexical-session", WorktreePath: alias, Phase: session.PhaseActive, StartedAt: time.Now()}
	store, err := session.NewStateStore(t.Context())
	require.NoError(t, err)
	require.NoError(t, store.Save(t.Context(), state))
	var status bytes.Buffer
	writeActiveSessions(t.Context(), &status, newStatusStyles(&status))
	require.Contains(t, status.String(), state.SessionID)
	loaded, err := store.Load(t.Context(), state.SessionID)
	require.NoError(t, err)
	require.Equal(t, alias, loaded.WorktreePath)
	require.Equal(t, "linked", resolveWorktreeBranch(t.Context(), alias))
}

func TestTrailCheckoutPreservesCallerRootSpelling(t *testing.T) {
	if runtime.GOOS == windowsGOOS {
		t.Skip("symlink creation requires privileges on Windows")
	}
	for _, subdir := range []string{"", "nested/child"} {
		t.Run(subdir, func(t *testing.T) {
			repo := newTrailWorktreeTestRepo(t)
			runGit(t, repo, "branch", "lexical-trail")
			alias := filepath.Join(t.TempDir(), "checkout-alias")
			require.NoError(t, os.Symlink(repo, alias))
			require.NoError(t, os.MkdirAll(filepath.Join(repo, "nested", "child"), 0o755))
			t.Chdir(filepath.Join(alias, filepath.FromSlash(subdir)))
			paths.ClearWorktreeRootCache()
			t.Cleanup(paths.ClearWorktreeRootCache)
			base, err := trailWorktreeBaseRoot(t.Context())
			require.NoError(t, err)
			require.Equal(t, alias, base)
			var stdout, stderr bytes.Buffer
			require.NoError(t, checkoutTrailWorktree(t.Context(), &stdout, &stderr, "lexical-trail", false, 9))
			require.Equal(t, defaultTrailWorktreePath(alias, "lexical-trail", 9)+"\n", stdout.String())
			other := t.TempDir()
			require.Equal(t, other, trailWorktreeRootSpelling(other), "CWD must not replace a different discovered root")
		})
	}
}
