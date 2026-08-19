package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Tags used across the remote-install lifecycle tests.
const (
	remoteTestTagOld = "v0.1.0"
	remoteTestTagMid = "v0.2.0"
)

// pluginReleaseServer serves goreleaser-shaped release assets for the "demo"
// plugin at the given versions, alongside the per-tag checksums.txt goreleaser
// publishes. Assets are tar.gz archives whose entire-<name> entry content is
// "payload-<version>", letting tests assert which version got installed.
//
// Serving checksums matters: installs require an authenticated download by
// default, so a server without them would exercise only the refusal path.
func pluginReleaseServer(t *testing.T, versions ...string) *httptest.Server {
	t.Helper()
	binName := pluginBinaryPrefix + "demo"
	assets := map[string][]byte{}
	// checksums.txt is per-tag, and the URL template routes by tag, so index
	// the manifests by the version segment of the request path.
	sumsByVersion := map[string][]byte{}
	for _, v := range versions {
		archive := makeTarGz(t, map[string][]byte{binName: []byte("payload-" + v)})
		asset := fmt.Sprintf("%s_%s_%s_%s.tar.gz", binName, v, runtime.GOOS, runtime.GOARCH)
		assets[asset] = archive
		sumsByVersion[v] = []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), asset))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if base == checksumsFileName {
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

// newDemoPluginRepo is the fixture every lifecycle test starts from: a release
// server for the "demo" plugin at the given versions, and a tagged repo whose
// metadata points at it. Eight tests opened with these same four lines.
func newDemoPluginRepo(t *testing.T, tags []string, versions ...string) (repoURL string, srv *httptest.Server) {
	t.Helper()
	srv = pluginReleaseServer(t, versions...)
	meta := fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL)
	return newTaggedPluginRepo(t, meta, tags...), srv
}

func gitTag(t *testing.T, repoURL, tag string) {
	t.Helper()
	gitTagRepo(t, repoDirFromURL(repoURL), tag)
}

// installedPayload reads back the payload of the "demo" fixture plugin, which
// is the only plugin these lifecycle tests install.
func installedPayload(t *testing.T) string {
	t.Helper()
	const name = "demo"
	p, err := FindInstalledPlugin(name)
	if err != nil || p == nil {
		t.Fatalf("FindInstalledPlugin(%s) = %v, %v", name, p, err)
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	return string(data)
}

func TestInstallPluginFromRepo_EndToEnd(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	ctx := context.Background()

	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld, remoteTestTagMid}, "0.1.0", "0.2.0")

	res, err := InstallPluginFromRepo(ctx, repoURL, "", RemoteInstallOptions{})
	if err != nil {
		t.Fatalf("InstallPluginFromRepo: %v", err)
	}
	if res.Manifest.Tag != remoteTestTagMid {
		t.Errorf("installed tag = %s, want newest v0.2.0", res.Manifest.Tag)
	}
	if res.Manifest.SHA256 == "" || res.Manifest.Asset == "" {
		t.Errorf("manifest missing provenance: %+v", res.Manifest)
	}
	if got := installedPayload(t); got != "payload-0.2.0" {
		t.Errorf("installed payload = %q, want payload-0.2.0", got)
	}

	// Second install without --force refuses.
	if _, err := InstallPluginFromRepo(ctx, repoURL, "", RemoteInstallOptions{}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("reinstall without force = %v, want already-installed error", err)
	}

	// Upgrade with no new tag reports up-to-date.
	o, err := UpgradeInstalledPlugin(ctx, "demo")
	if err != nil || !o.UpToDate {
		t.Errorf("upgrade with no new tag = %+v, %v; want UpToDate", o, err)
	}

	// New tag + new asset → upgrade replaces the binary.
	srv2 := pluginReleaseServer(t, "0.1.0", "0.2.0", "0.3.0")
	updateRepoMetadata(t, repoURL, fmt.Sprintf("name: demo\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv2.URL))
	gitTag(t, repoURL, "v0.3.0")
	o, err = UpgradeInstalledPlugin(ctx, "demo")
	if err != nil {
		t.Fatalf("UpgradeInstalledPlugin: %v", err)
	}
	if o.FromTag != remoteTestTagMid || o.ToTag != "v0.3.0" {
		t.Errorf("upgrade outcome = %+v, want v0.2.0 → v0.3.0", o)
	}
	if got := installedPayload(t); got != "payload-0.3.0" {
		t.Errorf("post-upgrade payload = %q, want payload-0.3.0", got)
	}
}

// updateRepoMetadata commits a new entire-plugin.yml to the test repo.
func updateRepoMetadata(t *testing.T, repoURL, metadata string) {
	t.Helper()
	dir := repoDirFromURL(repoURL)
	testutil.WriteFile(t, dir, pluginMetadataFileName, metadata)
	testutil.GitAdd(t, dir, pluginMetadataFileName)
	testutil.GitCommit(t, dir, "update metadata")
}

func TestInstallPluginFromRepo_PinnedSkipsUpgrade(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	ctx := context.Background()

	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld, remoteTestTagMid}, "0.1.0", "0.2.0")

	res, err := InstallPluginFromRepo(ctx, repoURL, "", RemoteInstallOptions{Pin: remoteTestTagOld})
	if err != nil {
		t.Fatalf("pinned install: %v", err)
	}
	if res.Manifest.Tag != remoteTestTagOld || !res.Manifest.Pinned {
		t.Errorf("manifest = %+v, want pinned v0.1.0", res.Manifest)
	}
	if got := installedPayload(t); got != "payload-0.1.0" {
		t.Errorf("payload = %q, want payload-0.1.0", got)
	}
	o, err := UpgradeInstalledPlugin(ctx, "demo")
	if err != nil || !o.Pinned {
		t.Errorf("upgrade of pinned = %+v, %v; want Pinned skip", o, err)
	}
}

func TestInstallPluginFromRepo_FallsBackPastAssetlessTag(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	ctx := context.Background()

	// Assets exist for 0.1.0 only; v0.2.0 is a pushed tag with no
	// published release. Install must fall back with the skipped tag
	// reported.
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld, remoteTestTagMid}, "0.1.0")

	res, err := InstallPluginFromRepo(ctx, repoURL, "", RemoteInstallOptions{})
	if err != nil {
		t.Fatalf("InstallPluginFromRepo: %v", err)
	}
	if res.Manifest.Tag != remoteTestTagOld {
		t.Errorf("tag = %s, want fallback to v0.1.0", res.Manifest.Tag)
	}
	if len(res.SkippedTags) != 1 || res.SkippedTags[0] != remoteTestTagMid {
		t.Errorf("SkippedTags = %v, want [v0.2.0]", res.SkippedTags)
	}
}

func TestUpgradeInstalledPlugin_NoManifest(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	if _, err := UpgradeInstalledPlugin(context.Background(), "localdev"); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Errorf("err = %v, want no-manifest explanation", err)
	}
}

func TestRemoveManagedPlugin_CleansBinAndPkg(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	ctx := context.Background()

	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")
	if _, err := InstallPluginFromRepo(ctx, repoURL, "", RemoteInstallOptions{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := RemoveManagedPlugin("demo"); err != nil {
		t.Fatalf("RemoveManagedPlugin: %v", err)
	}
	if p, err := FindInstalledPlugin("demo"); err != nil || p != nil {
		t.Error("bin entry survived removal")
	}
	if m, err := LoadPluginManifest("demo"); err != nil || m != nil {
		t.Error("manifest survived removal")
	}
}

func TestUpgradeInstalledPlugin_EquivalentTagSpellingIsUpToDate(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	// Manifest recorded the bare spelling; the remote tag carries the v
	// prefix. Equivalent semver must not trigger a reinstall — the repo
	// has no release server at all, so any download attempt would fail.
	repoURL := newTaggedPluginRepo(t, "", "v0.2.0")
	if err := SavePluginManifest(&PluginManifest{Name: "demo", RepoURL: repoURL, Tag: "0.2.0"}); err != nil {
		t.Fatal(err)
	}
	o, err := UpgradeInstalledPlugin(context.Background(), "demo")
	if err != nil {
		t.Fatalf("UpgradeInstalledPlugin: %v", err)
	}
	if !o.UpToDate {
		t.Errorf("outcome = %+v, want UpToDate for equivalent tag spellings", o)
	}
}

func TestUpgradeInstalledPlugin_AssetlessNewestTagReportsUpToDate(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	ctx := context.Background()
	// Assets exist only for 0.1.0; v0.2.0 is tagged but unpublished.
	// Upgrade falls back to the installed version and must report
	// up-to-date, not a misleading "v0.1.0 → v0.1.0" upgrade line.
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")
	if _, err := InstallPluginFromRepo(ctx, repoURL, "", RemoteInstallOptions{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	gitTag(t, repoURL, remoteTestTagMid) // newer tag, no assets
	o, err := UpgradeInstalledPlugin(ctx, "demo")
	if err != nil {
		t.Fatalf("UpgradeInstalledPlugin: %v", err)
	}
	if !o.UpToDate {
		t.Errorf("outcome = %+v, want UpToDate when fallback lands on the installed tag", o)
	}
}

// The installed name comes from the remote (entire-plugin.yml's name:, else the
// repo basename). When the caller already committed to a name — an index entry
// the user typed, a requirement being satisfied, a plugin being upgraded — the
// remote must not substitute a different one. Unchecked this let an index entry
// named "safe" install entire-hijack with no prompt, and let --force on plugin A
// replace an unrelated installed plugin B.
func TestInstallPluginFromRepo_RejectsNameMismatch(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")

	_, err := InstallPluginFromRepo(context.Background(), repoURL, "safe", RemoteInstallOptions{})
	if err == nil {
		t.Fatal("install proceeded under a name the caller did not request")
	}
	for _, want := range []string{`"demo"`, `"safe"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %s", err, want)
		}
	}
	// Nothing may land on PATH under either name.
	for _, name := range []string{"demo", "safe"} {
		p, findErr := FindInstalledPlugin(name)
		if findErr != nil {
			t.Fatalf("FindInstalledPlugin(%s): %v", name, findErr)
		}
		if p != nil {
			t.Errorf("plugin %q was installed despite the mismatch", name)
		}
	}

	// The same repo installs fine when the expectation matches, and when the
	// caller has no expectation at all (a bare `install <url>`, where the
	// repository legitimately names itself).
	if _, err := InstallPluginFromRepo(context.Background(), repoURL, "demo", RemoteInstallOptions{}); err != nil {
		t.Fatalf("matching expectation should install: %v", err)
	}
	if _, err := InstallPluginFromRepo(context.Background(), repoURL, "", RemoteInstallOptions{Force: true}); err != nil {
		t.Errorf("no expectation should install: %v", err)
	}
}

// A dependency installed under a different name would never satisfy the
// requirement, so dependencySatisfied and doctor would report it missing
// forever and every future parent install would re-attempt it.
func TestExecuteDepPlan_RejectsNameMismatch(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")

	// The plan says "sem"; the repo declares "demo".
	plan := &DepPlan{Actions: []DepAction{{Name: "sem", RepoURL: repoURL}}}
	_, err := ExecuteDepPlan(context.Background(), plan, false)
	if err == nil {
		t.Fatal("ExecuteDepPlan installed a dependency under the wrong name")
	}
	if !strings.Contains(err.Error(), "sem") {
		t.Errorf("err = %v, want it to name the unsatisfied requirement", err)
	}
	p, findErr := FindInstalledPlugin("demo")
	if findErr != nil {
		t.Fatalf("FindInstalledPlugin: %v", findErr)
	}
	if p != nil {
		t.Error("the mis-named dependency was installed anyway")
	}
}

// Upgrade knows the name of the plugin it is upgrading, so a repo that renamed
// itself must fail loudly rather than quietly installing a second plugin and
// leaving the original behind.
func TestUpgradeInstalledPlugin_RejectsRename(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	repoURL, srv := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0", "0.2.0")
	if _, err := InstallPluginFromRepo(context.Background(), repoURL, "", RemoteInstallOptions{}); err != nil {
		t.Fatalf("initial install: %v", err)
	}

	// The upstream renames itself and tags a newer release.
	updateRepoMetadata(t, repoURL,
		fmt.Sprintf("name: renamed\ndownload_url: \"%s/dl/{tag}/{asset}\"\n", srv.URL))
	gitTag(t, repoURL, remoteTestTagMid)

	if _, err := UpgradeInstalledPlugin(context.Background(), "demo"); err == nil {
		t.Error("upgrade accepted a renamed plugin")
	}
	p, findErr := FindInstalledPlugin("renamed")
	if findErr != nil {
		t.Fatalf("FindInstalledPlugin: %v", findErr)
	}
	if p != nil {
		t.Error("upgrade installed the plugin under its new name")
	}
	if installedPayload(t) != "payload-0.1.0" {
		t.Error("the original install was disturbed by the failed upgrade")
	}
}

// replaceBinary mutates pkg/<name>/ before the manifest catches up. Until it
// does, the manifest records the previous tag and binary_sha256 while the new
// binary is on disk, and checkManagedBinaryIntegrity reads that as tampering.
// The manifest must therefore be consistent with the binary once an upgrade
// returns — and doctor must be quiet about a healthy install.
func TestUpgrade_LeavesManifestConsistentWithBinary(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	ctx := context.Background()
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0", "0.2.0")
	if _, err := InstallPluginFromRepo(ctx, repoURL, "", RemoteInstallOptions{}); err != nil {
		t.Fatalf("initial install: %v", err)
	}
	// A new tag on the same commit is enough: the metadata already points at
	// the server, which serves 0.2.0 too. Rewriting the identical metadata
	// would be an empty commit.
	gitTag(t, repoURL, remoteTestTagMid)

	if _, err := UpgradeInstalledPlugin(ctx, "demo"); err != nil {
		t.Fatalf("UpgradeInstalledPlugin: %v", err)
	}

	m, err := LoadPluginManifest("demo")
	if err != nil || m == nil {
		t.Fatalf("LoadPluginManifest = %v, %v", m, err)
	}
	if m.Tag != remoteTestTagMid {
		t.Errorf("manifest tag = %s, want %s", m.Tag, remoteTestTagMid)
	}
	dir, err := PluginPkgDir("demo")
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := fileSHA256(filepath.Join(dir, pluginBinaryName("demo")))
	if err != nil {
		t.Fatal(err)
	}
	if onDisk != m.BinarySHA256 {
		t.Errorf("binary_sha256 = %s but the binary hashes to %s", m.BinarySHA256, onDisk)
	}
	// The whole point: doctor must not cry tampering after a clean upgrade.
	issues, err := RunPluginDoctor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range issues {
		if strings.Contains(i.Problem, "no longer matches") {
			t.Errorf("doctor reported false tampering after upgrade: %+v", i)
		}
	}
}

// --force is for replacing, so a repo move (entire-sem → entire-graph) must
// still work. What was missing is that the user learns *what* was displaced:
// a URL install's confirmation names a URL, and the remote picks which plugin
// that URL overwrites.
func TestInstallPluginFromRepo_ReportsWhatForceReplaced(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	ctx := context.Background()

	oldRepo, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")
	if _, err := InstallPluginFromRepo(ctx, oldRepo, "", RemoteInstallOptions{}); err != nil {
		t.Fatalf("initial install: %v", err)
	}

	// The same plugin name, published from a different repository.
	newRepo, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")
	res, err := InstallPluginFromRepo(ctx, newRepo, "", RemoteInstallOptions{Force: true})
	if err != nil {
		t.Fatalf("a repo move must still be allowed: %v", err)
	}
	if res.ReplacedFrom != oldRepo {
		t.Errorf("ReplacedFrom = %q, want the previous repo %q", res.ReplacedFrom, oldRepo)
	}

	// Reinstalling from the same repo is not a displacement and must stay quiet.
	res, err = InstallPluginFromRepo(ctx, newRepo, "", RemoteInstallOptions{Force: true})
	if err != nil {
		t.Fatalf("reinstall from the same repo: %v", err)
	}
	if res.ReplacedFrom != "" {
		t.Errorf("ReplacedFrom = %q for a same-repo reinstall, want empty", res.ReplacedFrom)
	}
}

// After a real install, the managed directories hold exactly what we put there:
// one entire-<name> binary plus its manifest under pkg/, and one entire-<name>
// entry in bin/. Nothing the archive carried leaks out alongside it.
func TestInstallPluginFromRepo_WritesOnlyItsOwnBinary(t *testing.T) { //nolint:paralleltest // mutates env
	withIsolatedPluginEnv(t)
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")
	if _, err := InstallPluginFromRepo(context.Background(), repoURL, "", RemoteInstallOptions{}); err != nil {
		t.Fatalf("InstallPluginFromRepo: %v", err)
	}

	pkgDir, err := PluginPkgDir("demo")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		got[e.Name()] = true
	}
	want := map[string]bool{pluginBinaryName("demo"): true, pluginManifestFileName: true}
	if len(got) != len(want) {
		t.Errorf("pkg dir holds %v, want exactly %v", got, want)
	}
	for name := range want {
		if !got[name] {
			t.Errorf("pkg dir missing %q (has %v)", name, got)
		}
	}

	installed, err := ListInstalledPlugins()
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 {
		t.Fatalf("bin dir holds %d entries, want 1", len(installed))
	}
	if base := filepath.Base(installed[0].Path); base != pluginBinaryName("demo") {
		t.Errorf("bin entry is %q, want %q", base, pluginBinaryName("demo"))
	}
}
