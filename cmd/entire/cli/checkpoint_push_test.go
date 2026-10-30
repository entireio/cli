package cli

import (
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func TestWriteCheckpointPushText_Changed(t *testing.T) {
	t.Parallel()

	ref := plumbing.ReferenceName("refs/entire/checkpoints/v1/a1/b2c3d4e5f6")
	newTip := plumbing.NewHash("1111111111111111111111111111111111111111")
	pin := plumbing.NewHash("2222222222222222222222222222222222222222")

	tests := []struct {
		name   string
		pinned plumbing.Hash
		commit plumbing.Hash
		want   string
	}{
		{
			name:   "new tip known",
			pinned: pin,
			commit: newTip,
			want:   "not sent: it changed since you looked; it is now " + newTip.String() + ". Check it with 'entire checkpoint list --outgoing'.",
		},
		{
			// A recovery can lose to a concurrent write whose new tip is then
			// unreadable; the output must not invent a commit.
			name:   "new tip unknown",
			pinned: pin,
			want:   "not sent: it changed since you looked. Check it with 'entire checkpoint list --outgoing'.",
		},
		{
			// --all and a bare ID pin nothing, so nothing was "looked at".
			name:   "not pinned",
			commit: newTip,
			want:   "not sent: a newer version was written while it was uploading. Run the command again to upload it.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			writeCheckpointPushText(&out, strategy.CheckpointPushReport{
				Remote: "origin",
				Outcomes: []strategy.CheckpointPushOutcome{{
					Target: strategy.CheckpointPushRequest{Ref: ref, Commit: tt.pinned},
					Commit: tt.commit,
					Status: strategy.CheckpointPushChanged,
				}},
			})
			if !strings.Contains(out.String(), tt.want) {
				t.Fatalf("output = %q, want it to contain %q", out.String(), tt.want)
			}
			if tt.pinned.IsZero() && strings.Contains(out.String(), "since you looked") {
				t.Fatalf("output = %q implies a pin the request did not make", out.String())
			}
			if tt.commit.IsZero() && strings.Contains(out.String(), "it is now") {
				t.Fatalf("output = %q names a commit it does not have", out.String())
			}
		})
	}
}
