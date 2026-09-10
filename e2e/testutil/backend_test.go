package testutil

import "testing"

func TestCheckpointStoreMode(t *testing.T) {
	// Each case changes process-wide environment variables, so it cannot run
	// in parallel with the other cases.
	for _, tc := range []struct {
		name      string
		selected  string
		inherited string
		want      string
	}{
		{"default", "", "", storeModeGitRefs},
		{"explicit refs", storeModeGitRefs, "", storeModeGitRefs},
		{"legacy", storeModeGitBranch, "", storeModeGitBranch},
		{"inherited legacy", "", storeModeGitBranch, storeModeGitBranch},
		{"explicit legacy beats inherited refs", storeModeGitBranch, storeModeGitRefs, storeModeGitBranch},
		{"explicit refs beats inherited legacy", storeModeGitRefs, storeModeGitBranch, storeModeGitRefs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("E2E_CHECKPOINT_STORE", tc.selected)
			t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", tc.inherited)
			if got := checkpointStoreMode(); got != tc.want {
				t.Errorf("checkpointStoreMode() = %q, want %q", got, tc.want)
			}
		})
	}
}
