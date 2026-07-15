package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestParseTrailRepoArg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		raw                  string
		wantForge, wantOwner string
		wantRepo             string
		wantErr              bool
	}{
		{name: "forge/owner/repo", raw: "gh/entireio/cli", wantForge: "gh", wantOwner: "entireio", wantRepo: "cli"},
		{name: "strips .git", raw: "gh/acme/app.git", wantForge: "gh", wantOwner: "acme", wantRepo: "app"},
		{name: "trims whitespace", raw: "  gh/acme/app  ", wantForge: "gh", wantOwner: "acme", wantRepo: "app"},
		{name: "trims surrounding slashes", raw: "/gh/acme/app/", wantForge: "gh", wantOwner: "acme", wantRepo: "app"},
		{name: "https clone URL", raw: "https://github.com/acme/app.git", wantForge: "gh", wantOwner: "acme", wantRepo: "app"},
		{name: "ssh scp URL", raw: "git@github.com:acme/app.git", wantForge: "gh", wantOwner: "acme", wantRepo: "app"},
		{name: "entire URL", raw: "entire://host/gh/acme/app", wantForge: "gh", wantOwner: "acme", wantRepo: "app"},
		{name: "empty", raw: "", wantErr: true},
		{name: "two segments", raw: "acme/app", wantErr: true},
		{name: "forge plus owner only", raw: "gh/acme", wantErr: true},
		{name: "four segments", raw: "gh/acme/app/extra", wantErr: true},
		{name: "unsupported forge host", raw: "git@gitlab.com:acme/app.git", wantErr: true},
		{name: "bare host instead of forge id", raw: "github.com/acme/app", wantErr: true},
		{name: "bare unsupported forge host", raw: "gitlab.com/acme/app", wantErr: true},
		{name: "unknown short forge id", raw: "zz/acme/app", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			forge, owner, repo, err := parseTrailRepoArg(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseTrailRepoArg(%q) = (%q,%q,%q), want error", tt.raw, forge, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTrailRepoArg(%q): unexpected error %v", tt.raw, err)
			}
			if forge != tt.wantForge || owner != tt.wantOwner || repo != tt.wantRepo {
				t.Fatalf("parseTrailRepoArg(%q) = (%q,%q,%q), want (%q,%q,%q)",
					tt.raw, forge, owner, repo, tt.wantForge, tt.wantOwner, tt.wantRepo)
			}
		})
	}
}

// resolveTrailRepoOrRemote with an explicit override must not touch git: it
// resolves straight from the flag value. (The fallback path needs a repo and is
// covered elsewhere.)
func TestResolveTrailRepoOrRemote_OverrideSkipsGit(t *testing.T) {
	t.Parallel()
	forge, owner, repo, err := resolveTrailRepoOrRemote(t.Context(), "gh/acme/app")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if forge != "gh" || owner != "acme" || repo != "app" {
		t.Fatalf("got (%q,%q,%q), want (gh,acme,app)", forge, owner, repo)
	}
}

func TestResolveTrailBranch_OverrideWins(t *testing.T) {
	t.Parallel()
	got, err := resolveTrailBranch(t.Context(), "my/feature")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "my/feature" {
		t.Fatalf("got %q, want my/feature", got)
	}
}

// execTrailCmdExpectErr runs `entire trail <args...>` against a fresh command
// tree and returns the error, with output discarded. Used to assert flag
// validation that fires before any auth/network/git access.
func execTrailCmdExpectErr(t *testing.T, args ...string) error {
	t.Helper()
	cmd := newTrailCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func TestTrailRepoOverride_RejectedByLocalCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "create", args: []string{"create", "--repo", "gh/acme/app"}},
		{name: "checkout", args: []string{"checkout", "--repo", "gh/acme/app"}},
		{name: "finding apply", args: []string{"finding", "apply", "--repo", "gh/acme/app", "deadbeef"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := execTrailCmdExpectErr(t, tt.args...)
			if err == nil || !strings.Contains(err.Error(), "--repo is not supported") {
				t.Fatalf("err = %v, want '--repo is not supported'", err)
			}
		})
	}
}

func TestTrailSelectorAndBranchAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{name: "show", args: []string{"show", "123", "--branch", "foo"}, wantSub: "not both"},
		{name: "watch", args: []string{"watch", "5", "--branch", "foo"}, wantSub: "not both"},
		{name: "finding list positional", args: []string{"finding", "list", "123", "--branch", "foo"}, wantSub: "not both"},
		{name: "finding list --trail", args: []string{"finding", "list", "--trail", "123", "--branch", "foo"}, wantSub: "not both"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := execTrailCmdExpectErr(t, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantSub)
			}
		})
	}
}

// --repo must not silently fall back to the local checkout's branch: the
// branch-defaulting commands require an explicit branch or selector alongside it.
func TestTrailRepoRequiresExplicitTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "show", args: []string{"show", "--repo", "gh/acme/app"}},
		{name: "watch", args: []string{"watch", "--repo", "gh/acme/app"}},
		{name: "update", args: []string{"update", "--repo", "gh/acme/app"}},
		{name: "delete", args: []string{"delete", "--repo", "gh/acme/app"}},
		{name: "finding list", args: []string{"finding", "list", "--repo", "gh/acme/app"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := execTrailCmdExpectErr(t, tt.args...)
			if err == nil || !strings.Contains(err.Error(), "--repo requires an explicit target") {
				t.Fatalf("err = %v, want '--repo requires an explicit target'", err)
			}
		})
	}
}

// Sanity: the persistent --repo flag is registered on the trail root and shows
// up in help, so every read subcommand inherits it.
func TestTrailRepoFlagRegisteredOnRoot(t *testing.T) {
	t.Parallel()
	cmd := newTrailCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("help: %v", err)
	}
	if !strings.Contains(out.String(), "--repo") {
		t.Fatalf("help output missing --repo flag:\n%s", out.String())
	}
}
