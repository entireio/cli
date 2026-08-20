package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestDescribeSyncRemote covers every tier the note words differently, plus
// the fail-closed case where the election yields no name at all.
func TestDescribeSyncRemote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		topo   remoteTopology
		want   []string
		absent []string
	}{
		{
			name: "pinned by setting says nothing further",
			topo: remoteTopology{syncRemote: "publish", syncSource: strategy.SyncRemoteSourceConfig},
			want: []string{`"publish"`, "checkpoint_push_remote"},
			// The decision is already made; telling the user to set the key
			// they just set reads as a warning that something is unfinished.
			absent: []string{"Set strategy_options", "right now"},
		},
		{
			name:   "captured election is settled",
			topo:   remoteTopology{syncRemote: "fork", syncSource: strategy.SyncRemoteSourceObserved},
			want:   []string{`"fork"`, "push destination", "Set strategy_options.checkpoint_push_remote"},
			absent: []string{"right now"},
		},
		{
			name:   "open election says a push can move it",
			topo:   remoteTopology{syncRemote: "origin", syncSource: strategy.SyncRemoteSourceDefault},
			want:   []string{`right now "origin"`, "entire status", "Set strategy_options.checkpoint_push_remote"},
			absent: []string{"no session history"},
		},
		{
			name: "fail-closed election reports sync is off",
			topo: remoteTopology{syncErr: errors.New(`checkpoint_push_remote "gone" is not a configured git remote`)},
			want: []string{"NOT syncing", `"gone"`},
			// No destination may be named when the push side has none.
			absent: []string{"sync to one remote"},
		},
		{
			name:   "no remotes at all stays silent",
			topo:   remoteTopology{},
			absent: []string{"Checkpoints"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var b strings.Builder
			tt.topo.describeSyncRemote(&b)
			for _, want := range tt.want {
				if !strings.Contains(b.String(), want) {
					t.Errorf("note should mention %q:\n%s", want, b.String())
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(b.String(), absent) {
					t.Errorf("note should not mention %q:\n%s", absent, b.String())
				}
			}
		})
	}
}
