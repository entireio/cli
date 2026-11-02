package dispatch

import (
	"strings"
	"testing"
)

func TestNormalizeRepoSlugs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      []string
		want    string
		wantErr string
	}{
		{name: "github prefix", in: []string{"gh/entireio/cli"}, want: "gh/entireio/cli"},
		{name: "native prefix", in: []string{"et/myproject/service"}, want: "et/myproject/service"},
		{name: "whitespace trimmed", in: []string{"  gh/entireio/cli "}, want: "gh/entireio/cli"},
		{name: "punctuation in repo", in: []string{"gh/entireio/entire.io"}, want: "gh/entireio/entire.io"},
		{name: "same name on both forges is two repos", in: []string{"gh/entireio/cli", "et/entireio/cli"}, want: "gh/entireio/cli,et/entireio/cli"},
		{name: "dedupes case variants, first wins", in: []string{"gh/entireio/cli", "gh/ENTIREIO/CLI", "gh/EntireIO/cli"}, want: "gh/entireio/cli"},
		{name: "keeps order", in: []string{"gh/c/d", "et/a/b"}, want: "gh/c/d,et/a/b"},
		{name: "empty", in: nil, want: ""},
		{name: "bare names no forge", in: []string{"entireio/cli"}, wantErr: `invalid repo "entireio/cli": a repo must name its forge — did you mean gh/entireio/cli or et/entireio/cli?`},
		{name: "bare with whitespace", in: []string{" entireio/cli "}, wantErr: `did you mean gh/entireio/cli or et/entireio/cli?`},
		{name: "bare owner named like a forge", in: []string{"gh/cli"}, wantErr: `did you mean gh/gh/cli or et/gh/cli?`},
		{name: "missing slash", in: []string{"entireio"}, wantErr: `invalid repo "entireio": expected gh/<owner>/<repo> or et/<project>/<repo>`},
		{name: "unknown forge", in: []string{"gl/entireio/cli"}, wantErr: `invalid repo "gl/entireio/cli": expected`},
		{name: "uppercase forge", in: []string{"GH/entireio/cli"}, wantErr: `invalid repo "GH/entireio/cli": expected`},
		{name: "too many segments", in: []string{"gh/entireio/cli/issues"}, wantErr: `invalid repo "gh/entireio/cli/issues": expected`},
		{name: "traversal", in: []string{"../../etc/passwd"}, wantErr: `invalid repo "../../etc/passwd": expected`},
		{name: "empty owner", in: []string{"gh//cli"}, wantErr: `invalid repo "gh//cli": expected`},
		{name: "empty repo", in: []string{"gh/entireio/"}, wantErr: `invalid repo "gh/entireio/": expected`},
		{name: "second bad after good", in: []string{"gh/a/b", "nope"}, wantErr: `invalid repo "nope"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeRepoSlugs(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("normalizeRepoSlugs(%q) err = %v, want %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeRepoSlugs(%q) unexpected error: %v", tt.in, err)
			}
			if joined := strings.Join(got, ","); joined != tt.want {
				t.Fatalf("normalizeRepoSlugs(%q) = %q, want %q", tt.in, joined, tt.want)
			}
		})
	}
}

func TestGitHubRepoName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		slug   string
		want   string
		wantOK bool
	}{
		{slug: "gh/entireio/cli", want: "entireio/cli", wantOK: true},
		{slug: " gh/entireio/cli ", want: "entireio/cli", wantOK: true},
		// A bare name is never assumed to be GitHub.
		{slug: "entireio/cli"},
		{slug: "et/myproject/service"},
		{slug: "gl/entireio/cli"},
		{slug: "entireio"},
		{slug: "gh/entireio/cli/issues"},
		{slug: ""},
	}

	for _, tt := range tests {
		t.Run(tt.slug, func(t *testing.T) {
			t.Parallel()

			got, ok := GitHubRepoName(tt.slug)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("GitHubRepoName(%q) = %q, %v; want %q, %v", tt.slug, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestEchoedSlugMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		echoed, requested string
		want              bool
	}{
		{echoed: "gh/entireio/cli", requested: "gh/entireio/cli", want: true},
		{echoed: "gh/EntireIO/CLI", requested: "gh/entireio/cli", want: true},
		{echoed: "et/entireio/cli", requested: "et/entireio/cli", want: true},
		// The gateway's legacy bare spelling means GitHub.
		{echoed: "entireio/cli", requested: "gh/entireio/cli", want: true},
		{echoed: "entireio/cli", requested: "et/entireio/cli", want: false},
		{echoed: "et/entireio/cli", requested: "gh/entireio/cli", want: false},
		{echoed: "gh/entireio/cli", requested: "gh/entireio/cli2", want: false},
		{echoed: "not a slug", requested: "gh/entireio/cli", want: false},
		{echoed: "gh/entireio/cli", requested: "", want: false},
		// A request is always forge-qualified; a bare one matches nothing.
		{echoed: "gh/entireio/cli", requested: "entireio/cli", want: false},
		{echoed: "entireio/cli", requested: "entireio/cli", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.echoed+" vs "+tt.requested, func(t *testing.T) {
			t.Parallel()

			if got := echoedSlugMatches(tt.echoed, tt.requested); got != tt.want {
				t.Fatalf("echoedSlugMatches(%q, %q) = %v, want %v", tt.echoed, tt.requested, got, tt.want)
			}
		})
	}
}
