package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStateStore_Load_PreservesPendingContentAfterExpiry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		state    State
		retained bool
	}{
		{name: "steps", state: State{StepCount: 1}, retained: true},
		{name: "files", state: State{FilesTouched: []string{"main.go"}}, retained: true},
		{name: "tasks", state: State{TaskRecords: []TaskRecord{{}}}, retained: true},
		{name: "pending-turn", state: State{FullyCondensed: true, TurnCheckpointIDs: []string{"a1b2c3d4e5f6"}}, retained: true},
		{name: "attempt", state: State{CondensationAttempt: &CondensationAttempt{}}, retained: true},
		{name: "empty", state: State{}},
		{name: "condensed", state: State{FullyCondensed: true, StepCount: 1, FilesTouched: []string{"main.go"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), SessionStateDirName)
			store := NewStateStoreWithDir(dir)
			tc.state.SessionID = "expired"
			tc.state.StartedAt = time.Now().Add(-8 * 24 * time.Hour)
			require.NoError(t, store.Save(t.Context(), &tc.state))
			state, err := store.Load(t.Context(), tc.state.SessionID)
			require.NoError(t, err)
			if tc.retained {
				require.NotNil(t, state)
				require.FileExists(t, filepath.Join(dir, "expired.json"))
			} else {
				require.Nil(t, state)
				require.NoFileExists(t, filepath.Join(dir, "expired.json"))
			}
		})
	}
}

func TestStateStore_ListStrict_RefusesIncompleteInventory(t *testing.T) {
	t.Parallel()
	for _, directory := range []bool{false, true} {
		name := "malformed"
		if directory {
			name = "directory"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := filepath.Join(t.TempDir(), SessionStateDirName)
			store := NewStateStoreWithDir(dir)
			require.NoError(t, store.Save(ctx, &State{SessionID: "valid", StartedAt: time.Now()}))
			bad := filepath.Join(dir, "unreadable.json")
			if directory {
				require.NoError(t, os.Mkdir(bad, 0o700))
			} else {
				require.NoError(t, os.WriteFile(bad, []byte(`{"session_id":`), 0o600))
			}
			states, err := store.List(ctx)
			require.NoError(t, err)
			require.Len(t, states, 1)
			states, err = store.ListStrict(ctx)
			require.ErrorContains(t, err, "unreadable")
			require.Nil(t, states)
			require.FileExists(t, filepath.Join(dir, "valid.json"))
			require.NoError(t, os.Remove(bad))
			states, err = store.ListStrict(ctx)
			require.NoError(t, err)
			require.Len(t, states, 1)
		})
	}
}
