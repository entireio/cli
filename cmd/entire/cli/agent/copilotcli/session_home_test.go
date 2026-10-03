package copilotcli

import (
	"path/filepath"
	"testing"
)

func TestCopilotCLIAgent_SessionPathUnder(t *testing.T) {
	t.Parallel()

	c := &CopilotCLIAgent{}
	home := filepath.Join("/", "home", "u", ".copilot")

	tests := []struct {
		name string
		home string
		path string
		want bool
	}{
		{
			name: "session transcript beneath session-state",
			home: home,
			path: filepath.Join(home, "session-state", "abc-123", "events.jsonl"),
			want: true,
		},
		{
			name: "unrelated absolute path with root home",
			home: "/",
			path: "/etc/passwd",
			want: false,
		},
		{
			name: "under home but outside the session-state layout",
			home: home,
			path: filepath.Join(home, "config.json"),
			want: false,
		},
		{
			name: "under a different agent's layout (claude projects)",
			home: home,
			path: filepath.Join(home, "projects", "-home-u-repo", "abc.jsonl"),
			want: false,
		},
		{
			name: "bare session-state directory does not qualify",
			home: home,
			path: filepath.Join(home, "session-state"),
			want: false,
		},
		{
			name: "bare session directory does not qualify",
			home: home,
			path: filepath.Join(home, "session-state", "abc-123"),
			want: false,
		},
		{
			name: "wrong filename beneath the session directory",
			home: home,
			path: filepath.Join(home, "session-state", "abc-123", "notes.jsonl"),
			want: false,
		},
		{
			name: "empty home",
			home: "",
			path: filepath.Join(home, "session-state", "abc-123", "events.jsonl"),
			want: false,
		},
		{
			name: "empty path",
			home: home,
			path: "",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := c.SessionPathUnder(tc.home, tc.path)
			if got != tc.want {
				t.Errorf("SessionPathUnder(%q, %q) = %v, want %v", tc.home, tc.path, got, tc.want)
			}
		})
	}
}
