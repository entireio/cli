package factoryaidroid

import (
	"path/filepath"
	"testing"
)

func TestFactoryAIDroidAgent_SessionPathUnder(t *testing.T) {
	t.Parallel()

	f := &FactoryAIDroidAgent{}
	home := filepath.Join("/", "home", "u")

	tests := []struct {
		name string
		home string
		path string
		want bool
	}{
		{
			name: "session transcript beneath .factory/sessions",
			home: home,
			path: filepath.Join(home, ".factory", "sessions", "-home-u-repo", "abc123.jsonl"),
			want: true,
		},
		{
			name: "unrelated absolute path with root home",
			home: "/",
			path: "/etc/passwd",
			want: false,
		},
		{
			name: "under home but outside the .factory/sessions layout",
			home: home,
			path: filepath.Join(home, ".factory", "config.json"),
			want: false,
		},
		{
			name: "under a different agent's layout (claude projects)",
			home: home,
			path: filepath.Join(home, ".claude", "projects", "-home-u-repo", "abc.jsonl"),
			want: false,
		},
		{
			name: "bare sessions directory does not qualify",
			home: home,
			path: filepath.Join(home, ".factory", "sessions"),
			want: false,
		},
		{
			name: "non-jsonl file beneath .factory/sessions",
			home: home,
			path: filepath.Join(home, ".factory", "sessions", "-home-u-repo", "notes.txt"),
			want: false,
		},
		{
			name: "empty home",
			home: "",
			path: filepath.Join(home, ".factory", "sessions", "abc.jsonl"),
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
			got := f.SessionPathUnder(tc.home, tc.path)
			if got != tc.want {
				t.Errorf("SessionPathUnder(%q, %q) = %v, want %v", tc.home, tc.path, got, tc.want)
			}
		})
	}
}
