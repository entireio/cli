//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// =============================================================================
// `entire checkpoint migrate` end to end
//
// origin (GitHub in real life, a bare repo here) holds the checkpoints an
// earlier pre-push carried; entireRemoteName is an Entire remote simulated with the
// insteadOf trick (see setupEntireRemote). The command must report without
// changing anything when nobody can answer prompts, migrate on --yes, and
// clean the old remote only on --remove.
// =============================================================================

// entireRemoteName is the Entire remote in these fixtures.
const entireRemoteName = "entire"

// checkpointRefsOnRemote reports whether the bare remote holds any
// per-checkpoint ref, whichever store the env selects.
func checkpointRefsOnRemote(t *testing.T, bareDir string) bool {
	t.Helper()
	return strings.TrimSpace(testutil.RunGit(t, bareDir, "for-each-ref", checkpointRefPrefix)) != ""
}

// seedCheckpointOnOrigin creates one checkpointed commit and pushes it,
// checkpoint included, to origin before the Entire remote exists — the state
// every user upgrading into the Entire tier starts from.
func seedCheckpointOnOrigin(t *testing.T, env *TestEnv, bareOrigin string) string {
	t.Helper()
	checkpointID := createCheckpointedCommit(t, env, "Add sync module", "sync.go", "package sync", "Add sync module")
	env.GitPush(originRemoteName, "HEAD")
	env.RunPrePush(originRemoteName)
	if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
		t.Fatalf("checkpoint %s should be on origin before the migration", checkpointID)
	}
	return checkpointID
}

// checkpointRefOnRemote reports whether the per-checkpoint ref landed on the
// bare remote. The Entire remote receives refs whichever store the env selects
// (sync converts a v1 branch first), so this is deliberately not the
// backend-aware CheckpointExistsOnRemote.
func checkpointRefOnRemote(t *testing.T, bareDir, checkpointID string) bool {
	t.Helper()
	return refExists(t, bareDir, checkpointRefName(checkpointID))
}

func syncJSON(t *testing.T, env *TestEnv, args ...string) map[string]any {
	t.Helper()
	out := env.RunCLI(append([]string{"checkpoint", "migrate", "--json"}, args...)...)
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("parse checkpoint migrate --json: %v\nOutput: %s", err, out)
	}
	return doc
}

func TestCheckpointMigrate_NonInteractiveReportsPlanAndWritesNothing(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		bareOrigin := env.SetupBareRemote()
		checkpointID := seedCheckpointOnOrigin(t, env, bareOrigin)
		bareEntire := setupEntireRemote(t, env, entireRemoteName)

		out := env.RunCLI("checkpoint", "migrate")
		for _, want := range []string{"entire (your Entire remote)", "entire checkpoint migrate --yes", "--remove"} {
			if !strings.Contains(out, want) {
				t.Errorf("expected %q in report, got:\n%s", want, out)
			}
		}
		if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
			t.Error("a bare run must leave origin untouched")
		}
		if checkpointRefsOnRemote(t, bareEntire) {
			t.Error("a bare run must not push to the Entire remote")
		}

		doc := syncJSON(t, env)
		if doc["state"] != "migrate" {
			t.Errorf("state = %v, want migrate; doc: %v", doc["state"], doc)
		}
	})
}

func TestCheckpointMigrate_YesMigratesWithoutRemoving(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		bareOrigin := env.SetupBareRemote()
		checkpointID := seedCheckpointOnOrigin(t, env, bareOrigin)
		bareEntire := setupEntireRemote(t, env, entireRemoteName)

		out := env.RunCLI("checkpoint", "migrate", "--yes")
		if !strings.Contains(out, "Your checkpoints now live on Entire.") {
			t.Errorf("expected the celebration, got:\n%s", out)
		}
		if !checkpointRefOnRemote(t, bareEntire, checkpointID) {
			t.Errorf("checkpoint %s should now be on the Entire remote; output:\n%s", checkpointID, out)
		}
		if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
			t.Error("--yes alone must not remove anything from origin")
		}
		if !strings.Contains(out, "Older copies stay on origin") {
			t.Errorf("expected the remaining-copies note, got:\n%s", out)
		}
		// Every checkpoint is a ref locally now, whichever store the env
		// selects; the settings write is skipped under the env override.
		refs := testutil.RunGit(t, env.RepoDir, "for-each-ref", checkpointRefPrefix)
		if strings.TrimSpace(refs) == "" {
			t.Error("expected per-checkpoint refs after the migration")
		}
		if backend == StoreGitBranch && !strings.Contains(out, "ENTIRE_CHECKPOINTS_PRIMARY is set") {
			t.Errorf("git-branch conversion under the env override must say the backend was not written, got:\n%s", out)
		}
	})
}

func TestCheckpointMigrate_YesRemoveCleansOriginAndIsIdempotent(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		bareOrigin := env.SetupBareRemote()
		checkpointID := seedCheckpointOnOrigin(t, env, bareOrigin)
		bareEntire := setupEntireRemote(t, env, entireRemoteName)

		out := env.RunCLI("checkpoint", "migrate", "--yes", "--remove")
		if !checkpointRefOnRemote(t, bareEntire, checkpointID) {
			t.Errorf("checkpoint %s should be on the Entire remote; output:\n%s", checkpointID, out)
		}
		if checkpointRefsOnRemote(t, bareOrigin) {
			t.Errorf("origin should hold no checkpoint refs after --remove; output:\n%s", out)
		}
		if !entireStateFileExists(t, env) {
			t.Error("the migration ledger should be recorded")
		}
		v1OnOrigin := func() bool {
			return strings.TrimSpace(testutil.RunGit(t, bareOrigin, "for-each-ref", "refs/heads/entire/checkpoints/v1")) != ""
		}
		if backend == StoreGitBranch {
			// ENTIRE_CHECKPOINTS_PRIMARY pins git-branch for this env, standing
			// in for a repo whose settings could not be flipped: readers still
			// use the branch, so the old remote keeps it and the run says so.
			if !v1OnOrigin() {
				t.Error("the v1 branch must stay on origin while the store is still git-branch")
			}
			if !strings.Contains(out, "entire/checkpoints/v1 branch stays on the old remote") {
				t.Errorf("expected the kept-branch note, got:\n%s", out)
			}
			// Once the store is git-refs (here: the env selects it the way the
			// written setting would), the next run converts and cleans it up.
			env.CheckpointStore = StoreGitRefs
			out = env.RunCLI("checkpoint", "migrate", "--yes", "--remove")
			if v1OnOrigin() {
				t.Errorf("the v1 branch should be deleted once git-refs is the store; output:\n%s", out)
			}
		} else if v1OnOrigin() {
			t.Error("a git-refs seed never created a v1 branch on origin")
		}
		if strings.Contains(out, "Older copies stay") {
			t.Errorf("nothing should remain on origin, got:\n%s", out)
		}

		status := statusSyncJSONOutput(t, env)
		if status.CheckpointSyncRemoteSource != entireRemoteName || status.CheckpointSyncRemote != entireRemoteName {
			t.Errorf("status should report the Entire remote, got %+v", status)
		}

		// A further run finds everything home and changes nothing.
		env.CheckpointStore = StoreGitRefs
		again := env.RunCLI("checkpoint", "migrate")
		if !strings.Contains(again, "Checkpoints live on entire (your Entire remote)") {
			t.Errorf("rerun should report already home, got:\n%s", again)
		}
	})
}

func TestCheckpointMigrate_ToWritesLocalSettingOnlyWhenNeeded(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()
	env.SetupNamedBareRemote("fork")

	// origin is the default election: naming it records nothing.
	env.RunCLI("checkpoint", "migrate", "--to", "origin", "--dry-run")
	if env.FileExists(".entire/settings.local.json") {
		t.Error("--to origin must not write settings when origin is already elected")
	}

	// fork is not: --yes records it in the per-clone file.
	out := env.RunCLI("checkpoint", "migrate", "--to", "fork", "--yes")
	if !strings.Contains(out, "Recorded strategy_options.checkpoint_push_remote in .entire/settings.local.json") {
		t.Errorf("expected the recorded line, got:\n%s", out)
	}
	local := env.ReadFile(".entire/settings.local.json")
	if !strings.Contains(local, `"checkpoint_push_remote": "fork"`) {
		t.Errorf("settings.local.json should pin fork, got:\n%s", local)
	}
	status := statusSyncJSONOutput(t, env)
	if status.CheckpointSyncRemote != "fork" || status.CheckpointSyncRemoteSource != "config" {
		t.Errorf("status should report fork as configured, got %+v", status)
	}
}

func TestCheckpointMigrate_FailClosedSuggestsTo(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()
	testutil.WriteCheckpointPushRemoteSetting(t, env.RepoDir, "gone")

	out, err := env.RunCLIWithError("checkpoint", "migrate")
	if err == nil {
		t.Fatalf("a fail-closed election must exit non-zero, got:\n%s", out)
	}
	for _, want := range []string{"Checkpoints are NOT syncing", "entire checkpoint migrate --to <remote>"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q, got:\n%s", want, out)
		}
	}
}

func TestCheckpointMigrate_TwoEntireRemotesRequireTo(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.SetupBareRemote()
	setupEntireRemote(t, env, "entire-a")
	setupEntireRemote(t, env, "entire-b")

	out, err := env.RunCLIWithError("checkpoint", "migrate")
	if err == nil {
		t.Fatalf("several Entire remotes need --to non-interactively, got:\n%s", out)
	}
	if !strings.Contains(out, "This repo has 2 Entire remotes (entire-a, entire-b)") {
		t.Errorf("expected the choice explanation, got:\n%s", out)
	}
}

// TestCheckpointMigrate_FreshCloneMigratesOnFirstRun is the state every real
// user starts from: a clone that holds none of the checkpoint refs (git clone
// does not fetch refs/entire/checkpoints/*), no v1 branch, and an empty push
// queue. The first run must fetch, queue, push and verify — not report that
// nothing arrived.
func TestCheckpointMigrate_FreshCloneMigratesOnFirstRun(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		bareOrigin := env.SetupBareRemote()
		checkpointID := seedCheckpointOnOrigin(t, env, bareOrigin)
		bareEntire := setupEntireRemote(t, env, entireRemoteName)

		// Forget everything local: the checkpoint refs, the v1 branch, the
		// remote-tracking v1 ref and the push queue.
		for _, line := range strings.Split(strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "for-each-ref", "--format=%(refname)", checkpointRefPrefix, "refs/heads/entire/checkpoints/v1", "refs/remotes/origin/entire/checkpoints/v1")), "\n") {
			if line != "" {
				testutil.RunGit(t, env.RepoDir, "update-ref", "-d", line)
			}
		}
		_ = os.Remove(filepath.Join(env.RepoDir, ".git", "entire-checkpoint-push-queue.jsonl"))
		if env.CheckpointsPresentLocally() {
			t.Fatal("test setup: the clone should hold no checkpoint artifacts")
		}

		out, err := env.RunCLIWithError("checkpoint", "migrate", "--yes")
		if err != nil {
			t.Fatalf("first run in a fresh clone must succeed; output:\n%s", out)
		}
		if !checkpointRefOnRemote(t, bareEntire, checkpointID) {
			t.Errorf("checkpoint %s should be on the Entire remote after one run; output:\n%s", checkpointID, out)
		}
		if strings.Contains(out, "did not arrive") {
			t.Errorf("no verify failure on the first run; output:\n%s", out)
		}
	})
}
