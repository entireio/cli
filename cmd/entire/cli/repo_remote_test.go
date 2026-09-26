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

// planTestMirrorURL is the URL every planning case writes; only the remote's
// current state varies between them.
const planTestMirrorURL = "entire://aws-us-east-2.entire.io/gh/octocat/hello-world"

// mustPlan plans a write that is expected to be allowed, so each case below
// asserts the plan rather than restating the nil-error check.
func mustPlan(t *testing.T, remote, currentURL string, override bool, remotes map[string]bool) mirrorRemotePlan {
	t.Helper()
	plan, err := planMirrorRemote(remote, planTestMirrorURL, currentURL, nil, override, remotes)
	require.NoError(t, err)
	return plan
}

// An occupied remote name is refused the way `git remote add` refuses one, so
// the mirror URL never lands on a remote the caller did not mean to repoint.
// --override is the only way through, and it names itself in the refusal.
func TestPlanMirrorRemote_OccupiedNameNeedsOverride(t *testing.T) {
	t.Parallel()
	const mirrorURL = planTestMirrorURL
	const forgeURL = "git@github.com:octocat/hello-world.git"

	_, err := planMirrorRemote("origin", mirrorURL, forgeURL, nil, false, map[string]bool{"origin": true})
	require.ErrorContains(t, err, "origin")
	require.ErrorContains(t, err, "already exists")
	require.ErrorContains(t, err, "--override")

	// A remote already pointing at this very URL is what the caller asked for,
	// so it reports rather than refusing — re-running must stay safe.
	plan, err := planMirrorRemote("origin", mirrorURL, mirrorURL, nil, false, map[string]bool{"origin": true})
	require.NoError(t, err)
	require.True(t, plan.noop)
}

func TestPlanMirrorRemote(t *testing.T) {
	t.Parallel()
	const mirrorURL = planTestMirrorURL
	const forgeURL = "git@github.com:octocat/hello-world.git"

	t.Run("adds a remote that does not exist", func(t *testing.T) {
		t.Parallel()
		plan := mustPlan(t, "entire", "", false, map[string]bool{"origin": true})
		require.True(t, plan.add)
		require.False(t, plan.noop)
		require.Empty(t, plan.replacedURL)
		require.Equal(t, mirrorURL, plan.mirrorURL)
	})

	t.Run("--override replaces and records what it replaced", func(t *testing.T) {
		t.Parallel()
		plan := mustPlan(t, "origin", forgeURL, true, map[string]bool{"origin": true})
		require.False(t, plan.add)
		require.False(t, plan.noop)
		require.Equal(t, forgeURL, plan.replacedURL, "the report is the only record of it")
	})

	t.Run("noop when already pointing at the mirror", func(t *testing.T) {
		t.Parallel()
		plan := mustPlan(t, "origin", mirrorURL, false, map[string]bool{"origin": true})
		require.True(t, plan.noop)
		require.Empty(t, plan.replacedURL)
	})

	t.Run("noop tolerates surrounding whitespace and case", func(t *testing.T) {
		t.Parallel()
		plan := mustPlan(t, "origin", "  "+strings.ToUpper(mirrorURL)+"  ", false,
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

// mustListRemotes is the assertion that a plan wrote no remote beyond the one
// it named — the property that replaced --upstream.
func mustListRemotes(t *testing.T, dir string) map[string]bool {
	t.Helper()
	remotes, err := listGitRemotes(t.Context(), dir)
	require.NoError(t, err)
	return remotes
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
	const mirrorURL = planTestMirrorURL
	const forgeURL = "git@github.com:octocat/hello-world.git"

	// --override writes exactly one remote: the replaced URL is not copied
	// anywhere, so nothing but the named remote changes.
	t.Run("override repoints and writes nothing else", func(t *testing.T) {
		t.Parallel()
		dir := applyPlanRepo(t, map[string]string{"origin": forgeURL})
		plan := mustPlan(t, "origin", forgeURL, true, map[string]bool{"origin": true})
		require.NoError(t, applyMirrorRemotePlan(t.Context(), dir, plan))
		require.Equal(t, mirrorURL, remoteURL(t, dir, "origin"))
		require.Equal(t, map[string]bool{"origin": true}, mustListRemotes(t, dir))
	})

	t.Run("add creates a side remote and leaves origin alone", func(t *testing.T) {
		t.Parallel()
		dir := applyPlanRepo(t, map[string]string{"origin": forgeURL})
		plan := mustPlan(t, "entire", "", false, map[string]bool{"origin": true})
		require.NoError(t, applyMirrorRemotePlan(t.Context(), dir, plan))
		require.Equal(t, mirrorURL, remoteURL(t, dir, "entire"))
		require.Equal(t, forgeURL, remoteURL(t, dir, "origin"), "origin must be untouched")
	})

	t.Run("noop writes nothing", func(t *testing.T) {
		t.Parallel()
		dir := applyPlanRepo(t, map[string]string{"origin": mirrorURL})
		plan := mustPlan(t, "origin", mirrorURL, false, map[string]bool{"origin": true})
		require.NoError(t, applyMirrorRemotePlan(t.Context(), dir, plan))
		require.Equal(t, mirrorURL, remoteURL(t, dir, "origin"))
		require.Equal(t, map[string]bool{"origin": true}, mustListRemotes(t, dir))
	})

	// A failing git command echoes its argv into the error, and that error is a
	// plain (printed) error — so a credentialed URL must not survive into it.
	// Guards redactGitArgs, which is the only thing standing between the argv
	// and stderr.
	t.Run("a failed git command does not leak credentials from the argv", func(t *testing.T) {
		t.Parallel()
		dir := applyPlanRepo(t, map[string]string{"origin": forgeURL})
		// set-url against a remote that does not exist fails, with the URL in
		// the argv it reports.
		plan := mirrorRemotePlan{
			remote:    "no-such-remote",
			mirrorURL: "https://user:ghp_SUPERSECRET@github.com/octocat/hello-world",
		}
		err := applyMirrorRemotePlan(t.Context(), dir, plan)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "ghp_SUPERSECRET", "credentials must not reach the error message")
		require.NotContains(t, err.Error(), "user:", "userinfo must not reach the error message")
		// Still useful for diagnosis: the command and the host survive.
		require.Contains(t, err.Error(), "git remote set-url")
		require.Contains(t, err.Error(), "github.com/octocat/hello-world")
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

func TestResolveRemoteRepoRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// remotes configures the repo's remotes before resolving.
		remotes map[string]string
		// remote is the write target passed to resolveRemoteRepoRef;
		// defaults to "origin" when empty.
		remote    string
		arg       string
		wantForge string
		wantOwner string
		wantRepo  string
		wantErr   string
	}{
		{
			name:      "explicit repository reference wins over origin",
			remotes:   map[string]string{"origin": "git@github.com:other/repo.git"},
			arg:       "/gh/OctoCat/Hello-World",
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
			name: "falls back to origin when the target remote names no Entire forge",
			remotes: map[string]string{
				"origin": "git@github.com:octocat/hello-world.git",
				"weird":  "git@gitlab.com:acme/app.git",
			},
			remote:    "weird",
			wantOwner: "octocat", wantRepo: "hello-world",
		},
		{
			// A clone of an Entire-native repo resolves the same way, so its
			// remote can be repointed at another cluster without retyping it.
			name:      "derives from a native entire origin",
			remotes:   map[string]string{"origin": "entire://aws-us-east-2.entire.io/et/acme/web"},
			wantForge: nativeCloneForge,
			wantOwner: "acme", wantRepo: "web",
		},
		{
			name:      "an explicit native reference wins over origin",
			remotes:   map[string]string{"origin": "git@github.com:other/repo.git"},
			arg:       "/et/acme/web",
			wantForge: nativeCloneForge,
			wantOwner: "acme", wantRepo: "web",
		},
		{
			name:    "invalid explicit url errors",
			arg:     "https://gitlab.com/a/b",
			wantErr: "invalid <repo>",
		},
		{
			name:    "no remotes errors with a pointer",
			wantErr: "pass a repository reference explicitly",
		},
		{
			name:    "an origin on neither forge errors naming the reason",
			remotes: map[string]string{"origin": "git@gitlab.com:acme/app.git"},
			wantErr: "not an Entire or GitHub repo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			remote := cmp.Or(tt.remote, "origin")
			got, err := resolveRemoteRepoRef(t.Context(), applyPlanRepo(t, tt.remotes), remote, tt.arg)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			want := mirrorRepoRef{forge: cmp.Or(tt.wantForge, mirrorCloneForge), owner: tt.wantOwner, repo: tt.wantRepo}
			require.Equal(t, want, got)
		})
	}
}

func TestReportMirrorRemotePlan(t *testing.T) {
	t.Parallel()
	const mirrorURL = planTestMirrorURL

	report := func(plan mirrorRemotePlan) string {
		var o strings.Builder
		reportMirrorRemotePlan(&o, plan)
		return o.String()
	}

	// The replaced URL is printed because this output is the only record of it:
	// --override copies it nowhere.
	t.Run("override reports the URL it replaced", func(t *testing.T) {
		t.Parallel()
		out := report(mirrorRemotePlan{
			remote:      "origin",
			mirrorURL:   mirrorURL,
			replacedURL: "git@github.com:octocat/hello-world.git",
		})
		require.Contains(t, out, "Repointed remote \"origin\"")
		require.Contains(t, out, mirrorURL)
		require.Contains(t, out, "was: git@github.com:octocat/hello-world.git")
		require.Contains(t, out, "git fetch origin")
	})

	t.Run("credentials are redacted in the replaced URL", func(t *testing.T) {
		t.Parallel()
		out := report(mirrorRemotePlan{
			remote:      "origin",
			mirrorURL:   mirrorURL,
			replacedURL: "https://user:s3cret@github.com/octocat/hello-world",
		})
		require.NotContains(t, out, "s3cret")
		require.Contains(t, out, "github.com/octocat/hello-world")
	})

	t.Run("add reports no replacement", func(t *testing.T) {
		t.Parallel()
		out := report(mirrorRemotePlan{remote: "entire", mirrorURL: mirrorURL, add: true})
		require.Contains(t, out, "Added remote \"entire\"")
		require.NotContains(t, out, "was:")
		require.Contains(t, out, "git fetch entire")
	})

	t.Run("noop reports no change", func(t *testing.T) {
		t.Parallel()
		out := report(mirrorRemotePlan{remote: "origin", mirrorURL: mirrorURL, noop: true})
		require.Contains(t, out, "already points at the mirror")
		require.NotContains(t, out, "git fetch")
	})
}

func TestRepoRemoteAddCmd_ArgValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no remote name", args: []string{}, want: "accepts between 1 and 2 arg"},
		{name: "bad remote name", args: []string{"-f"}, want: "unknown shorthand flag"},
		{name: "remote name git would refuse", args: []string{"bad name"}, want: "invalid remote name"},
		{name: "bad cluster flag", args: []string{"entire", "--cluster", "not a host"}, want: "invalid --cluster"},
		{name: "a third positional is not a cluster host", args: []string{"entire", "github.com/a/b", "aws-us-east-2.entire.io"}, want: "accepts between 1 and 2 arg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := newRepoRemoteAddCmd()
			cmd.SetArgs(tt.args)
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})
			err := cmd.ExecuteContext(t.Context())
			require.ErrorContains(t, err, tt.want)
		})
	}
}

// `add` is the whole `repo remote` surface: it must be reachable, visible, and
// the only verb there — the URL-printing half was removed in favour of
// `entire repo mirror get`, which lists a clone URL per cluster.
func TestRepoRemoteCmd_AddIsTheOnlyVerb(t *testing.T) {
	t.Parallel()
	names := make([]string, 0, 1)
	for _, c := range newRepoRemoteCmd().Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		require.False(t, c.Hidden, "`repo remote %s` must be visible", c.Name())
		names = append(names, c.Name())
	}
	require.Equal(t, []string{"add"}, names)
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

// A remote URL is legitimately a local path, and the report is the only record
// of a URL --override just overwrote. RedactURL mangles one ("/srv/repo.git"
// becomes ":///srv/repo.git"); RedactURLOrPath is the one that must be used.
func TestRemoteReportKeepsALocalPathIntact(t *testing.T) {
	t.Parallel()
	const localPath = "/srv/repo.git"

	var out strings.Builder
	reportMirrorRemotePlan(&out, mirrorRemotePlan{
		remote:      "backup",
		mirrorURL:   planTestMirrorURL,
		replacedURL: localPath,
	})
	require.Contains(t, out.String(), "was: "+localPath)

	_, err := planMirrorRemote("backup", planTestMirrorURL, localPath, nil, false, map[string]bool{"backup": true})
	require.ErrorContains(t, err, localPath)
	require.NotContains(t, err.Error(), ":///srv")
}

// explicitPushURLs must answer only for a pushurl that is really configured.
// `git remote get-url --push` echoes the FETCH url when none is set, and
// against that every ordinary repoint looks stranded — the fetch URL is by
// definition about to change — so a plain --override would print a spurious
// "still pushes elsewhere" and talk the user into creating the very pushurl it
// was warning about.
func TestExplicitPushURLsIgnoresGitsFetchURLEcho(t *testing.T) {
	t.Parallel()
	const forgeURL = "git@github.com:octocat/hello-world.git"

	t.Run("no pushurl configured is no push URL", func(t *testing.T) {
		t.Parallel()
		dir := applyPlanRepo(t, map[string]string{"origin": forgeURL})
		require.Empty(t, explicitPushURLs(t.Context(), dir, "origin"))

		// The end-to-end consequence: nothing to warn about on a plain repoint.
		plan, err := planMirrorRemote("origin", planTestMirrorURL, forgeURL,
			explicitPushURLs(t.Context(), dir, "origin"), true, map[string]bool{"origin": true})
		require.NoError(t, err)
		require.Empty(t, plan.strandedPushURLs)

		var out strings.Builder
		reportMirrorRemotePlan(&out, plan)
		require.NotContains(t, out.String(), "still pushes elsewhere")
	})

	t.Run("a configured pushurl is reported", func(t *testing.T) {
		t.Parallel()
		dir := applyPlanRepo(t, map[string]string{"origin": forgeURL})
		set := exec.CommandContext(t.Context(), "git", "remote", "set-url", "--push", "origin", "https://github.com/o/fork.git")
		set.Dir = dir
		require.NoError(t, set.Run())
		require.Equal(t, []string{"https://github.com/o/fork.git"}, explicitPushURLs(t.Context(), dir, "origin"))
	})
}

// An explicit pushurl outranks the URL this command writes, so a remote that
// still pushes to the forge must be named rather than reported as fully
// repointed — the Long promises fetch AND push go through Entire.
func TestStrandedPushURLsAreReported(t *testing.T) {
	t.Parallel()

	t.Run("a differing push URL is stranded and named", func(t *testing.T) {
		t.Parallel()
		plan, err := planMirrorRemote("origin", planTestMirrorURL, "git@github.com:o/r.git",
			[]string{"https://github.com/o/fork.git"}, true, map[string]bool{"origin": true})
		require.NoError(t, err)
		require.Equal(t, []string{"https://github.com/o/fork.git"}, plan.strandedPushURLs)

		var out strings.Builder
		reportMirrorRemotePlan(&out, plan)
		require.Contains(t, out.String(), "still pushes elsewhere")
		require.Contains(t, out.String(), "https://github.com/o/fork.git")
		require.Contains(t, out.String(), "git remote set-url --push origin")
	})

	// A fetch URL already naming the mirror says nothing about an explicit
	// pushurl, so the no-op path must report one too — this is what an
	// idempotent script hits on its second run.
	t.Run("a no-op still names a stranded push URL", func(t *testing.T) {
		t.Parallel()
		plan, err := planMirrorRemote("origin", planTestMirrorURL, planTestMirrorURL,
			[]string{"https://github.com/o/fork.git"}, false, map[string]bool{"origin": true})
		require.NoError(t, err)
		require.True(t, plan.noop)

		var out strings.Builder
		reportMirrorRemotePlan(&out, plan)
		require.Contains(t, out.String(), "already points at the mirror")
		require.Contains(t, out.String(), "still pushes elsewhere",
			"a no-op fetch URL does not make an explicit pushurl harmless")
	})

	// `git remote get-url --push` echoes the fetch URL when no pushurl is set,
	// so the URL this run writes is not a stranded push target.
	t.Run("the mirror URL itself is not stranded", func(t *testing.T) {
		t.Parallel()
		plan, err := planMirrorRemote("origin", planTestMirrorURL, "git@github.com:o/r.git",
			[]string{planTestMirrorURL}, true, map[string]bool{"origin": true})
		require.NoError(t, err)
		require.Empty(t, plan.strandedPushURLs)

		var out strings.Builder
		reportMirrorRemotePlan(&out, plan)
		require.NotContains(t, out.String(), "still pushes elsewhere")
	})
}

// A bare "exit status 128" says nothing the user can act on; git's own sentence
// is the only thing that names the cause.
func TestGitRunnerSurfacesGitsOwnDiagnosis(t *testing.T) {
	t.Parallel()
	dir := applyPlanRepo(t, map[string]string{"origin": "https://github.com/o/r.git"})
	// Two url values make `git remote set-url` refuse with a message of its own.
	add := exec.CommandContext(t.Context(), "git", "config", "--add", "remote.origin.url", "https://github.com/o/second.git")
	add.Dir = dir
	require.NoError(t, add.Run())

	_, err := gitRunner(t.Context(), dir, "remote", "set-url", "origin", planTestMirrorURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "multiple values", "git's own diagnosis reaches the user")
	// The line naming what git could not set is the one that matters, and it
	// embeds a URL. Redacting per line would route the whole sentence through
	// RedactURL — which rebuilds it from a parsed scheme — and leave "fatal://".
	require.Contains(t, err.Error(), "could not set", "the sentence survives the URL redaction")
	require.NotContains(t, err.Error(), "fatal://", "a prose line must not be rebuilt as a URL")
}

// Redaction inside a stderr line must strip credentials from the URL without
// destroying the sentence carrying it.
func TestGitStderrRedactsOnlyTheURL(t *testing.T) {
	t.Parallel()
	err := &exec.ExitError{ProcessState: nil}
	err.Stderr = []byte("error: cannot fetch https://user:ghp_SECRET@github.com/o/r\nfatal: could not set 'remote.origin.url'\n")

	got := gitStderr(err)
	require.NotContains(t, got, "ghp_SECRET", "credentials are stripped")
	require.Contains(t, got, "error: cannot fetch https://github.com/o/r", "the sentence and the host survive")
	require.Contains(t, got, "fatal: could not set 'remote.origin.url'")
}

// A credential containing a quote must not split the match: ending it on a
// quote would redact the head of the URL and leave the tail of the password
// sitting in the output.
func TestGitStderrRedactsACredentialContainingAQuote(t *testing.T) {
	t.Parallel()
	err := &exec.ExitError{}
	err.Stderr = []byte("error: cannot fetch https://user:pa'ss@github.com/o/r now\n")

	got := gitStderr(err)
	require.NotContains(t, got, "ss@", "no part of the credential survives")
	require.NotContains(t, got, "pa'", "no part of the credential survives")
	require.Contains(t, got, "https://github.com/o/r", "and the URL is still readable")
}

// git quotes the URL in its own messages; the sentence must stay intact.
func TestGitStderrKeepsAQuotedURLsSentence(t *testing.T) {
	t.Parallel()
	err := &exec.ExitError{}
	err.Stderr = []byte("fatal: could not set 'remote.origin.url' to 'https://user:tok@third/z'\n")

	got := gitStderr(err)
	require.NotContains(t, got, "tok")
	require.Contains(t, got, "fatal: could not set 'remote.origin.url' to ")
	require.Contains(t, got, "https://third/z")
}
