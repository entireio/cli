package claudecode

import (
	"path/filepath"
	"testing"
)

func TestClaudeCodeAgent_SessionPathUnder(t *testing.T) {
	t.Parallel()

	c := &ClaudeCodeAgent{}
	home := filepath.Join("/", "home", "u", ".claude")

	tests := []struct {
		name string
		home string
		path string
		want bool
	}{
		{
			name: "session transcript beneath projects",
			home: home,
			path: filepath.Join(home, "projects", "-home-u-repo", "abc123.jsonl"),
			want: true,
		},
		{
			name: "unrelated absolute path with root home",
			home: "/",
			path: "/etc/passwd",
			want: false,
		},
		{
			name: "under home but outside the projects layout",
			home: home,
			path: filepath.Join(home, "settings.json"),
			want: false,
		},
		{
			name: "under a different agent's layout (codex sessions)",
			home: home,
			path: filepath.Join(home, "sessions", "2026", "01", "01", "rollout-x.jsonl"),
			want: false,
		},
		{
			name: "bare projects directory does not qualify",
			home: home,
			path: filepath.Join(home, "projects"),
			want: false,
		},
		{
			name: "non-jsonl file beneath projects",
			home: home,
			path: filepath.Join(home, "projects", "-home-u-repo", "notes.txt"),
			want: false,
		},
		{
			name: "empty home",
			home: "",
			path: filepath.Join(home, "projects", "abc.jsonl"),
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
