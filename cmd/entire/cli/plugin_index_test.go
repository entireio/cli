package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// withIndexCache points the plugin-index cache at a scratch dir, so tests never
// read or write the developer's real ~/.cache/entire. Sibling of withPluginDir.
func withIndexCache(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
}

// newIndexRepo creates a git repo holding index.json and returns its
// file:// URL.
func newIndexRepo(t *testing.T, indexJSON string) (url, dir string) {
	t.Helper()
	dir = t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, pluginIndexFileName, indexJSON)
	testutil.GitAdd(t, dir, pluginIndexFileName)
	testutil.GitCommit(t, dir, "index")
	return "file://" + filepath.ToSlash(dir), dir
}

const testIndexJSON = `{
  "version": 1,
  "plugins": [
    {"name": "run", "repo_url": "https://github.com/entireio/entire-run", "description": "Run apps", "official": true},
    {"name": "sem", "repo_url": "https://github.com/entireio/entire-sem", "description": "Semantic search"},
    {"name": "agent-bad", "repo_url": "https://example.com/x", "description": "invalid, filtered"},
    {"name": "noend", "repo_url": "", "description": "missing repo, filtered"}
  ]
}`

func TestSyncPluginIndex_CloneSearchFind(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	withIndexCache(t)
	url, _ := newIndexRepo(t, testIndexJSON)
	idx, err := SyncPluginIndex(context.Background(), url, false)
	if err != nil {
		t.Fatalf("SyncPluginIndex: %v", err)
	}
	// Invalid entries (reserved name, empty repo) are filtered, not fatal.
	if len(idx.Plugins) != 2 {
		t.Fatalf("plugins = %+v, want 2 valid entries", idx.Plugins)
	}
	if e := idx.Find("run"); e == nil || !e.Official {
		t.Errorf("Find(run) = %+v", e)
	}
	if got := idx.Search("semantic"); len(got) != 1 || got[0].Name != "sem" {
		t.Errorf("Search(semantic) = %+v", got)
	}
	if e := idx.FindByRepoURL("https://github.com/entireio/entire-run.git"); e == nil || e.Name != "run" {
		t.Errorf("FindByRepoURL should normalize the .git suffix, got %+v", e)
	}
	if e := idx.FindByRepoURL("https://github.com/entireio/entire-other"); e != nil {
		t.Errorf("FindByRepoURL matched an unlisted repo: %+v", e)
	}
}

func TestSyncPluginIndex_RefreshPicksUpNewEntries(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	withIndexCache(t)
	url, dir := newIndexRepo(t, `{"version":1,"plugins":[{"name":"run","repo_url":"https://x.example/entire-run"}]}`)
	ctx := context.Background()
	idx, err := SyncPluginIndex(ctx, url, false)
	if err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Fatalf("initial plugins = %d, want 1", len(idx.Plugins))
	}

	testutil.WriteFile(t, dir, pluginIndexFileName, `{"version":1,"plugins":[{"name":"run","repo_url":"https://x.example/entire-run"},{"name":"sem","repo_url":"https://x.example/entire-sem"}]}`)
	testutil.GitAdd(t, dir, pluginIndexFileName)
	testutil.GitCommit(t, dir, "add sem")

	// Within TTL the cached copy is served...
	idx, err = SyncPluginIndex(ctx, url, false)
	if err != nil {
		t.Fatalf("cached sync: %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Errorf("TTL-fresh sync re-fetched: got %d plugins", len(idx.Plugins))
	}
	// ...force bypasses the TTL.
	idx, err = SyncPluginIndex(ctx, url, true)
	if err != nil {
		t.Fatalf("forced sync: %v", err)
	}
	if len(idx.Plugins) != 2 {
		t.Errorf("forced sync plugins = %d, want 2", len(idx.Plugins))
	}
}

func TestSyncPluginIndex_OfflineUsesStaleCopy(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	withIndexCache(t)
	url, dir := newIndexRepo(t, `{"version":1,"plugins":[{"name":"run","repo_url":"https://x.example/entire-run"}]}`)
	ctx := context.Background()
	if _, err := SyncPluginIndex(ctx, url, false); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	// Simulate the remote disappearing (laptop offline / index moved).
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove remote dir: %v", err)
	}
	idx, err := SyncPluginIndex(ctx, url, true) // force → refresh fails → stale copy
	if err != nil {
		t.Fatalf("offline sync should fall back to cache: %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Errorf("stale copy plugins = %d, want 1", len(idx.Plugins))
	}
}

// version is advisory, not a gate. Enforcing it would guard a migration that
// can never happen — the index is one shared resource read by every shipped
// CLI, so a bump breaks discovery fleet-wide — while punishing the case that
// does happen: an index (often hand-written, as an internal catalog set via
// ENTIRE_PLUGIN_INDEX_URL is) that omits the field, or one that grows a field
// this CLI ignores.
func TestSyncPluginIndex_VersionIsAdvisory(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	withIndexCache(t)
	entry := `{"name":"run","repo_url":"https://x.example/entire-run"}`
	for _, tt := range []struct{ label, json string }{
		{label: "omitted", json: `{"plugins":[` + entry + `]}`},
		{label: "current", json: `{"version":1,"plugins":[` + entry + `]}`},
		{label: "future", json: `{"version":99,"plugins":[` + entry + `]}`},
		{label: "future with unknown fields", json: `{"version":99,"generated_at":"x","plugins":[` + entry + `]}`},
	} {
		url, _ := newIndexRepo(t, tt.json)
		idx, err := SyncPluginIndex(context.Background(), url, false)
		if err != nil {
			t.Errorf("%s: SyncPluginIndex = %v, want the catalog to load", tt.label, err)
			continue
		}
		if idx.Find("run") == nil {
			t.Errorf("%s: entry dropped; got %+v", tt.label, idx.Plugins)
		}
	}
}

// An index entry whose repo_url is option-shaped would reach the git CLI on
// an index-resolved install, which is treated as trusted and never prompts.
// Bad entries are dropped the same way an invalid name is, so one hostile
// row can't take out the whole catalog.
func TestSyncPluginIndex_DropsEntriesWithUnusableRepoURL(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	withIndexCache(t)
	url, _ := newIndexRepo(t, `{"version":1,"plugins":[
		{"name":"evil","repo_url":"--upload-pack=touch /tmp/pwned; git-upload-pack"},
		{"name":"alsoevil","repo_url":"ext::sh -c whoami"},
		{"name":"good","repo_url":"https://github.com/entireio/entire-good"}
	]}`)
	idx, err := SyncPluginIndex(context.Background(), url, false)
	if err != nil {
		t.Fatalf("SyncPluginIndex: %v", err)
	}
	if idx.Find("evil") != nil || idx.Find("alsoevil") != nil {
		t.Error("index kept an entry whose repo_url is not a usable git URL")
	}
	if idx.Find("good") == nil {
		t.Error("index dropped the valid entry alongside the bad ones")
	}
}

// The index URL itself reaches `git clone` as a positional, and --index /
// ENTIRE_PLUGIN_INDEX_URL bypass the settings validator entirely.
func TestSyncPluginIndex_RejectsUnusableIndexURL(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	withIndexCache(t)
	if _, err := SyncPluginIndex(context.Background(), "--upload-pack=touch /tmp/pwned; git-upload-pack", false); err == nil {
		t.Error("SyncPluginIndex accepted an option-shaped index URL")
	}
}

// Precedence is --index > ENTIRE_PLUGIN_INDEX_URL > built-in default, and
// repo-level settings are deliberately not a source: .entire/settings.json is
// committed and resolved from the working directory, so honoring it let a
// cloned repository redirect the catalog — and an index-listed repo installs
// with no confirmation prompt.
func TestResolvePluginIndexURL_Precedence(t *testing.T) { //nolint:paralleltest // mutates env
	t.Setenv(pluginIndexEnvVar, "")
	if got := resolvePluginIndexURL("https://flag.example/idx"); got != "https://flag.example/idx" {
		t.Errorf("flag should win: %q", got)
	}
	if got := resolvePluginIndexURL(""); got != defaultPluginIndexURL {
		t.Errorf("bare call should fall back to the built-in default: %q", got)
	}
	t.Setenv(pluginIndexEnvVar, "https://env.example/idx")
	if got := resolvePluginIndexURL(""); got != "https://env.example/idx" {
		t.Errorf("env should win over the default: %q", got)
	}
	if got := resolvePluginIndexURL("https://flag.example/idx"); got != "https://flag.example/idx" {
		t.Errorf("flag should win over env: %q", got)
	}
}

// A committed .entire/settings.json must not be able to steer the catalog. The
// settings schema no longer has the key at all, so the strict loader rejects it
// outright rather than honoring it — this pins that the door stays shut.
func TestPluginIndexURL_NotConfigurableViaRepoSettings(t *testing.T) { //nolint:paralleltest // mutates env
	t.Setenv(pluginIndexEnvVar, "")
	dir := t.TempDir()
	testutil.WriteFile(t, dir, ".entire/settings.json",
		`{"plugins":{"index_url":"https://evil.example/idx"}}`)
	t.Chdir(dir)
	if got := resolvePluginIndexURL(""); got != defaultPluginIndexURL {
		t.Errorf("repo settings steered the index to %q", got)
	}
}

func TestParseInstallSource(t *testing.T) {
	t.Parallel()
	for arg, want := range map[string]installArgKind{
		"https://github.com/entireio/entire-run": installFromURL,
		"git@github.com:entireio/entire-run.git": installFromURL,
		// Any SSH username, not just "git" — the classifier and
		// validatePluginRepoURL share one definition of scp-like, and the
		// validator has always accepted these. Note these contain a path
		// separator, so the scp-like test has to run first.
		"deploy@git.corp.io:group/entire-foo.git": installFromURL,
		"ci-bot@git.corp.io:entire-foo":           installFromURL,
		"file:///tmp/repo":                        installFromURL,
		"./dist/entire-run":                       installFromPath,
		"dist/entire-run":                         installFromPath,
		"../entire-run":                           installFromPath,
		// A relative path is not scp-like even though it has a dot and a colon
		// nearby; the regex anchors the username at the start.
		"./deploy@host:entire-run": installFromPath,
		"run":                      installFromIndex,
		"brain":                    installFromIndex,
		// Bare names are index lookups even when a same-named file exists
		// in the CWD — classification is pure string logic, never stat,
		// so a stray local file can't shadow an index name. Local files
		// need an explicit ./ prefix.
		"entire-run": installFromIndex,
	} {
		src, err := parseInstallSource(arg)
		if err != nil {
			t.Errorf("parseInstallSource(%q) = %v, want no error", arg, err)
			continue
		}
		if src.Kind != want {
			t.Errorf("parseInstallSource(%q).Kind = %d, want %d", arg, src.Kind, want)
			continue
		}
		if src.Ref != arg {
			t.Errorf("parseInstallSource(%q).Ref = %q, want the argument unchanged", arg, src.Ref)
		}
	}

	// Parsing validates as well as classifies, so a malformed source fails at
	// the boundary with a message about itself rather than being handed to the
	// catalog or the git CLI.
	//
	// Two shapes deliberately are NOT rejected here, because neither can reach
	// git as a repository URL: "ext::sh -c whoami" has no separator and no
	// scheme, so it is an index name that simply misses the catalog, and
	// anything containing a separator is a path that fails at stat.
	// validatePluginName permits spaces and colons in names — pre-existing
	// looseness, and harmless on those two routes.
	for arg, want := range map[string]string{
		"agent-evil": "reserved",
		"--upload-pack=touch /tmp/x; git-upload-pack": "must not start with '-'",
		"http://forge.internal/entire-x":              "must use one of",
		"git://forge.internal/entire-x":               "must use one of",
		"-x":                                          "must not start with '-'",
	} {
		if _, err := parseInstallSource(arg); err == nil {
			t.Errorf("parseInstallSource(%q) = nil error, want a rejection", arg)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("parseInstallSource(%q) = %v, want it to mention %q", arg, err, want)
		}
	}
}

func TestSyncPluginIndex_RecoversFromPartialClone(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	withIndexCache(t)
	url, _ := newIndexRepo(t, `{"version":1,"plugins":[{"name":"run","repo_url":"https://x.example/entire-run"}]}`)
	// Simulate an interrupted first clone: cache dir exists, is non-empty,
	// but has no .git. git clone refuses non-empty targets, so sync must
	// sweep the partial dir instead of staying wedged.
	dir, err := pluginIndexCacheDir(url)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "leftover"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	idx, err := SyncPluginIndex(context.Background(), url, false)
	if err != nil {
		t.Fatalf("SyncPluginIndex after partial clone: %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Errorf("plugins = %d, want 1", len(idx.Plugins))
	}
}

// The per-URL cache dir is shared by every concurrent `entire plugin`
// invocation, and syncing it is destructive (RemoveAll + clone, or
// fetch + reset --hard). Concurrent syncs of the same index must all return a
// usable catalog rather than racing on partial state — including the cold-start
// case, where every caller wants to create the same clone at once.
func TestSyncPluginIndex_ConcurrentSyncsAreSerialized(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	withIndexCache(t)
	url, _ := newIndexRepo(t, `{"version":1,"plugins":[{"name":"run","repo_url":"https://x.example/entire-run"}]}`)

	const workers = 6
	errs := make(chan error, workers)
	var start sync.WaitGroup
	start.Add(1)
	for range workers {
		go func() {
			start.Wait() // maximize overlap on the cold clone
			idx, err := SyncPluginIndex(context.Background(), url, true)
			switch {
			case err != nil:
				errs <- err
			case idx.Find("run") == nil:
				errs <- errors.New("catalog loaded without the expected entry")
			default:
				errs <- nil
			}
		}()
	}
	start.Done()
	for range workers {
		if err := <-errs; err != nil {
			t.Errorf("concurrent SyncPluginIndex: %v", err)
		}
	}
}

// browse is a TTY-only convenience, so it must say so rather than half-working
// — the repo's agent-safe-fallback rule requires the same workflow to be
// reachable non-interactively, which 'search' + 'install <name>' provides.
func TestPluginBrowse_RequiresTerminal(t *testing.T) { //nolint:paralleltest // mutates env
	withIndexCache(t)
	cmd := newPluginBrowseCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("browse succeeded without a terminal")
	}
	if !strings.Contains(err.Error(), "needs a terminal") {
		t.Errorf("err = %v, want it to say a terminal is needed", err)
	}
	// And it must point at the non-interactive equivalent.
	if !strings.Contains(err.Error(), "plugin search") {
		t.Errorf("err = %v, want it to name the non-interactive alternative", err)
	}
}

// index.json is remote and attacker-influenced, and its name/description/repo_url
// are printed straight to the terminal by search, info, and the browse picker.
// An escape sequence in any of them could repaint the row — forging the
// "[official]" marker, or the repository name in the confirmation whose whole
// job is to say where the binary comes from.
//
// repo_url is the one that used to get through: url.Parse rejects control
// characters, but git's scp-like form never reaches url.Parse — RedactURL
// returns it verbatim — so it was the single shape that could carry an escape
// into that prompt.
func TestSyncPluginIndex_DropsEntriesCarryingTerminalEscapes(t *testing.T) { //nolint:paralleltest // mutates env via cache isolation
	withIndexCache(t)
	// JSON \u escapes, so these decode to a real ESC and a real
	// RIGHT-TO-LEFT OVERRIDE rather than the text spelling them.
	const esc = "\\u001b[2K"
	const bidi = "\\u202e"
	url, _ := newIndexRepo(t, `{
  "version": 1,
  "plugins": [
    {"name": "good", "repo_url": "https://example.com/entire-good", "description": "fine"},
    {"name": "esc`+esc+`name", "repo_url": "https://example.com/entire-a", "description": "x"},
    {"name": "escdesc", "repo_url": "https://example.com/entire-b", "description": "y`+esc+`[official]"},
    {"name": "escurl", "repo_url": "git@example.com:`+esc+`entire-c", "description": "z"},
    {"name": "bidi", "repo_url": "https://example.com/entire-d", "description": "`+bidi+`gro.live"}
  ]
}`)
	idx, err := SyncPluginIndex(context.Background(), url, false)
	if err != nil {
		t.Fatalf("SyncPluginIndex: %v", err)
	}
	if len(idx.Plugins) != 1 || idx.Plugins[0].Name != "good" {
		t.Fatalf("plugins = %+v, want only the clean entry", idx.Plugins)
	}
	// Nothing a terminal would act on survived into what gets printed.
	for _, e := range idx.Plugins {
		for _, field := range []string{e.Name, e.Description, e.RepoURL} {
			if hasTerminalControlChars(field) {
				t.Errorf("field %q reached a render path with control characters", field)
			}
		}
	}
}
