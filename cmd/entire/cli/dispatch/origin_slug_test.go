package dispatch

import (
	"strings"
	"testing"
)

// TestOriginRepoSlug pins that the origin-derived dispatch default names the
// forge the remote actually points at, so an Entire-native checkout dispatches
// as et/<project>/<repo> instead of being rejected as "not GitHub".
func TestOriginRepoSlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		remote  string
		want    string
		wantErr string
	}{
		{name: "github https", remote: "https://github.com/acme/thing.git", want: "gh/acme/thing"},
		{name: "github scp", remote: "git@github.com:acme/thing.git", want: "gh/acme/thing"},
		{name: "entire mirror of github", remote: "entire://cell1.entire.io/gh/acme/thing", want: "gh/acme/thing"},
		{name: "entire native", remote: "entire://aws-us-east-2.entire.io/et/entirehq/entire-api", want: "et/entirehq/entire-api"},
		{name: "entire native with .git", remote: "entire://cell1.entire.io/et/audit1/foo.git", want: "et/audit1/foo"},
		{name: "whitespace trimmed", remote: "  entire://cell1.entire.io/et/audit1/foo \n", want: "et/audit1/foo"},
		{name: "other host", remote: "https://gitlab.com/acme/thing.git", wantErr: "gitlab.com"},
		{name: "entire unknown forge token", remote: "entire://cell1.entire.io/xx/acme/thing", wantErr: "gh/<owner>/<repo> or et/<project>/<repo>"},
		{name: "entire without forge token", remote: "entire://cell1.entire.io/acme/thing", wantErr: "parsing remote URL"},
		{name: "empty", remote: "", wantErr: "empty remote URL"},
		{name: "unparseable", remote: "not a url", wantErr: "parsing remote URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := OriginRepoSlug(tt.remote)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("OriginRepoSlug(%q) err = %v, want containing %q", tt.remote, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("OriginRepoSlug(%q): %v", tt.remote, err)
			}
			if got != tt.want {
				t.Fatalf("OriginRepoSlug(%q) = %q, want %q", tt.remote, got, tt.want)
			}
		})
	}
}

// TestOriginRepoSlug_RoundTripsThroughRepoSlugValidation: every slug the
// origin produces must be one --repos would accept, or the implicit default
// would send something the explicit flag refuses.
func TestOriginRepoSlug_RoundTripsThroughRepoSlugValidation(t *testing.T) {
	t.Parallel()

	for _, remote := range []string{
		"https://github.com/acme/thing.git",
		"entire://cell1.entire.io/et/audit1/foo",
	} {
		slug, err := OriginRepoSlug(remote)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := normalizeRepoSlug(slug); err != nil {
			t.Fatalf("origin slug %q for %s is not a valid --repos value: %v", slug, remote, err)
		}
	}
}
