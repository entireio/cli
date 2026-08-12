package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// runGitIsolated runs a git command with the same config isolation
// testutil.CreateBranch/GitReset use. Without it these shell-outs inherit the
// developer's global git config — a `tag.gpgSign = true` there fails the tag
// calls below on their machine but not in CI.
func runGitIsolated(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Env = testutil.GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitTagRepo tags a repo given its dir. Three separate copies of this
// shell-out existed across the plugin tests, none of them isolated.
func gitTagRepo(t *testing.T, dir, tag string) {
	t.Helper()
	runGitIsolated(t, "-C", dir, "tag", tag)
}

// repoDirFromURL maps a file:// fixture URL back to its directory.
func repoDirFromURL(repoURL string) string {
	return strings.TrimPrefix(repoURL, "file://")
}

// newTaggedPluginRepo creates a git repo with entire-plugin.yml (when
// metadata is non-empty) and the given tags, returning its file:// URL.
// file:// (not a bare path) forces git's transport machinery, matching how
// a real remote behaves for ls-remote and shallow clones.
func newTaggedPluginRepo(t *testing.T, metadata string, tags ...string) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	if metadata != "" {
		testutil.WriteFile(t, dir, pluginMetadataFileName, metadata)
		testutil.GitAdd(t, dir, pluginMetadataFileName)
	} else {
		testutil.WriteFile(t, dir, "README.md", "readme")
		testutil.GitAdd(t, dir, "README.md")
	}
	testutil.GitCommit(t, dir, "init")
	for _, tag := range tags {
		gitTagRepo(t, dir, tag)
	}
	return "file://" + filepath.ToSlash(dir)
}

func TestListRemoteSemverTags(t *testing.T) {
	t.Parallel()
	url := newTaggedPluginRepo(t, "", "v0.1.0", "v0.10.0", "v0.2.0", "not-a-version", "1.0.0")
	tags, err := listRemoteSemverTags(context.Background(), url)
	if err != nil {
		t.Fatalf("listRemoteSemverTags: %v", err)
	}
	// Bare "1.0.0" is valid semver (canonicalized); "not-a-version" is dropped.
	want := []string{"1.0.0", "v0.10.0", "v0.2.0", "v0.1.0"}
	if len(tags) != len(want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Errorf("tags[%d] = %q, want %q (full: %v)", i, tags[i], want[i], tags)
		}
	}
}

func TestListRemoteSemverTags_BadRemote(t *testing.T) {
	t.Parallel()
	if _, err := listRemoteSemverTags(context.Background(), "file:///nonexistent/repo"); err == nil {
		t.Error("listRemoteSemverTags succeeded on missing repo")
	}
}

func TestFetchPluginMetadataAtTag(t *testing.T) {
	t.Parallel()
	url := newTaggedPluginRepo(t, "name: demo\ndescription: a demo\n", "v1.0.0")
	meta, err := fetchPluginMetadataAtTag(context.Background(), url, "v1.0.0")
	if err != nil {
		t.Fatalf("fetchPluginMetadataAtTag: %v", err)
	}
	if meta == nil || meta.Name != "demo" {
		t.Errorf("meta = %+v, want name demo", meta)
	}
}

func TestFetchPluginMetadataAtTag_NoFileIsNilNil(t *testing.T) {
	t.Parallel()
	url := newTaggedPluginRepo(t, "", "v1.0.0")
	meta, err := fetchPluginMetadataAtTag(context.Background(), url, "v1.0.0")
	if err != nil {
		t.Fatalf("fetchPluginMetadataAtTag: %v", err)
	}
	if meta != nil {
		t.Errorf("meta = %+v, want nil for repo without %s", meta, pluginMetadataFileName)
	}
}

func TestValidatePluginRepoURL(t *testing.T) {
	t.Parallel()
	valid := []string{
		"https://github.com/entireio/entire-run",
		"ssh://git@github.com/entireio/entire-run.git",
		// Accepted only because the suite runs under `go test`; see
		// TestValidatePluginRepoURL_RejectsFileURLsInProduction.
		"file:///tmp/entire-run",
		"git@github.com:entireio/entire-run.git",
		"forge-user@git.example.com:team/entire-run",
	}
	for _, u := range valid {
		if err := validatePluginRepoURL(u); err != nil {
			t.Errorf("validatePluginRepoURL(%q) = %v, want nil", u, err)
		}
	}
	invalid := []string{
		"",
		"   ",
		"-x",
		// Unauthenticated, unencrypted transports: the catalog fetched over
		// them decides what installs without a prompt, so a network attacker
		// who can rewrite the transport chooses the binary.
		"http://forge.internal/team/entire-run.git",
		"git://example.com/entire-run.git",
		// Local remotes are rejected outside tests; see
		// TestValidatePluginRepoURL_RejectsFileURLsInProduction.
		"/srv/plugin-index",
		`C:\\repos\\entire-run`,
		// The injection payloads: git reads an option-shaped positional as an
		// option, and --upload-pack's value is shell-interpreted.
		"--upload-pack=touch /tmp/pwned; git-upload-pack",
		"--config=core.pager=sh",
		// git's ext:: transport runs a command directly, so a "--" separator
		// alone would not stop it; the scheme allowlist does.
		"ext::sh -c whoami",
		"./relative/path",
		"../up/one",
		"entire-run",
		"https://",
	}
	for _, u := range invalid {
		if err := validatePluginRepoURL(u); err == nil {
			t.Errorf("validatePluginRepoURL(%q) = nil, want error", u)
		}
	}
}

// TestGitRemote_OptionShapedURLNeverExecutes is the regression test for
// argument injection into the git CLI. Repo URLs arrive from index.json
// entries and from other plugins' entire-plugin.yml requirements, so they are
// attacker-influenced. git parses an option-shaped positional as an option;
// `--upload-pack=<cmd>` is shell-interpreted and, with no positional
// repository left, runs against the *ambient* repo's origin. This asserts the
// stronger property — not just that the call fails, but that the payload
// never ran.
//
// No t.Parallel: t.Chdir mutates process-global state.
func TestGitRemote_OptionShapedURLNeverExecutes(t *testing.T) {
	remote := t.TempDir()
	runGitIsolated(t, "init", "-q", "--bare", remote)
	work := t.TempDir()
	testutil.InitRepo(t, work)
	testutil.WriteFile(t, work, "f.txt", "x")
	testutil.GitAdd(t, work, "f.txt")
	testutil.GitCommit(t, work, "init")
	runGitIsolated(t, "-C", work, "remote", "add", "origin", remote)
	t.Chdir(work)

	markerDir := t.TempDir()
	for _, tc := range []struct {
		name   string
		marker string
	}{
		{name: "ls-remote", marker: filepath.Join(markerDir, "tags-pwned")},
		{name: "clone", marker: filepath.Join(markerDir, "meta-pwned")},
	} {
		payload := "--upload-pack=touch " + tc.marker + "; git-upload-pack"
		var err error
		if tc.name == "ls-remote" {
			_, err = listRemoteSemverTags(context.Background(), payload)
		} else {
			_, err = fetchPluginMetadataAtTag(context.Background(), payload, "v1.0.0")
		}
		if err == nil {
			t.Errorf("%s accepted an option-shaped repo URL", tc.name)
		}
		if _, statErr := os.Stat(tc.marker); statErr == nil {
			t.Fatalf("%s: injected payload executed — %s was created", tc.name, tc.marker)
		}
	}
}

func TestPluginNameFromRepoURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url, want string
		wantErr   bool
	}{
		{url: "https://github.com/entireio/entire-run", want: "run"},
		{url: "https://github.com/entireio/entire-run.git", want: "run"},
		{url: "https://github.com/entireio/entire-run/", want: "run"},
		{url: "git@github.com:entireio/entire-brain.git", want: "brain"},
		{url: "https://github.com/entireio/some-tool", wantErr: true},
		{url: "https://github.com/entireio/entire-agent-x", wantErr: true}, // reserved
	}
	for _, tt := range tests {
		got, err := pluginNameFromRepoURL(tt.url)
		if tt.wantErr {
			if err == nil {
				t.Errorf("pluginNameFromRepoURL(%q) = %q, want error", tt.url, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("pluginNameFromRepoURL(%q) = %q, %v; want %q", tt.url, got, err, tt.want)
		}
	}
}

// semver ranks v2.0.0-rc1 above stable v1.9.0, so listing prereleases would
// migrate every user onto a release candidate on the next `plugin upgrade
// --all` the moment an author pushed one. --pin installs an exact tag and
// bypasses this listing, which is the deliberate opt-in.
func TestListRemoteSemverTags_SkipsPrereleases(t *testing.T) {
	t.Parallel()
	url := newTaggedPluginRepo(t, "", "v1.0.0", "v1.9.0", "v2.0.0-rc1", "v2.0.0-alpha", "v2.0.0+build7")
	tags, err := listRemoteSemverTags(context.Background(), url)
	if err != nil {
		t.Fatalf("listRemoteSemverTags: %v", err)
	}
	for _, tg := range tags {
		if strings.Contains(tg, "-rc") || strings.Contains(tg, "-alpha") {
			t.Errorf("prerelease %q was listed", tg)
		}
	}
	// Build metadata is not a prerelease and must survive.
	if len(tags) == 0 || tags[0] != "v2.0.0+build7" {
		t.Errorf("tags = %v, want v2.0.0+build7 newest", tags)
	}
}

// A repo with only prereleases must say so, not merely "no semver tags".
func TestListRemoteSemverTags_PrereleaseOnlyRepoIsEmpty(t *testing.T) {
	t.Parallel()
	url := newTaggedPluginRepo(t, "", "v1.0.0-rc1")
	tags, err := listRemoteSemverTags(context.Background(), url)
	if err != nil {
		t.Fatalf("listRemoteSemverTags: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("tags = %v, want none", tags)
	}
}

// The shipped allowlist has no local-filesystem transport. file:// exists for
// the test suite only: nothing in production needs it — a file:// plugin
// repository cannot complete an install, since releaseAssetBaseURL requires an
// http(s) repo URL to locate the asset — and widening a security allowlist to
// pay for test convenience is the thing to avoid. A hostile catalog entry
// therefore cannot use one to probe the filesystem.
//
// The gate is a var so this test can reach the production branch;
// testing.Testing() is true wherever the test itself runs.
func TestValidatePluginRepoURL_RejectsFileURLsInProduction(t *testing.T) { //nolint:paralleltest // swaps a package-level seam
	original := allowLocalGitURL
	t.Cleanup(func() { allowLocalGitURL = original })
	allowLocalGitURL = func() bool { return false }

	for _, u := range []string{"file:///tmp/entire-run", "file://localhost/srv/entire-run"} {
		err := validatePluginRepoURL(u)
		if err == nil {
			t.Errorf("validatePluginRepoURL(%q) = nil in production, want a rejection", u)
			continue
		}
		if !strings.Contains(err.Error(), "must use one of") {
			t.Errorf("%q: err = %v, want the allowlist message", u, err)
		}
	}
	// The transports that do ship are unaffected.
	for _, u := range []string{"https://github.com/entireio/entire-run", "git@github.com:entireio/entire-run.git"} {
		if err := validatePluginRepoURL(u); err != nil {
			t.Errorf("validatePluginRepoURL(%q) = %v, want nil", u, err)
		}
	}
}

// The spawned integration binary cannot see testing.Testing(), so it is told
// through the environment. Losing that wiring would fail every plugin
// integration test with an allowlist error rather than silently, but pin the
// contract anyway since the env var is the only thing holding it up.
//
// Exercises fileRemotesAllowed rather than the ambient allowLocalGitURL: the
// spawned-binary case is precisely underTest=false, which an in-process test
// cannot produce. Substituting a stand-in closure here would only assert that
// the stand-in works.
func TestFileRemotesAllowed_HonorsTheSpawnedBinaryEnvVar(t *testing.T) {
	t.Parallel()
	if fileRemotesAllowed(false, "") {
		t.Error("file:// allowed in a spawned binary with the sentinel unset")
	}
	if !fileRemotesAllowed(false, "1") {
		t.Error("file:// refused in a spawned binary with the sentinel set")
	}
	if !fileRemotesAllowed(true, "") {
		t.Error("file:// refused under go test")
	}
}

// `git show <tag>:entire-plugin.yml` returns whatever an author committed, and
// cmd.Output() would buffer all of it. A hostile repository could otherwise
// hand back an arbitrarily large file and take the process down, so the runner
// caps stdout and reports the overflow instead of truncating.
func TestFetchPluginMetadataAtTag_RejectsOversizeMetadata(t *testing.T) {
	t.Parallel()
	huge := "name: oversize\ndescription: \"" + strings.Repeat("A", maxPluginMetadataSize) + "\"\n"
	url := newTaggedPluginRepo(t, huge, "v1.0.0")

	_, err := fetchPluginMetadataAtTag(context.Background(), url, "v1.0.0")
	if err == nil {
		t.Fatal("oversize entire-plugin.yml was accepted")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("err = %v, want it to name the size limit", err)
	}
}

// Capping stdout alone left stderr unbounded: cmd.Output() had supplied a
// 32 KiB ceiling for free, and replacing it with a plain bytes.Buffer silently
// dropped that. git's stderr carries the remote's sideband, so it is
// remote-controlled by the same standard stdout is.
//
// The two streams differ on overflow, and that difference is the point: a
// truncated stdout is not the payload and must fail, while stderr is
// diagnostic and must never fail a command git itself completed.
func TestLimitedWriter_OverflowPolicyDiffersPerStream(t *testing.T) {
	t.Parallel()

	stdout := &limitedWriter{limit: 8, failOnOverflow: true}
	n, err := stdout.Write([]byte("0123456789"))
	if !errors.Is(err, errOutputTooLarge) {
		t.Errorf("stdout Write err = %v, want errOutputTooLarge", err)
	}
	if n != 0 {
		t.Errorf("stdout Write n = %d, want 0 so os/exec stops the copy", n)
	}
	if !stdout.exceeded {
		t.Error("stdout did not record the overflow")
	}
	if got := stdout.buf.String(); got != "01234567" {
		t.Errorf("stdout kept %q, want the first 8 bytes for diagnostics", got)
	}

	stderr := &limitedWriter{limit: 8}
	if n, err := stderr.Write([]byte("0123456789")); err != nil || n != 10 {
		t.Errorf("stderr Write = (%d, %v), want (10, nil) so a chatty but successful command still succeeds", n, err)
	}
	if !stderr.exceeded {
		t.Error("stderr did not record the overflow")
	}
	if got := stderr.buf.String(); got != "01234567" {
		t.Errorf("stderr kept %q, want the first 8 bytes — git's own message precedes any remote bulk", got)
	}
	// Still bounded after further writes: the excess is dropped, not buffered.
	if _, err := stderr.Write(bytes.Repeat([]byte("x"), 1<<20)); err != nil {
		t.Errorf("stderr Write after overflow: %v", err)
	}
	if stderr.buf.Len() != 8 {
		t.Errorf("stderr buffered %d bytes, want it pinned at the 8-byte cap", stderr.buf.Len())
	}
}

// A repository whose metadata is comfortably under the cap still parses, so the
// bound is not so tight that real files trip it.
func TestFetchPluginMetadataAtTag_AcceptsNormalMetadata(t *testing.T) {
	t.Parallel()
	url := newTaggedPluginRepo(t, "name: sizable\ndescription: "+strings.Repeat("x", 4096)+"\n", "v1.0.0")
	meta, err := fetchPluginMetadataAtTag(context.Background(), url, "v1.0.0")
	if err != nil {
		t.Fatalf("fetchPluginMetadataAtTag: %v", err)
	}
	if meta == nil || meta.Name != "sizable" {
		t.Errorf("meta = %+v, want name sizable", meta)
	}
}
