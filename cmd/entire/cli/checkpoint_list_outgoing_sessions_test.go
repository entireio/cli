package cli

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

func TestReadOutgoingSessions(t *testing.T) {
	t.Parallel()
	meta := func(id string) *checkpoint.Metadata { return &checkpoint.Metadata{SessionID: id} }
	broken := errors.New("unreadable")

	tests := []struct {
		name     string
		sessions []*checkpoint.Metadata // nil entry: that read fails
		wantIDs  []string
		wantLast string
		wantErr  bool
	}{
		{name: "all readable", sessions: []*checkpoint.Metadata{meta("a"), meta("b"), meta("c")}, wantIDs: []string{"a", "b", "c"}, wantLast: "c"},
		// The middle session only adds its ID, so an unreadable one leaves the
		// ID out instead of failing the row.
		{name: "middle unreadable", sessions: []*checkpoint.Metadata{meta("a"), nil, meta("c")}, wantIDs: []string{"a", "c"}, wantLast: "c"},
		{name: "first unreadable", sessions: []*checkpoint.Metadata{nil, meta("b")}, wantErr: true},
		{name: "last unreadable", sessions: []*checkpoint.Metadata{meta("a"), meta("b"), nil}, wantErr: true},
		{name: "one session", sessions: []*checkpoint.Metadata{meta("a")}, wantIDs: []string{"a"}, wantLast: "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			read := func(i int) (*checkpoint.Metadata, error) {
				if tt.sessions[i] == nil {
					return nil, broken
				}
				return tt.sessions[i], nil
			}
			first, latest, ids, err := readOutgoingSessions(len(tt.sessions), read)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.sessions[0], first)
			require.Equal(t, tt.wantLast, latest.SessionID)
			require.Equal(t, tt.wantIDs, ids)
		})
	}
}
