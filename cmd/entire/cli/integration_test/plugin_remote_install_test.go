//go:build integration

package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Integration tests for remote plugin install: the spawned binary resolves
// tags over the git protocol (file:// repos), downloads release assets from
// a local HTTP server (download_url template), installs into an isolated
// ENTIRE_PLUGIN_DIR, and then the kubectl dispatcher actually runs the
// installed plugin — the full M1+M2+M3 path with zero real network.

// makeScriptTarGz builds a tar.gz holding an executable shell script named
// entire-<name> that echoes a version marker.
func makeScriptTarGz(t *testing.T, name, marker string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	script := "#!/bin/sh\necho " + marker + "\n"
	if err := tw.WriteHeader(&tar.Header{Name: "entire-" + name, Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(script)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// startReleaseServer serves tar.gz assets for the named plugins/versions,
// plus the per-tag checksums.txt goreleaser publishes alongside them. Installs
// require an authenticated download by default, so serving checksums is what
// keeps these tests on the production code path rather than the refusal path.
func startReleaseServer(t *testing.T, plugins map[string][]string) *httptest.Server {
	t.Helper()
	assets := map[string][]byte{}
	// Keyed by version: checksums.txt is per-tag and the URL template routes
	// by tag, so the handler picks the manifest from the request path.
	sumsByVersion := map[string][]byte{}
	for name, versions := range plugins {
		for _, v := range versions {
			asset := fmt.Sprintf("entire-%s_%s_%s_%s.tar.gz", name, v, runtime.GOOS, runtime.GOARCH)
			data := makeScriptTarGz(t, name, name+"-ran-v"+v)
			assets[asset] = data
			sumsByVersion[v] = append(sumsByVersion[v], []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(data), asset))...)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if base == "checksums.txt" {
			for v, sums := range sumsByVersion {
				if strings.Contains(r.URL.Path, "/v"+v+"/") || strings.Contains(r.URL.Path, "/"+v+"/") {
					_, _ = w.Write(sums) //nolint:errcheck // test server write; failure surfaces as a client error
					return
				}
			}
			http.NotFound(w, r)
			return
		}
		if data, ok := assets[base]; ok {
			_, _ = w.Write(data) //nolint:errcheck // test server write; failure surfaces as a client error
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newPluginRepo creates a tagged git repo with entire-plugin.yml.
func newPluginRepo(t *testing.T, metadata string, tags ...string) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "entire-plugin.yml", metadata)
	testutil.GitAdd(t, dir, "entire-plugin.yml")
	testutil.GitCommit(t, dir, "init")
	for _, tag := range tags {
		if out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "tag", tag).CombinedOutput(); err != nil {
			t.Fatalf("git tag: %v: %s", err, out)
		}
	}
	return "file://" + filepath.ToSlash(dir)
}

// newIndexRepo creates a git repo holding index.json.
func newIndexRepo(t *testing.T, indexJSON string) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "index.json", indexJSON)
	testutil.GitAdd(t, dir, "index.json")
	testutil.GitCommit(t, dir, "index")
	return "file://" + filepath.ToSlash(dir)
}

// pluginTestEnv builds the child env and the directory to run it from:
// isolated plugin dir + cache + index URL.
//
// The base MUST be testutil.GitIsolatedEnv(), not os.Environ(). The
// process-wide ENTIRE_TEST_GIT_HERMETIC=1 set by TestMain is inert on its own —
// it bites through the per-host http.<url>.proxy entries GitIsolatedEnv writes
// into an isolated GIT_CONFIG_GLOBAL, which point github.com/gitlab.com at a
// dead loopback address so a stray network dial fails fast instead of
// succeeding quietly. Building from os.Environ() sets the flag with no config
// backing it: tripwire absent, and the developer's real global git config
// inherited. That would let these tests pass by cloning the real
// entireio/plugin-index if index-URL resolution ever regressed to the built-in
// default — precisely the regression they exist to catch.
func pluginTestEnv(t *testing.T, indexURL string) (env []string, workDir string) {
	t.Helper()
	env = append(testutil.GitIsolatedEnv(),
		"ENTIRE_PLUGIN_DIR="+t.TempDir(),
		"XDG_CACHE_HOME="+t.TempDir(),
	)
	if indexURL != "" {
		env = append(env, "ENTIRE_PLUGIN_INDEX_URL="+indexURL)
	}
	// A dedicated CWD, because settings resolve from the working directory: an
	// unset cmd.Dir leaves the child standing in the real repo and reading its
	// committed .entire/settings.json.
	return env, t.TempDir()
}

func runEntire(t *testing.T, env []string, workDir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := execx.NonInteractive(context.Background(), getTestBinary(), args...)
	cmd.Env = env
	cmd.Dir = workDir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return out.String(), errBuf.String(), err
}

func TestPluginRemoteInstall_FromIndexAndDispatch(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsGOOS {
		t.Skip("shell-script plugin payloads are Unix-only")
	}
	srv := startReleaseServer(t, map[string][]string{"demo": {"0.1.0"}})
	repoURL := newPluginRepo(t, fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL), "v0.1.0")
	indexURL := newIndexRepo(t, fmt.Sprintf(`{"version":1,"plugins":[{"name":"demo","repo_url":"%s","description":"Demo plugin","official":true}]}`, repoURL))
	env, workDir := pluginTestEnv(t, indexURL)

	// Bare-name install resolves through the index; index-listed repos
	// need no --yes.
	stdout, stderr, err := runEntire(t, env, workDir, "plugin", "install", "demo")
	if err != nil {
		t.Fatalf("plugin install demo: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, `Installed plugin "demo" v0.1.0`) {
		t.Errorf("install output = %q", stdout)
	}

	// The dispatcher must now run it as `entire demo`.
	stdout, stderr, err = runEntire(t, env, workDir, "demo")
	if err != nil {
		t.Fatalf("entire demo: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "demo-ran-v0.1.0") {
		t.Errorf("dispatched plugin output = %q, want demo-ran-v0.1.0", stdout)
	}

	// list shows the tag from the manifest.
	stdout, _, err = runEntire(t, env, workDir, "plugin", "list")
	if err != nil {
		t.Fatalf("plugin list: %v", err)
	}
	if !strings.Contains(stdout, "demo") || !strings.Contains(stdout, "v0.1.0") {
		t.Errorf("plugin list output = %q, want name+tag", stdout)
	}
}

func TestPluginRemoteInstall_URLNeedsYesWhenUnlisted(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsGOOS {
		t.Skip("shell-script plugin payloads are Unix-only")
	}
	srv := startReleaseServer(t, map[string][]string{"solo": {"0.1.0"}})
	repoURL := newPluginRepo(t, fmt.Sprintf("name: solo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL), "v0.1.0")
	// Empty index: the URL is unlisted → untrusted.
	indexURL := newIndexRepo(t, `{"version":1,"plugins":[]}`)
	env, workDir := pluginTestEnv(t, indexURL)

	// Non-interactive without --yes refuses.
	_, stderr, err := runEntire(t, env, workDir, "plugin", "install", repoURL)
	if err == nil {
		t.Fatal("unlisted URL install without --yes succeeded in non-interactive mode")
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("stderr = %q, want a hint about --yes", stderr)
	}

	// With --yes it proceeds.
	stdout, stderr, err := runEntire(t, env, workDir, "plugin", "install", repoURL, "--yes")
	if err != nil {
		t.Fatalf("install --yes: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, `Installed plugin "solo" v0.1.0`) {
		t.Errorf("install output = %q", stdout)
	}
}

func TestPluginRemoteInstall_DependenciesAndRemoveGuard(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsGOOS {
		t.Skip("shell-script plugin payloads are Unix-only")
	}
	srv := startReleaseServer(t, map[string][]string{"brainy": {"0.1.0"}, "semy": {"0.1.0"}})
	semRepo := newPluginRepo(t, fmt.Sprintf("name: semy\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL), "v0.1.0")
	brainRepo := newPluginRepo(t, fmt.Sprintf(
		// requires[] has no repo_url any more; a leftover one must be ignored and
		// the dependency resolved from the index. Point it somewhere unreachable
		// so honoring it would fail the test loudly.
		"name: brainy\ndownload_url: \"%s/dl/{tag}/{asset}\"\nrequires:\n  - name: semy\n    repo_url: https://unreachable.invalid/entire-semy\n", srv.URL), "v0.1.0")
	indexURL := newIndexRepo(t, fmt.Sprintf(
		`{"version":1,"plugins":[{"name":"brainy","repo_url":"%s"},{"name":"semy","repo_url":"%s"}]}`, brainRepo, semRepo))
	env, workDir := pluginTestEnv(t, indexURL)

	stdout, stderr, err := runEntire(t, env, workDir, "plugin", "install", "brainy", "--yes")
	if err != nil {
		t.Fatalf("install brainy: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, `Installed dependency "semy"`) {
		t.Errorf("install output = %q, want dependency install", stdout)
	}

	// Both dispatchable.
	for _, name := range []string{"brainy", "semy"} {
		out, _, err := runEntire(t, env, workDir, name)
		if err != nil || !strings.Contains(out, name+"-ran-v0.1.0") {
			t.Errorf("entire %s = %q, %v", name, out, err)
		}
	}

	// Remove guard: semy is required by brainy.
	_, stderr, err = runEntire(t, env, workDir, "plugin", "remove", "semy")
	if err == nil {
		t.Fatal("remove of depended-on plugin succeeded without --force")
	}
	if !strings.Contains(stderr, "brainy") {
		t.Errorf("remove stderr = %q, want dependent named", stderr)
	}
	if _, _, err := runEntire(t, env, workDir, "plugin", "remove", "semy", "--force"); err != nil {
		t.Errorf("remove --force failed: %v", err)
	}

	// Doctor now reports the missing dependency, exit code 1.
	stdout, _, err = runEntire(t, env, workDir, "plugin", "doctor")
	if err == nil {
		t.Error("doctor exit code = 0 with missing dependency, want failure")
	}
	if !strings.Contains(stdout, `requires "semy"`) {
		t.Errorf("doctor output = %q, want missing-dep report", stdout)
	}
}

func TestPluginSearchAndInfo(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsGOOS {
		t.Skip("shell-script plugin payloads are Unix-only")
	}
	indexURL := newIndexRepo(t, `{"version":1,"plugins":[
		{"name":"alpha","repo_url":"https://example.com/entire-alpha","description":"First letter","official":true},
		{"name":"beta","repo_url":"https://example.com/entire-beta","description":"Second letter"}]}`)
	env, workDir := pluginTestEnv(t, indexURL)

	stdout, stderr, err := runEntire(t, env, workDir, "plugin", "search", "letter")
	if err != nil {
		t.Fatalf("plugin search: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "alpha") || !strings.Contains(stdout, "beta") || !strings.Contains(stdout, "[official]") {
		t.Errorf("search output = %q", stdout)
	}

	stdout, _, err = runEntire(t, env, workDir, "plugin", "info", "alpha")
	if err != nil {
		t.Fatalf("plugin info: %v", err)
	}
	for _, want := range []string{"First letter", "https://example.com/entire-alpha", "Official:    true"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("info output missing %q: %s", want, stdout)
		}
	}
}
