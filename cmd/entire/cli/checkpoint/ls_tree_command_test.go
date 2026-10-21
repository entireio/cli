package checkpoint

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func TestCheckpointLsTreeCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		id       string
		basePath string
		want     string
	}{
		{name: "refs store lists the checkpoint's own ref", id: "01M3PWG7BKWYH0XJKS810J0XEX", basePath: "", want: "git ls-tree refs/entire/checkpoints/EX/01M3PWG7BKWYH0XJKS810J0XEX"},
		{name: "branch store lists the sharded v1 directory", id: "abc123def456", basePath: "ab/c123def456/", want: "git ls-tree entire/checkpoints/v1 ab/c123def456/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := checkpointLsTreeCommand(id.MustCheckpointID(tt.id), tt.basePath); got != tt.want {
				t.Errorf("checkpointLsTreeCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}
