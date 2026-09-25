//go:build integration

package integration

import "testing"

// TestCheckpointDestinationChange_RedeliversToTheNewStore is the end-to-end form
// of the re-sync: a checkpoint delivered to one store must follow the user when
// they point checkpoint sync somewhere else.
//
// This is what claiming a refused checkpoint_remote looks like in practice —
// every checkpoint written before the fix is sitting in the wrong store, and
// fixing the setting has to bring them along rather than only helping future
// ones.
//
// Both backends, because they get there differently and only one needs code:
// git-branch pushes the whole entire/checkpoints/v1 branch, so the history
// travels by construction, while git-refs empties its push queue on a
// successful push and would otherwise have nothing left to offer the new store.
func TestCheckpointDestinationChange_RedeliversToTheNewStore(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		firstStore := env.SetupNamedBareRemote("first")
		secondStore := env.SetupNamedBareRemote("second")

		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{"checkpoint_push_remote": "first"},
		})

		checkpointID := createCheckpointedCommit(t, env, "Add parser", "parser.go", "package parser", "Add parser")
		if checkpointID == "" {
			t.Fatal("should have a checkpoint ID after condensation")
		}

		env.RunPrePush("first")
		if !env.CheckpointExistsOnRemote(firstStore, checkpointID) {
			t.Fatalf("checkpoint %s should be on the first store", checkpointID)
		}
		if env.CheckpointsPresentOnRemote(secondStore) {
			t.Fatal("the second store should be untouched so far")
		}

		// The user points checkpoint sync at a different store. Nothing new is
		// written — the only thing that can carry the existing checkpoint to
		// the new store is the destination change itself.
		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{"checkpoint_push_remote": "second"},
		})
		env.RunPrePush("second")

		if !env.CheckpointExistsOnRemote(secondStore, checkpointID) {
			t.Errorf("checkpoint %s should have followed the destination change to the second store", checkpointID)
		}
	})
}
