package dispatch

import "testing"

func TestGitHubRepoURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fullName string
		want     string
	}{
		{
			name:     "valid",
			fullName: "entireio/cli",
			want:     testRepoURL,
		},
		{
			name:     "valid punctuation in repo",
			fullName: "entireio/entire.io",
			want:     "https://github.com/entireio/entire.io",
		},
		{
			name:     "missing slash",
			fullName: "entireio",
			want:     "",
		},
		{
			name:     "nested path",
			fullName: "entireio/cli/issues",
			want:     "",
		},
		{
			name:     "unsafe owner",
			fullName: "-entireio/cli",
			want:     "",
		},
		{
			name:     "unsafe repo",
			fullName: "entireio/cli)",
			want:     "",
		},
		{
			name:     "dot repo",
			fullName: "entireio/.",
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := githubRepoURL(tt.fullName); got != tt.want {
				t.Fatalf("githubRepoURL(%q) = %q, want %q", tt.fullName, got, tt.want)
			}
		})
	}
}
