package cli

import (
	"testing"

	"github.com/entireio/cli/internal/coreapi"
)

// TestParseGitHubURL is ported from entiredb's cmd/entire-repo/cli
// mirror_test.go, since parseGitHubURL was carried over verbatim.
func TestParseGitHubURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{name: "HTTPS", url: "https://github.com/entirehq/entiredb", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "HTTPS with .git", url: "https://github.com/entirehq/entiredb.git", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "SSH", url: "git@github.com:entirehq/entiredb", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "SSH with .git", url: "git@github.com:entirehq/entiredb.git", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "HTTP", url: "http://github.com/owner/repo", wantOwner: "owner", wantRepo: "repo"},
		{name: "bare with github.com prefix", url: "github.com/octocat/hello-world", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "bare github.com prefix with .git", url: "github.com/octocat/hello-world.git", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "bare owner/repo", url: "octocat/hello-world", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "bare lowercased", url: "OctoCat/Hello-World", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "repo with dot", url: "github.com/octocat/hello.world", wantOwner: "octocat", wantRepo: "hello.world"},
		{name: "repo with underscore", url: "octocat/hello_world", wantOwner: "octocat", wantRepo: "hello_world"},
		{name: "GitLab", url: "https://gitlab.com/owner/repo", wantErr: true},
		{name: "missing repo", url: "https://github.com/owner", wantErr: true},
		{name: "not a URL", url: "not-a-url", wantErr: true},
		{name: "entire URL", url: "entire://host/git/owner/repo", wantErr: true},
		// Parameter-smuggling shapes the tightened owner/repo charset rejects:
		// these would otherwise mutate the audience / probe URL built from
		// owner/repo.
		{name: "repo with query smuggle", url: "octocat/repo?bypass=1", wantErr: true},
		{name: "repo with fragment", url: "octocat/repo#anchor", wantErr: true},
		{name: "owner with at-sign", url: "a@b/repo", wantErr: true},
		{name: "repo with encoded slash", url: "octocat/repo%2fevil", wantErr: true},
		{name: "owner with dot-dot", url: "../repo", wantErr: true},
		{name: "owner with underscore (not a GitHub login)", url: "oct_cat/repo", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := parseGitHubURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseGitHubURL(%q) expected error, got %q/%q", tt.url, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitHubURL(%q) unexpected error: %v", tt.url, err)
			}
			if owner != tt.wantOwner || repo != tt.wantRepo {
				t.Errorf("parseGitHubURL(%q) = %q/%q, want %q/%q", tt.url, owner, repo, tt.wantOwner, tt.wantRepo)
			}
		})
	}
}

func TestMirrorRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mirror coreapi.Mirror
		want   []string
	}{
		{
			name:   "private mirror synthesises clone URL",
			mirror: coreapi.Mirror{Owner: "entirehq", Repo: "entire.io", ClusterHost: "aws-us-east-2.entire.io", IsPrivate: coreapi.NewOptBool(true)},
			want:   []string{"entirehq/entire.io", "entire://aws-us-east-2.entire.io/gh/entirehq/entire.io", "yes"},
		},
		{
			name:   "public mirror, unset IsPrivate defaults to no",
			mirror: coreapi.Mirror{Owner: "octocat", Repo: "hello", ClusterHost: "eu-west-1.entire.io"},
			want:   []string{"octocat/hello", "entire://eu-west-1.entire.io/gh/octocat/hello", "no"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mirrorRow(tt.mirror)
			if len(got) != len(tt.want) {
				t.Fatalf("mirrorRow len = %d, want %d (%v)", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("mirrorRow[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestClusterArg(t *testing.T) {
	t.Parallel()
	if got := clusterArg([]string{"github.com/o/r", "eu-west-1.entire.io"}); got != "eu-west-1.entire.io" {
		t.Errorf("explicit cluster = %q, want eu-west-1.entire.io", got)
	}
	if got := clusterArg([]string{"github.com/o/r"}); got != defaultClusterHost {
		t.Errorf("omitted cluster = %q, want default %q", got, defaultClusterHost)
	}
}

func TestValidateClusterHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{name: "default cluster", host: defaultClusterHost},
		{name: "other region", host: "eu-west-1.entire.io"},
		{name: "single label", host: "localhost"},
		{name: "host with port", host: "localhost:8080"},
		{name: "ipv4", host: "10.0.0.1"},
		{name: "ipv4 with port", host: "10.0.0.1:8080"},
		// The token-leak primitive: userinfo demotes the real cluster so the
		// request (and basic-auth token) targets evil.com.
		{name: "userinfo smuggle", host: "aws-us-east-2.entire.io@evil.com", wantErr: true},
		{name: "path smuggle", host: "aws-us-east-2.entire.io/../evil", wantErr: true},
		{name: "query smuggle", host: "aws-us-east-2.entire.io?x=1", wantErr: true},
		{name: "fragment smuggle", host: "aws-us-east-2.entire.io#x", wantErr: true},
		{name: "scheme prefix", host: "https://evil.com", wantErr: true},
		{name: "empty", host: "", wantErr: true},
		{name: "whitespace", host: "   ", wantErr: true},
		{name: "leading hyphen label", host: "-bad.entire.io", wantErr: true},
		{name: "space in host", host: "evil .com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateClusterHost(tt.host)
			if tt.wantErr && err == nil {
				t.Errorf("validateClusterHost(%q) = nil, want error", tt.host)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateClusterHost(%q) = %v, want nil", tt.host, err)
			}
		})
	}
}
