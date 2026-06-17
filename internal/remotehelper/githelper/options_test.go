package githelper

import (
	"slices"
	"testing"
)

func TestOptions_SetAndSendPackArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		commands  [][2]string
		wantReply []string
		wantArgs  []string
	}{
		{
			name:      "no options accepted",
			commands:  [][2]string{{"object-format", "sha1"}},
			wantReply: []string{"ok"},
			wantArgs:  nil,
		},
		{
			name:      "dry-run on",
			commands:  [][2]string{{"dry-run", "true"}},
			wantReply: []string{"ok"},
			wantArgs:  []string{"--dry-run"},
		},
		{
			name:      "atomic + force-if-includes",
			commands:  [][2]string{{"atomic", "true"}, {"force-if-includes", "true"}},
			wantReply: []string{"ok", "ok"},
			wantArgs:  []string{"--atomic", "--force-if-includes"},
		},
		{
			name:      "push-cert true → signed=true",
			commands:  [][2]string{{"push-cert", "true"}},
			wantReply: []string{"ok"},
			wantArgs:  []string{"--signed=true"},
		},
		{
			name:      "push-cert if-asked",
			commands:  [][2]string{{"push-cert", "if-asked"}},
			wantReply: []string{"ok"},
			wantArgs:  []string{"--signed=if-asked"},
		},
		{
			name:      "push-option and cas accumulate",
			commands:  [][2]string{{"push-option", "ci.skip"}, {"push-option", "ref-prefix=refs/heads/"}, {"cas", "refs/heads/main:deadbeef"}},
			wantReply: []string{"ok", "ok", "ok"},
			wantArgs:  []string{"--push-option=ci.skip", "--push-option=ref-prefix=refs/heads/", "--force-with-lease=refs/heads/main:deadbeef"},
		},
		{
			name:      "unknown option → unsupported",
			commands:  [][2]string{{"weird-option", "value"}},
			wantReply: []string{"unsupported"},
			wantArgs:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := &Options{}
			for i, cmd := range tt.commands {
				if reply := opts.Set(cmd[0], cmd[1]); reply != tt.wantReply[i] {
					t.Errorf("Set(%q, %q) = %q, want %q", cmd[0], cmd[1], reply, tt.wantReply[i])
				}
			}
			if got := opts.SendPackArgs(); !slices.Equal(got, tt.wantArgs) {
				t.Errorf("SendPackArgs = %v, want %v", got, tt.wantArgs)
			}
		})
	}
}
