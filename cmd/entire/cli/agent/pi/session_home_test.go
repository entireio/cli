package pi

import (
	"path/filepath"
	"testing"
)

func TestPiAgent_SessionPathUnder(t *testing.T) {
	t.Parallel()

	a := &PiAgent{}
	home := filepath.Join("/", "home", "u", ".pi", "agent")

	tests := []struct {
		name string
		home string
		path string
		want bool
	}{
		{
			name: "session transcript beneath sessions",
			home: home,
			path: filepath.Join(home, "sessions", "--home-u-repo--", "20260101T000000_abc.jsonl"),
			want: true,
		},
		{
			name: "unrelated absolute path with root home",
			home: "/",
			path: "/etc/passwd",
			want: false,
		},
		{
			name: "under home but outside the sessions layout",
			home: home,
			path: filepath.Join(home, "settings.json"),
			want: false,
		},
		{
			name: "under a different agent's layout (claude projects)",
			home: home,
			path: filepath.Join(home, "projects", "-home-u-repo", "abc.jsonl"),
			want: false,
		},
		{
			name: "bare sessions directory does not qualify",
			home: home,
			path: filepath.Join(home, "sessions"),
			want: false,
		},
		{
			name: "non-jsonl file beneath sessions",
			home: home,
			path: filepath.Join(home, "sessions", "--home-u-repo--", "notes.txt"),
			want: false,
		},
		{
			name: "empty home",
			home: "",
			path: filepath.Join(home, "sessions", "abc.jsonl"),
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
			got := a.SessionPathUnder(tc.home, tc.path)
			if got != tc.want {
				t.Errorf("SessionPathUnder(%q, %q) = %v, want %v", tc.home, tc.path, got, tc.want)
			}
		})
	}
}
