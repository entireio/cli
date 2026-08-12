package cli

import (
	"cmp"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestValidateGitRemoteName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		remote  string
		wantErr bool
	}{
		{name: "origin", remote: "origin"},
		{name: "entire", remote: "entire"},
		{name: "digits and dashes", remote: "mirror-2"},
		{name: "dotted", remote: "my.remote"},
		{name: "slashed", remote: "team/mirror"},
		{name: "empty", remote: "", wantErr: true},
		{name: "leading dash reads as a flag", remote: "-f", wantErr: true},
		{name: "leading dot", remote: ".hidden", wantErr: true},
		{name: "space", remote: "my remote", wantErr: true},
		{name: "glob", remote: "mirror*", wantErr: true},
		{name: "traversal", remote: "a/../b", wantErr: true},
		{name: "lock suffix", remote: "origin.lock", wantErr: true},
		{name: "newline", remote: "origin\nfetch", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateGitRemoteName(tt.remote)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestRedactGitArgs(t *testing.T) {
	t.Parallel()
	got := redactGitArgs([]string{
		"remote", "add", "upstream",
		"https://user:ghp_SECRET@github.com/octocat/hello-world",
	})
	require.Equal(t, []string{
		"remote", "add", "upstream",
		"https://github.com/octocat/hello-world",
	}, got)

	t.Run("leaves non-URL args untouched", func(t *testing.T) {
		t.Parallel()
		// RedactURL would mangle bare words into "://word", so they must be
		// passed through rather than redacted blanket-fashion.
		require.Equal(t, []string{"remote"}, redactGitArgs([]string{"remote"}))
		require.Equal(t,
			[]string{"remote", "set-url", "origin"},
			redactGitArgs([]string{"remote", "set-url", "origin"}))
	})

	t.Run("passes through URL forms that carry no credentials", func(t *testing.T) {
		t.Parallel()
		require.Equal(t,
			[]string{"entire://aws-us-east-2.entire.io/gh/octocat/hello-world"},
			redactGitArgs([]string{"entire://aws-us-east-2.entire.io/gh/octocat/hello-world"}))
		// SCP-style has no embeddable credentials; the "@" must not mangle it.
		require.Equal(t,
			[]string{"git@github.com:octocat/hello-world.git"},
			redactGitArgs([]string{"git@github.com:octocat/hello-world.git"}))
	})
}

func TestPlanMirrorRemote(t *testing.T) {
	t.Parallel()
	const mirrorURL = "entire://aws-us-east-2.entire.io/gh/octocat/hello-world"
	const forgeURL = "git@github.com:octocat/hello-world.git"

	t.Run("adds a remote that does not exist", func(t *testing.T) {
		t.Parallel()
		plan := planMirrorRemote("entire", mirrorURL, "", "upstream", map[string]bool{"origin": true})
		require.True(t, plan.add)
		require.False(t, plan.noop)
		require.Empty(t, plan.replacedURL)
		require.Empty(t, plan.preserveAs, "nothing was replaced, so nothing is preserved")
		require.Equal(t, mirrorURL, plan.mirrorURL)
	})

	t.Run("replaces and preserves the previous URL", func(t *testing.T) {
		t.Parallel()
		plan := planMirrorRemote("origin", mirrorURL, forgeURL, "upstream", map[string]bool{"origin": true})
		require.False(t, plan.add)
		require.False(t, plan.noop)
		require.Equal(t, forgeURL, plan.replacedURL)
		require.Equal(t, "upstream", plan.preserveAs)
	})

	// The fork layout (origin + upstream both configured) hits this by default,
	// so the skip must be recorded for the report to warn about — not silently
	// dropped, which would leave a clean ✓ over a lost URL.
	t.Run("records the skip when the upstream name is taken", func(t *testing.T) {
		t.Parallel()
		plan := planMirrorRemote("origin", mirrorURL, forgeURL, "upstream",
			map[string]bool{"origin": true, "upstream": true})
		require.Equal(t, forgeURL, plan.replacedURL)
		require.Empty(t, plan.preserveAs, "an existing upstream must not be clobbered")
		require.Equal(t, "upstream", plan.preserveSkipped)
	})

	// `--upstream ''` is an explicit opt-out, so there is nothing to warn about.
	t.Run("skips preserving silently when disabled", func(t *testing.T) {
		t.Parallel()
		plan := planMirrorRemote("origin", mirrorURL, forgeURL, "", map[string]bool{"origin": true})
		require.Equal(t, forgeURL, plan.replacedURL)
		require.Empty(t, plan.preserveAs)
		require.Empty(t, plan.preserveSkipped, "an explicit opt-out is not a skipped preservation")
	})

	t.Run("records the skip when preserving onto itself", func(t *testing.T) {
		t.Parallel()
		plan := planMirrorRemote("origin", mirrorURL, forgeURL, "origin", map[string]bool{"origin": true})
		require.Empty(t, plan.preserveAs)
		require.Equal(t, "origin", plan.preserveSkipped)
	})

	t.Run("a successful preserve records no skip", func(t *testing.T) {
		t.Parallel()
		plan := planMirrorRemote("origin", mirrorURL, forgeURL, "upstream", map[string]bool{"origin": true})
		require.Equal(t, "upstream", plan.preserveAs)
		require.Empty(t, plan.preserveSkipped)
	})

	// add/noop never replace anything, so neither can strand a URL.
	t.Run("add and noop never record a skip", func(t *testing.T) {
		t.Parallel()
		add := planMirrorRemote("entire", mirrorURL, "", "upstream", map[string]bool{"origin": true, "upstream": true})
		require.Empty(t, add.preserveSkipped)
		noop := planMirrorRemote("origin", mirrorURL, mirrorURL, "upstream", map[string]bool{"origin": true, "upstream": true})
		require.Empty(t, noop.preserveSkipped)
	})

	t.Run("noop when already pointing at the mirror", func(t *testing.T) {
		t.Parallel()
		plan := planMirrorRemote("origin", mirrorURL, mirrorURL, "upstream", map[string]bool{"origin": true})
		require.True(t, plan.noop)
		require.Empty(t, plan.preserveAs)
	})

	t.Run("noop tolerates surrounding whitespace and case", func(t *testing.T) {
		t.Parallel()
		plan := planMirrorRemote("origin", mirrorURL, "  "+strings.ToUpper(mirrorURL)+"  ", "upstream",
			map[string]bool{"origin": true})
		require.True(t, plan.noop)
	})
}

// applyPlanRepo is a temp git repo with the given remotes configured, for the
// apply-path tests.
func applyPlanRepo(t *testing.T, remotes map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	for name, url := range remotes {
		cmd := exec.CommandContext(t.Context(), "git", "remote", "add", name, url)
		cmd.Dir = dir
		require.NoError(t, cmd.Run(), "add remote %q", name)
	}
	return dir
}

func remoteURL(t *testing.T, dir, name string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "remote", "get-url", name)
	cmd.Dir = dir
	out, err := cmd.Output()
	require.NoError(t, err, "get-url %q", name)
	return strings.TrimSpace(string(out))
}

func TestApplyMirrorRemotePlan(t *testing.T) {
	t.Parallel()
	const mirrorURL = "entire://aws-us-east-2.entire.io/gh/octocat/hello-world"
	const forgeURL = "git@github.com:octocat/hello-world.git"

	t.Run("replace preserves the old URL under upstream", func(t *testing.T) {
		t.Parallel()
		dir := applyPlanRepo(t, map[string]string{"origin": forgeURL})
		plan := planMirrorRemote("origin", mirrorURL, forgeURL, "upstream", map[string]bool{"origin": true})
		require.NoError(t, applyMirrorRemotePlan(t.Context(), dir, plan))
		require.Equal(t, mirrorURL, remoteURL(t, dir, "origin"))
		require.Equal(t, forgeURL, remoteURL(t, dir, "upstream"))
	})

	t.Run("add creates a side remote and leaves origin alone", func(t *testing.T) {
		t.Parallel()
		dir := applyPlanRepo(t, map[string]string{"origin": forgeURL})
		plan := planMirrorRemote("entire", mirrorURL, "", "upstream", map[string]bool{"origin": true})
		require.NoError(t, applyMirrorRemotePlan(t.Context(), dir, plan))
		require.Equal(t, mirrorURL, remoteURL(t, dir, "entire"))
		require.Equal(t, forgeURL, remoteURL(t, dir, "origin"), "origin must be untouched")
	})

	t.Run("replace without preserving discards the old URL", func(t *testing.T) {
		t.Parallel()
		dir := applyPlanRepo(t, map[string]string{"origin": forgeURL})
		plan := planMirrorRemote("origin", mirrorURL, forgeURL, "", map[string]bool{"origin": true})
		require.NoError(t, applyMirrorRemotePlan(t.Context(), dir, plan))
		require.Equal(t, mirrorURL, remoteURL(t, dir, "origin"))
		cmd := exec.CommandContext(t.Context(), "git", "remote", "get-url", "upstream")
		cmd.Dir = dir
		require.Error(t, cmd.Run(), "no upstream remote should have been created")
	})

	t.Run("noop writes nothing", func(t *testing.T) {
		t.Parallel()
		dir := applyPlanRepo(t, map[string]string{"origin": mirrorURL})
		plan := planMirrorRemote("origin", mirrorURL, mirrorURL, "upstream", map[string]bool{"origin": true})
		require.NoError(t, applyMirrorRemotePlan(t.Context(), dir, plan))
		require.Equal(t, mirrorURL, remoteURL(t, dir, "origin"))
		cmd := exec.CommandContext(t.Context(), "git", "remote", "get-url", "upstream")
		cmd.Dir = dir
		require.Error(t, cmd.Run())
	})

	// A failing `git remote add` echoes its argv into the error, and that error is
	// a plain (printed) error — so a credentialed replaced URL must not survive
	// into it. Guards the same property reportMirrorRemotePlan already has.
	t.Run("a failed git command does not leak credentials from the argv", func(t *testing.T) {
		t.Parallel()
		dir := applyPlanRepo(t, map[string]string{"origin": "git@github.com:octocat/hello-world.git"})
		plan := mirrorRemotePlan{
			remote:      "origin",
			mirrorURL:   mirrorURL,
			replacedURL: "https://user:ghp_SUPERSECRET@github.com/octocat/hello-world",
			// Collides with the existing origin, so `git remote add` fails.
			preserveAs: "origin",
		}
		err := applyMirrorRemotePlan(t.Context(), dir, plan)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "ghp_SUPERSECRET", "credentials must not reach the error message")
		require.NotContains(t, err.Error(), "user:", "userinfo must not reach the error message")
		// Still useful for diagnosis: the command and the host survive.
		require.Contains(t, err.Error(), "git remote add")
		require.Contains(t, err.Error(), "github.com/octocat/hello-world")
	})

	t.Run("a failed preserve leaves the target URL intact", func(t *testing.T) {
		t.Parallel()
		dir := applyPlanRepo(t, map[string]string{"origin": forgeURL})
		// preserveAs collides with the existing origin, so `git remote add`
		// fails. The target must not have been rewritten.
		plan := mirrorRemotePlan{
			remote:      "origin",
			mirrorURL:   mirrorURL,
			replacedURL: forgeURL,
			preserveAs:  "origin",
		}
		require.Error(t, applyMirrorRemotePlan(t.Context(), dir, plan))
		require.Equal(t, forgeURL, remoteURL(t, dir, "origin"))
	})
}

func TestListGitRemotes(t *testing.T) {
	t.Parallel()
	dir := applyPlanRepo(t, map[string]string{
		"origin":   "git@github.com:octocat/hello-world.git",
		"upstream": "https://github.com/octocat/hello-world",
	})
	remotes, err := listGitRemotes(t.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"origin": true, "upstream": true}, remotes)
}

func TestListGitRemotes_NoRemotes(t *testing.T) {
	t.Parallel()
	dir := applyPlanRepo(t, nil)
	remotes, err := listGitRemotes(t.Context(), dir)
	require.NoError(t, err)
	require.Empty(t, remotes)
}

func TestResolveMirrorUseUpstream(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// remotes configures the repo's remotes before resolving.
		remotes map[string]string
		// remote is the write target passed to resolveMirrorUseUpstream;
		// defaults to "origin" when empty.
		remote    string
		arg       string
		wantOwner string
		wantRepo  string
		wantErr   string
	}{
		{
			name:      "explicit github url wins over origin",
			remotes:   map[string]string{"origin": "git@github.com:other/repo.git"},
			arg:       "github.com/OctoCat/Hello-World",
			wantOwner: "octocat", wantRepo: "hello-world",
		},
		{
			name:      "derives from an ssh origin",
			remotes:   map[string]string{"origin": "git@github.com:OctoCat/Hello-World.git"},
			wantOwner: "octocat", wantRepo: "hello-world",
		},
		{
			name:      "derives from an https origin",
			remotes:   map[string]string{"origin": "https://github.com/octocat/hello-world"},
			wantOwner: "octocat", wantRepo: "hello-world",
		},
		{
			// Re-running `use` on a clone that already goes through a mirror
			// must resolve, so switching clusters needs no retyped URL.
			name:      "derives from an entire origin",
			remotes:   map[string]string{"origin": "entire://aws-us-east-2.entire.io/gh/octocat/hello-world"},
			wantOwner: "octocat", wantRepo: "hello-world",
		},
		{
			// --remote names the WRITE target, which need not exist yet; repo
			// identity must still come from origin.
			name:      "falls back to origin when the target remote is absent",
			remotes:   map[string]string{"origin": "git@github.com:octocat/hello-world.git"},
			remote:    "entire",
			wantOwner: "octocat", wantRepo: "hello-world",
		},
		{
			// The target remote wins over origin, so re-running on an existing
			// side remote resolves from the repo it actually points at.
			name: "prefers the target remote over origin",
			remotes: map[string]string{
				"origin": "git@github.com:other/other-repo.git",
				"entire": "entire://aws-us-east-2.entire.io/gh/octocat/hello-world",
			},
			remote:    "entire",
			wantOwner: "octocat", wantRepo: "hello-world",
		},
		{
			// A target remote that cannot name an upstream must not shadow a
			// perfectly good origin.
			name: "falls back to origin when the target remote is not a GitHub repo",
			remotes: map[string]string{
				"origin": "git@github.com:octocat/hello-world.git",
				"weird":  "git@gitlab.com:acme/app.git",
			},
			remote:    "weird",
			wantOwner: "octocat", wantRepo: "hello-world",
		},
		{
			name:    "invalid explicit url errors",
			arg:     "https://gitlab.com/a/b",
			wantErr: "invalid <github-url>",
		},
		{
			name:    "no remotes errors with a pointer",
			wantErr: "pass the GitHub URL explicitly",
		},
		{
			name:    "non-github origin errors naming the reason",
			remotes: map[string]string{"origin": "git@gitlab.com:acme/app.git"},
			wantErr: "GitHub-only",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			remote := cmp.Or(tt.remote, "origin")
			owner, repo, err := resolveMirrorUseUpstream(t.Context(), applyPlanRepo(t, tt.remotes), remote, tt.arg)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantOwner, owner)
			require.Equal(t, tt.wantRepo, repo)
		})
	}
}

func TestReportMirrorRemotePlan(t *testing.T) {
	t.Parallel()
	const mirrorURL = "entire://aws-us-east-2.entire.io/gh/octocat/hello-world"

	// report returns the plan's stdout and stderr separately.
	report := func(plan mirrorRemotePlan) (stdout, stderr string) {
		var o, e strings.Builder
		reportMirrorRemotePlan(&o, &e, plan)
		return o.String(), e.String()
	}

	t.Run("replace reports the old URL and the preserve remote", func(t *testing.T) {
		t.Parallel()
		out, errOut := report(mirrorRemotePlan{
			remote:      "origin",
			mirrorURL:   mirrorURL,
			replacedURL: "git@github.com:octocat/hello-world.git",
			preserveAs:  "upstream",
		})
		require.Contains(t, out, "Repointed remote \"origin\"")
		require.Contains(t, out, mirrorURL)
		require.Contains(t, out, "was: git@github.com:octocat/hello-world.git")
		require.Contains(t, out, "as remote \"upstream\"")
		require.Contains(t, out, "git fetch origin")
		require.Empty(t, errOut, "a successful preserve warns about nothing")
	})

	// Even with no preserve remote, the replaced URL must be printed so the
	// previous value stays recoverable from the transcript.
	t.Run("replace without preserve still prints the old URL", func(t *testing.T) {
		t.Parallel()
		out, errOut := report(mirrorRemotePlan{
			remote:      "origin",
			mirrorURL:   mirrorURL,
			replacedURL: "https://github.com/octocat/hello-world",
		})
		require.Contains(t, out, "was: https://github.com/octocat/hello-world")
		require.NotContains(t, out, "Kept the previous URL")
		require.Empty(t, errOut, "an explicit --upstream '' opt-out is not warned about")
	})

	// The finding this guards: a skipped preservation must be stated outright, not
	// signalled by the absence of the "Kept the previous URL" line.
	t.Run("a skipped preserve warns loudly on stderr", func(t *testing.T) {
		t.Parallel()
		out, errOut := report(mirrorRemotePlan{
			remote:          "origin",
			mirrorURL:       mirrorURL,
			replacedURL:     "git@github.com:octocat/hello-world.git",
			preserveSkipped: "upstream",
		})
		require.Contains(t, out, "was: git@github.com:octocat/hello-world.git")
		require.NotContains(t, out, "Kept the previous URL")
		require.Contains(t, errOut, "WARNING")
		require.Contains(t, errOut, "NOT saved to git config")
		require.Contains(t, errOut, "remote \"upstream\" already exists")
		require.Contains(t, errOut, "git remote add <name> git@github.com:octocat/hello-world.git",
			"the warning must carry the URL needed to recover it")
	})

	t.Run("credentials are redacted in both the report and the warning", func(t *testing.T) {
		t.Parallel()
		out, errOut := report(mirrorRemotePlan{
			remote:          "origin",
			mirrorURL:       mirrorURL,
			replacedURL:     "https://user:s3cret@github.com/octocat/hello-world",
			preserveSkipped: "upstream",
		})
		require.NotContains(t, out, "s3cret")
		require.Contains(t, out, "github.com/octocat/hello-world")
		require.NotContains(t, errOut, "s3cret", "the recovery hint must not leak credentials either")
		require.Contains(t, errOut, "redacted")
	})

	t.Run("add reports no replacement", func(t *testing.T) {
		t.Parallel()
		out, errOut := report(mirrorRemotePlan{remote: "entire", mirrorURL: mirrorURL, add: true})
		require.Contains(t, out, "Added remote \"entire\"")
		require.NotContains(t, out, "was:")
		require.Contains(t, out, "git fetch entire")
		require.Empty(t, errOut)
	})

	t.Run("noop reports no change", func(t *testing.T) {
		t.Parallel()
		out, errOut := report(mirrorRemotePlan{remote: "origin", mirrorURL: mirrorURL, noop: true})
		require.Contains(t, out, "already points at the mirror")
		require.NotContains(t, out, "git fetch")
		require.Empty(t, errOut)
	})
}

func TestRepoMirrorUseCmd_FlagValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "bad remote", args: []string{"--remote", "-f"}, want: "invalid --remote"},
		{name: "bad upstream", args: []string{"--upstream", "bad name"}, want: "invalid --upstream"},
		{name: "bad positional cluster host", args: []string{"github.com/a/b", "not a host"}, want: "invalid cluster host"},
		{name: "bad cluster flag", args: []string{"--cluster", "not a host"}, want: "invalid cluster host"},
		{
			name: "positional and flag disagree",
			args: []string{"github.com/a/b", "aws-us-east-2.entire.io", "--cluster", "aws-eu-central-1.entire.io"},
			want: "disagree; pass only one",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := newRepoMirrorUseCmd()
			cmd.SetArgs(tt.args)
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})
			err := cmd.ExecuteContext(t.Context())
			require.ErrorContains(t, err, tt.want)
		})
	}
}

// The command must be reachable at `entire repo mirror use`, and must not have
// been registered as hidden.
func TestRepoMirrorUseCmd_Registered(t *testing.T) {
	t.Parallel()
	var found bool
	for _, c := range newRepoMirrorCmd().Commands() {
		if c.Name() == "use" {
			require.False(t, c.Hidden, "`repo mirror use` must be visible")
			found = true
		}
	}
	require.True(t, found, "`use` must be registered under `repo mirror`")
}

// huh answers an unreadable accessible prompt by writing the FIRST option's
// value and returning a nil error, so an interrupted prompt takes whichever
// branch is listed first. That must be the same outcome the non-interactive path
// produces with the same flags (repoint the target remote, preserving the old
// URL), or a Ctrl+D would silently diverge from the documented default. This
// pins that invariant: the prompt's first branch and the no-prompt path must
// plan identically.
// Not parallel: t.Setenv forces accessible mode process-wide so the prompt takes
// the deterministic text path instead of trying to open a TTY.
func TestPromptMirrorRemoteChoice_FirstOptionMatchesNonInteractive(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	const mirrorURL = "entire://aws-us-east-2.entire.io/gh/octocat/hello-world"
	const forgeURL = "git@github.com:octocat/hello-world.git"
	remotes := map[string]bool{"origin": true}

	cmd := newRepoMirrorUseCmd()
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SetContext(t.Context())

	// Runs with stdin at EOF under `go test`, so huh selects the first option.
	choice, err := promptMirrorRemoteChoice(cmd, "origin", forgeURL, mirrorURL, "upstream", remotes)
	require.NoError(t, err)

	fromPrompt := planMirrorRemote(choice.remote, mirrorURL, forgeURL, choice.upstream, remotes)
	fromFlags := planMirrorRemote("origin", mirrorURL, forgeURL, "upstream", remotes)
	require.Equal(t, fromFlags, fromPrompt,
		"the prompt's first option must plan the same writes as the non-interactive path")
	require.False(t, fromPrompt.add, "the first option must repoint, not add")
	require.Equal(t, "upstream", fromPrompt.preserveAs, "the replaced URL must still be preserved")
}

// gitRunner is the single chokepoint for the command's git writes; a failure
// must surface rather than being reported as success.
func TestApplyMirrorRemotePlan_GitFailureSurfaces(t *testing.T) {
	t.Parallel()
	plan := mirrorRemotePlan{remote: "origin", mirrorURL: "entire://h/gh/a/b"}
	// A path that is not a git repository makes `git remote set-url` fail.
	err := applyMirrorRemotePlan(context.Background(), t.TempDir(), plan)
	require.ErrorContains(t, err, "point remote \"origin\" at the mirror")
}
