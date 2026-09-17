package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestPluginStepPlainOutputIsImmediate(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	stop := startPluginStep(withPluginProgress(t.Context(), &out), "Downloading plugin archive...")
	if got := out.String(); got != "Downloading plugin archive...\n" {
		t.Fatalf("status must be visible before work completes, got %q", got)
	}
	stop()
	stop()
	if strings.Count(out.String(), "Downloading") != 1 || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("plain progress duplicated output or wrote terminal escapes: %q", out.String())
	}
}

func TestPluginInstallReportsStagesOnStderr(t *testing.T) { //nolint:paralleltest // isolates managed plugins and index cache
	withIsolatedPluginEnv(t)
	withIndexCache(t)
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")
	indexURL, _ := newIndexRepo(t, fmt.Sprintf(`{"version":1,"plugins":[{"name":"demo","repo_url":%q}]}`, repoURL))
	cmd := newPluginInstallCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := runRemoteInstall(t.Context(), cmd, installSource{Kind: installFromIndex, Ref: "demo"}, remoteInstallFlags{index: indexURL})
	if err != nil {
		t.Fatal(err)
	}
	stages := []string{
		"Checking plugin index...",
		"Finding latest plugin release...",
		"Fetching plugin metadata for v0.1.0...",
		"Locating plugin release files...",
		"Downloading plugin archive...",
		"Installing entire-demo v0.1.0...",
	}
	if got, want := errOut.String(), strings.Join(stages, "\n")+"\n"; got != want {
		t.Fatalf("install progress:\ngot %q\nwant %q", got, want)
	}
	if !strings.HasPrefix(out.String(), `Installed plugin "demo" v0.1.0 from `) || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("stdout should contain only the install result: %q", out.String())
	}
}

// Progress travels on the context, so a command that does the network work
// without opting in reports nothing. `plugin upgrade` list tags, fetches
// metadata, downloads and places a binary exactly as `plugin install` does,
// and the stages were silent there until the command opted in.
func TestPluginUpgradeReportsStagesOnStderr(t *testing.T) { //nolint:paralleltest // isolates managed plugins and index cache
	withIsolatedPluginEnv(t)
	withIndexCache(t)
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")
	if _, err := InstallPluginFromRepo(t.Context(), repoURL, "", RemoteInstallOptions{}); err != nil {
		t.Fatalf("InstallPluginFromRepo: %v", err)
	}
	srv := pluginReleaseServer(t, "0.1.0", "0.2.0")
	updateRepoMetadata(t, repoURL, fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL))
	gitTag(t, repoURL, remoteTestTagMid)

	cmd := newPluginUpgradeCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetContext(t.Context())
	cmd.SetArgs([]string{"demo"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{
		"Finding latest plugin release...",
		"Fetching plugin metadata for " + remoteTestTagMid + "...",
		"Downloading plugin archive...",
		"Installing entire-demo " + remoteTestTagMid + "...",
	} {
		if !strings.Contains(errOut.String(), stage) {
			t.Errorf("missing %q in upgrade progress: %q", stage, errOut.String())
		}
	}
	if !strings.Contains(out.String(), remoteTestTagOld+" → "+remoteTestTagMid) {
		t.Errorf("stdout should carry the upgrade result: %q", out.String())
	}
}

// installSource.Resolved binds an install to the entry its caller already
// showed the user. Without it runRemoteInstall reads the index a second time,
// and the two reads can disagree — a failed refresh leaves the freshness
// marker untouched so the next call retries and may succeed with different
// content, and a concurrent forced update rewrites the clone either way —
// letting the prompt name repository A while repository B is installed.
//
// The index here does not list "demo" at all, so a re-resolution cannot
// silently substitute: it fails outright, which is what makes the assertion
// unambiguous.
func TestRunRemoteInstall_ResolvedEntryIsNotReResolved(t *testing.T) { //nolint:paralleltest // isolates managed plugins and index cache
	withIsolatedPluginEnv(t)
	withIndexCache(t)
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")
	indexURL, _ := newIndexRepo(t, `{"version":1,"plugins":[{"name":"somethingelse","repo_url":"https://example.invalid/entire-somethingelse"}]}`)

	cmd := newPluginInstallCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	src := installSource{
		Kind:     installFromIndex,
		Ref:      "demo",
		Resolved: &PluginIndexEntry{Name: "demo", RepoURL: repoURL},
	}
	if err := runRemoteInstall(t.Context(), cmd, src, remoteInstallFlags{index: indexURL}); err != nil {
		t.Fatalf("runRemoteInstall: %v", err)
	}
	if !strings.Contains(out.String(), `Installed plugin "demo" `+remoteTestTagOld+" from "+repoURL) {
		t.Errorf("installed something other than the resolved repository: %q", out.String())
	}
	installed, err := FindInstalledPlugin("demo")
	if err != nil || installed == nil {
		t.Fatalf("FindInstalledPlugin: %v %v", installed, err)
	}
}
