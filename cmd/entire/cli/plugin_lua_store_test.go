package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestLooksLikeGitURL(t *testing.T) {
	t.Parallel()
	git := []string{
		"https://github.com/acme/entire-notify.git",
		"http://example.com/x.git",
		"git@github.com:acme/repo.git",
		"ssh://git@host/repo",
		"git://host/repo",
	}
	notGit := []string{
		"./local/dir",
		"/abs/path",
		"entire-pgr",
		"my-plugin",
	}
	for _, u := range git {
		if !looksLikeGitURL(u) {
			t.Errorf("looksLikeGitURL(%q) = false, want true", u)
		}
	}
	for _, u := range notGit {
		if looksLikeGitURL(u) {
			t.Errorf("looksLikeGitURL(%q) = true, want false", u)
		}
	}
}

func writeLocalPluginDir(t *testing.T, name, mainLua string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src-"+name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := `{"name":"` + name + `","version":"1.0.0"}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.lua"), []byte(mainLua), 0o600); err != nil {
		t.Fatalf("write main.lua: %v", err)
	}
	return dir
}

func TestInstallLuaPluginFromPath_ListAndRemove(t *testing.T) {
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())
	src := writeLocalPluginDir(t, "notify", `-- noop`)

	p, err := InstallLuaPluginFromPath(context.Background(), src, false)
	if err != nil {
		t.Fatalf("InstallLuaPluginFromPath() error = %v", err)
	}
	if p.Name != "notify" || p.Type != "local" {
		t.Errorf("installed = %+v, want name=notify type=local", p)
	}

	all, err := ListInstalledLuaPlugins()
	if err != nil {
		t.Fatalf("ListInstalledLuaPlugins() error = %v", err)
	}
	if len(all) != 1 || all[0].Name != "notify" {
		t.Fatalf("list = %+v, want [notify]", all)
	}

	// Re-install without --force must fail; with force must succeed.
	if _, err := InstallLuaPluginFromPath(context.Background(), src, false); err == nil {
		t.Error("expected re-install without --force to fail")
	}
	if _, err := InstallLuaPluginFromPath(context.Background(), src, true); err != nil {
		t.Errorf("force re-install failed: %v", err)
	}

	if err := RemoveLuaPlugin("notify"); err != nil {
		t.Fatalf("RemoveLuaPlugin() error = %v", err)
	}
	found, ferr := FindInstalledLuaPlugin("notify")
	if ferr != nil {
		t.Fatalf("FindInstalledLuaPlugin() error = %v", ferr)
	}
	if found != nil {
		t.Error("expected plugin removed")
	}
}

func TestInstallLuaPluginFromGit_AndUpdate(t *testing.T) {
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())
	ctx := context.Background()

	// Build a local git repo to act as the remote source.
	srcRepo := t.TempDir()
	testutil.InitRepo(t, srcRepo)
	testutil.WriteFile(t, srcRepo, "plugin.json", `{"name":"gitplug","version":"1.0.0"}`)
	testutil.WriteFile(t, srcRepo, "main.lua", `-- v1`)
	testutil.GitAdd(t, srcRepo, "plugin.json", "main.lua")
	testutil.GitCommit(t, srcRepo, "initial")

	p, err := InstallLuaPluginFromGit(ctx, srcRepo, "", false)
	if err != nil {
		t.Fatalf("InstallLuaPluginFromGit() error = %v", err)
	}
	if p.Name != "gitplug" || p.Type != "git" {
		t.Errorf("installed = %+v, want name=gitplug type=git", p)
	}
	if got := readInstalled(t, p.Dir, "main.lua"); got != "-- v1" {
		t.Errorf("main.lua = %q, want -- v1", got)
	}

	// Advance the source, then update the installed plugin.
	testutil.WriteFile(t, srcRepo, "main.lua", `-- v2`)
	testutil.GitAdd(t, srcRepo, "main.lua")
	testutil.GitCommit(t, srcRepo, "update")

	if err := UpdateLuaPlugin(ctx, "gitplug"); err != nil {
		t.Fatalf("UpdateLuaPlugin() error = %v", err)
	}
	if got := readInstalled(t, p.Dir, "main.lua"); got != "-- v2" {
		t.Errorf("after update main.lua = %q, want -- v2", got)
	}
}

func readInstalled(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}
