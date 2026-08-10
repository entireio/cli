//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// =============================================================================
// Single-remote checkpoint sync (ENT-1451)
//
// Checkpoint data syncs to exactly one elected git remote:
// strategy_options.checkpoint_push_remote (fail-closed when the named remote
// is not configured) -> origin -> sole remote -> first
// remote in .git/config order. The branch's tracking config deliberately does
// not participate; see TestCheckpointSyncRemote_BranchTrackingDoesNotReroute.
// The pre-push gate drops checkpoint sync for
// every other remote and for raw-URL pushes, on both backends. The dedicated
// checkpoint_remote URL mode is exempt. These are end-to-end acceptance tests
// through the simulated hook flow; the election precedence itself is
// unit-tested in strategy/checkpoint_sync_remote_test.go.
// =============================================================================

// remoteTarget names a configured remote and the bare repo backing it.
type remoteTarget struct {
	name    string
	bareDir string
}

// queuedCheckpointRefCount returns the git-refs push-discovery queue length
// for the test repo. The queue lives in the git common dir, which for the
// plain (non-worktree) TestEnv repos is .git.
func queuedCheckpointRefCount(t *testing.T, env *TestEnv) int {
	t.Helper()
	refs, err := checkpoint.NewPushQueue(filepath.Join(env.RepoDir, ".git")).Peek()
	if err != nil {
		t.Fatalf("read push queue: %v", err)
	}
	return len(refs)
}

// assertSingleRemoteRouting drives the pre-push flow first against the
// non-elected remote (no checkpoint data may land there; on git-refs the push
// queue must survive), then against the elected remote (the checkpoint lands
// and the queue drains). Shared by the default-election and config-override
// scenarios, which are mirror images of each other.
func assertSingleRemoteRouting(t *testing.T, env *TestEnv, checkpointID string, gated, elected remoteTarget) {
	t.Helper()

	if checkpointID == "" {
		t.Fatal("should have a checkpoint ID after condensation")
	}
	if !env.CheckpointsPresentLocally() {
		t.Fatal("checkpoints should exist locally after condensation")
	}
	if env.usingGitRefs() && queuedCheckpointRefCount(t, env) == 0 {
		t.Fatal("git-refs push queue should have entries after condensation")
	}

	// Pre-push to the non-elected remote: gated, no checkpoint data escapes.
	env.RunPrePush(gated.name)
	if env.CheckpointsPresentOnRemote(gated.bareDir) {
		t.Errorf("checkpoints should NOT be on non-elected remote %q", gated.name)
	}
	if env.usingGitRefs() && queuedCheckpointRefCount(t, env) == 0 {
		t.Error("push queue should be preserved after a gated pre-push")
	}

	// Pre-push to the elected remote: checkpoint data lands, queue drains.
	env.RunPrePush(elected.name)
	if !env.CheckpointExistsOnRemote(elected.bareDir, checkpointID) {
		t.Errorf("checkpoint %s should be on elected remote %q", checkpointID, elected.name)
	}
	if env.usingGitRefs() && queuedCheckpointRefCount(t, env) != 0 {
		t.Error("push queue should drain after pushing to the elected remote")
	}
	if env.CheckpointsPresentOnRemote(gated.bareDir) {
		t.Errorf("non-elected remote %q must never receive checkpoint data", gated.name)
	}
}

// TestCheckpointSyncRemote_DefaultElection_OnlyOriginReceivesCheckpoints
// verifies that with two configured remotes and no override, checkpoint data
// syncs only to origin (the default election): a pre-push for "publish"
// carries nothing, a pre-push for "origin" delivers the checkpoint.
func TestCheckpointSyncRemote_DefaultElection_OnlyOriginReceivesCheckpoints(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		barePublish := env.SetupNamedBareRemote("publish")
		checkpointID := createCheckpointedCommit(t, env, "Add gate module", "gate.go", "package gate", "Add gate module")

		assertSingleRemoteRouting(t, env, checkpointID,
			remoteTarget{name: "publish", bareDir: barePublish},
			remoteTarget{name: "origin", bareDir: bareOrigin})
	})
}

// TestCheckpointSyncRemote_ConfigOverride_RoutesToNamedRemote verifies the
// mirror image: with checkpoint_push_remote set to "publish", origin is the
// gated remote and publish receives the checkpoint data.
func TestCheckpointSyncRemote_ConfigOverride_RoutesToNamedRemote(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		barePublish := env.SetupNamedBareRemote("publish")

		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{
				"checkpoint_push_remote": "publish",
			},
		})

		checkpointID := createCheckpointedCommit(t, env, "Add router module", "router.go", "package router", "Add router module")

		assertSingleRemoteRouting(t, env, checkpointID,
			remoteTarget{name: "origin", bareDir: bareOrigin},
			remoteTarget{name: "publish", bareDir: barePublish})

		// Status reflects the override election.
		st := statusSyncJSONOutput(t, env)
		if st.CheckpointSyncRemote != "publish" || st.CheckpointSyncRemoteSource != "config" {
			t.Errorf("status should report remote %q from source %q, got %q from %q",
				"publish", "config", st.CheckpointSyncRemote, st.CheckpointSyncRemoteSource)
		}
	})
}

// TestCheckpointSyncRemote_BranchTrackingDoesNotReroute pins the regression
// that removed the tracking tier before merge: a branch tracking a non-origin
// remote must NOT move checkpoint sync there.
//
// Election is compared against the remote of the push being made, so electing
// the tracking remote made every push to a different remote a silent no-op —
// caught by TestAlternates_RelativeObjectAlternate_CheckpointSync, which
// clones with `-o base` and pushes checkpoints to a separately added origin.
// It also elected a remote the read paths cannot see: resume and explain
// resolve checkpoints through origin's remote-tracking refs.
//
// The fork topology this tier was meant to serve — origin is the unpushable
// base repo, you push to your own fork — is served explicitly by
// checkpoint_push_remote (TestCheckpointSyncRemote_ConfigOverride_RoutesToNamedRemote).
func TestCheckpointSyncRemote_BranchTrackingDoesNotReroute(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		// SetupNamedBareRemote pushes with `-u`, so the branch ends up
		// tracking "upstream" — the exact config that used to win the
		// election. Origin must still be elected.
		bareOrigin := env.SetupBareRemote()
		bareUpstream := env.SetupNamedBareRemote("upstream")

		checkpointID := createCheckpointedCommit(t, env, "Add fork module", "fork.go", "package fork", "Add fork module")

		assertSingleRemoteRouting(t, env, checkpointID,
			remoteTarget{name: "upstream", bareDir: bareUpstream},
			remoteTarget{name: "origin", bareDir: bareOrigin})

		const wantRemote, wantSource = "origin", "default"
		st := statusSyncJSONOutput(t, env)
		if st.CheckpointSyncRemote != wantRemote || st.CheckpointSyncRemoteSource != wantSource {
			t.Errorf("status should report remote %q from source %q, got %q from %q",
				wantRemote, wantSource, st.CheckpointSyncRemote, st.CheckpointSyncRemoteSource)
		}
	})
}

// TestCheckpointSyncRemote_MisconfiguredSettingFailsClosed verifies that when
// checkpoint_push_remote names a remote that is not configured, checkpoint
// sync is disabled for every remote (fail-closed) while the user's own push
// flow keeps working: the real pre-push hook exits zero and the branch push
// succeeds.
func TestCheckpointSyncRemote_MisconfiguredSettingFailsClosed(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		barePublish := env.SetupNamedBareRemote("publish")

		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{
				"checkpoint_push_remote": "gone",
			},
		})

		_ = createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
		if !env.CheckpointsPresentLocally() {
			t.Fatal("checkpoints should exist locally after condensation")
		}

		// The user's own push must succeed with the real pre-push hook
		// installed — the misconfiguration silently skips checkpoint sync
		// but never breaks the push itself.
		env.GitPushWithHooks("origin", "HEAD")
		if env.CheckpointsPresentOnRemote(bareOrigin) {
			t.Error("checkpoints should NOT reach origin when checkpoint_push_remote is misconfigured")
		}

		if err := env.RunPrePushWithError("publish"); err != nil {
			t.Errorf("pre-push must not fail on a fail-closed misconfiguration: %v", err)
		}
		if env.CheckpointsPresentOnRemote(barePublish) {
			t.Error("checkpoints should NOT reach publish when checkpoint_push_remote is misconfigured")
		}

		// git-refs: the refs stay queued for whenever the setting is fixed.
		if env.usingGitRefs() && queuedCheckpointRefCount(t, env) == 0 {
			t.Error("push queue should be preserved while checkpoint sync is fail-closed")
		}

		// Status is the user's signal that sync is silently disabled: the
		// fail-closed error names the setting and no remote is elected.
		st := statusSyncJSONOutput(t, env)
		if st.CheckpointSyncError == "" || !strings.Contains(st.CheckpointSyncError, "checkpoint_push_remote") {
			t.Errorf("checkpoint_sync_error should mention checkpoint_push_remote, got %q", st.CheckpointSyncError)
		}
		if st.CheckpointSyncRemote != "" {
			t.Errorf("checkpoint_sync_remote should be empty when fail-closed, got %q", st.CheckpointSyncRemote)
		}
	})
}

// TestCheckpointSyncRemote_RawURLPushCarriesNoCheckpointData verifies that a
// push whose destination is a raw path/URL (git passes it verbatim as the
// hook's remote arg, and no such destination can be the elected remote) never
// carries checkpoint data — and that the elected remote still receives it on
// the next push.
func TestCheckpointSyncRemote_RawURLPushCarriesNoCheckpointData(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		rawDir := initUnregisteredBareRepo(t)

		checkpointID := createCheckpointedCommit(t, env, "Add worker module", "worker.go", "package worker", "Add worker module")

		env.RunPrePush(rawDir)
		if env.CheckpointsPresentOnRemote(rawDir) {
			t.Error("checkpoints should NOT land on a raw-URL push destination")
		}
		if env.usingGitRefs() && queuedCheckpointRefCount(t, env) == 0 {
			t.Error("push queue should be preserved after a raw-URL push")
		}

		// The elected remote still receives the checkpoint afterwards.
		env.RunPrePush("origin")
		if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
			t.Errorf("checkpoint %s should reach origin after the raw-URL push was gated", checkpointID)
		}
	})
}

// TestCheckpointSyncRemote_StatusReportsDestinationAndUnpushed verifies that
// after a gated push (nothing synced) `entire status` names the elected
// destination and counts the unpushed checkpoint, in both text and --json,
// and that the counter clears once the elected remote receives the data.
func TestCheckpointSyncRemote_StatusReportsDestinationAndUnpushed(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		_ = env.SetupBareRemote()
		_ = env.SetupNamedBareRemote("publish")
		_ = createCheckpointedCommit(t, env, "Add status module", "status.go", "package status", "Add status module")

		// Gated push: publish is not the elected remote, so nothing syncs.
		env.RunPrePush("publish")

		text := env.RunCLI("status")
		if !strings.Contains(text, "Checkpoints sync to: origin") {
			t.Errorf("status should name the elected sync remote, got:\n%s", text)
		}
		if !strings.Contains(text, "not yet on origin") {
			t.Errorf("status should show an unpushed counter after a gated push, got:\n%s", text)
		}

		st := statusSyncJSONOutput(t, env)
		if st.CheckpointSyncRemote != "origin" {
			t.Errorf("checkpoint_sync_remote = %q, want %q", st.CheckpointSyncRemote, "origin")
		}
		if st.CheckpointSyncRemoteSource != "default" {
			t.Errorf("checkpoint_sync_remote_source = %q, want %q", st.CheckpointSyncRemoteSource, "default")
		}
		if st.UnpushedCheckpoints < 1 {
			t.Errorf("unpushed_checkpoints = %d, want >= 1", st.UnpushedCheckpoints)
		}

		// Push to the elected remote: the counter clears.
		env.RunPrePush("origin")

		text = env.RunCLI("status")
		if !strings.Contains(text, "Checkpoints sync to: origin") {
			t.Errorf("status should still name the sync remote after pushing, got:\n%s", text)
		}
		if strings.Contains(text, "not yet on origin") {
			t.Errorf("unpushed counter should clear after pushing to origin, got:\n%s", text)
		}

		st = statusSyncJSONOutput(t, env)
		if st.CheckpointSyncRemote != "origin" {
			t.Errorf("checkpoint_sync_remote = %q after push, want %q", st.CheckpointSyncRemote, "origin")
		}
		if st.UnpushedCheckpoints != 0 {
			t.Errorf("unpushed_checkpoints = %d after push, want 0", st.UnpushedCheckpoints)
		}
	})
}

// TestCheckpointSyncRemote_DedicatedCheckpointRemoteExemptFromGate verifies
// the one exemption from the single-remote gate: a dedicated checkpoint_remote
// URL is a metadata store addressed directly, so a push to a non-elected
// remote still syncs checkpoint data — to the dedicated store, and only there.
//
// git-branch only: checkpoint_remote URL routing for git-refs per-checkpoint
// refs is separate future work (test plan B5), same scoping as
// TestHTTPS_CheckpointRemoteRoutesToSeparateRepo. The URL is derived from the
// push remote's HTTPS URL, so this reuses the smart-HTTP fixture.
// TestCheckpointSyncRemote_InheritedCheckpointRemoteDoesNotBypassGate is the
// end-to-end guard for the "cloned the base repo, added my fork" contributor
// topology.
//
// It matters at this level because of how the pre-push gate is written:
//
//	if !ps.hasCheckpointURL() && !checkpointSyncAllowedForRemote(ctx, ps.remote)
//
// A dedicated checkpoint_remote SKIPS the gate. So while the ownership check
// accepted an inherited committed setting, a contributor pushing to their own
// fork sailed straight past the gate and delivered their session transcripts to
// the upstream project's checkpoint repo. Ownership now also requires the push
// destination's owner to match, which makes hasCheckpointURL false here and lets
// the gate do its job.
func TestCheckpointSyncRemote_InheritedCheckpointRemoteDoesNotBypassGate(t *testing.T) {
	t.Parallel()

	srv := startGitHTTPSServer(t, "acme/app", "contributor/app", "acme/checkpoints")
	env := NewFeatureBranchEnv(t)

	upstreamBare := srv.BareDirs["acme/app"]
	forkBare := srv.BareDirs["contributor/app"]
	checkpointBare := srv.BareDirs["acme/checkpoints"]

	// origin is the UPSTREAM base repo the contributor cloned; myfork is their
	// own. Note origin's owner ("acme") matches the checkpoint repo's owner,
	// which is precisely why an origin-only ownership check was fooled.
	seedBareRepo(t, env, upstreamBare, srv.URL+"/acme/app.git")
	testutil.AddRemote(t, env.RepoDir, "myfork", srv.URL+"/contributor/app.git")
	env.setGitConfigBaseline()
	env.ExtraEnv = srv.tokenEnv("fork-provenance-token")

	// The setting is committed in .entire/settings.json — i.e. inherited by
	// cloning, not chosen by this developer.
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "github",
				"repo":     "acme/checkpoints",
			},
		},
	})

	_ = createCheckpointedCommit(t, env, "Add contrib module", "contrib.go", "package contrib", "Add contrib module")

	// The contributor's actual workflow: push to their fork. myfork is not the
	// elected sync remote (origin wins the default election), so with the
	// inherited setting correctly ignored the gate drops checkpoint sync
	// entirely — nothing reaches upstream's checkpoint repo.
	env.RunPrePush("myfork")

	if env.BranchExistsOnRemote(checkpointBare, paths.MetadataBranchName) {
		t.Error("upstream's checkpoint repo must not receive a contributor's session data from an inherited setting")
	}
	if env.BranchExistsOnRemote(upstreamBare, paths.MetadataBranchName) {
		t.Error("upstream must not receive the checkpoint branch")
	}
	if env.BranchExistsOnRemote(forkBare, paths.MetadataBranchName) {
		t.Error("the fork is not the elected remote, so the gate must drop checkpoint sync there too")
	}
}

func TestCheckpointSyncRemote_DedicatedCheckpointRemoteExemptFromGate(t *testing.T) {
	t.Parallel()

	srv := startGitHTTPSServer(t, "testorg/main-repo", "testorg/publish-repo", "testorg/checkpoints")
	env := NewFeatureBranchEnv(t)

	mainBare := srv.BareDirs["testorg/main-repo"]
	publishBare := srv.BareDirs["testorg/publish-repo"]
	checkpointBare := srv.BareDirs["testorg/checkpoints"]

	// origin -> main repo over HTTPS; publish -> a second HTTPS remote that is
	// NOT the elected sync remote (origin wins the default election).
	seedBareRepo(t, env, mainBare, srv.URL+"/testorg/main-repo.git")
	testutil.AddRemote(t, env.RepoDir, "publish", srv.URL+"/testorg/publish-repo.git")
	env.setGitConfigBaseline()
	env.ExtraEnv = srv.tokenEnv("gate-exemption-token")

	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "github",
				"repo":     "testorg/checkpoints",
			},
		},
	})

	checkpointID := createCheckpointedCommit(t, env, "Add dedicated module", "dedicated.go", "package dedicated", "Add dedicated module")

	// Pre-push for the non-elected remote: the dedicated URL exemption applies,
	// so the checkpoint syncs to the dedicated store.
	env.RunPrePush("publish")

	if !env.BranchExistsOnRemote(checkpointBare, paths.MetadataBranchName) {
		t.Fatal("dedicated checkpoint store should have the checkpoint branch")
	}
	if !fileExistsOnRemoteBranch(t, checkpointBare, CheckpointSummaryPath(checkpointID)) {
		t.Errorf("checkpoint %s should be on the dedicated checkpoint store", checkpointID)
	}
	if env.BranchExistsOnRemote(publishBare, paths.MetadataBranchName) {
		t.Error("the pushed-to remote must not receive the checkpoint branch")
	}
	if env.BranchExistsOnRemote(mainBare, paths.MetadataBranchName) {
		t.Error("origin must not receive the checkpoint branch in dedicated mode")
	}
}

// initUnregisteredBareRepo creates a bare repo that is deliberately NOT added
// as a remote of any test repo, for raw-URL push scenarios.
func initUnregisteredBareRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	cmd := exec.CommandContext(t.Context(), "git", "init", "--bare")
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, output)
	}
	return dir
}

// statusSyncJSON is the checkpoint-sync subset of `entire status --json`.
type statusSyncJSON struct {
	CheckpointSyncRemote       string `json:"checkpoint_sync_remote"`
	CheckpointSyncRemoteSource string `json:"checkpoint_sync_remote_source"`
	CheckpointSyncError        string `json:"checkpoint_sync_error"`
	UnpushedCheckpoints        int    `json:"unpushed_checkpoints"`
}

// statusSyncJSONOutput runs `entire status --json` and parses stdout (stderr
// is kept separate so hints can't corrupt the JSON).
func statusSyncJSONOutput(t *testing.T, env *TestEnv) statusSyncJSON {
	t.Helper()

	cmd := execx.NonInteractive(t.Context(), getTestBinary(), "status", "--json")
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("status --json failed: %v\nStderr: %s", err, stderr.String())
	}

	var parsed statusSyncJSON
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parse status --json: %v\nOutput: %s", err, out)
	}
	return parsed
}
