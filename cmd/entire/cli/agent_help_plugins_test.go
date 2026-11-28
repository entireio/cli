package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// installReleaseTestPlugin installs entire-<name> as a release install does:
// the binary in pkg/<name>/, the bin entry a symlink to it, and a manifest
// recording agentHelp (nil: a manifest written before the field existed).
// Skips on Windows (shell-script plugins).
func installReleaseTestPlugin(t *testing.T, name string, agentHelp *bool) {
	t.Helper()
	pkg, err := PluginPkgDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	release := writePluginBinary(t, pkg, pluginBinaryName(name), filepath.Join(t.TempDir(), "args.txt"), 0)
	binDir, err := EnsurePluginBinDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, filepath.Join(binDir, pluginBinaryPrefix+name)); err != nil {
		t.Fatal(err)
	}
	m := &PluginManifest{Name: name, RepoURL: "https://example.com/entire-" + name, Tag: "v1.0.0", AgentHelp: agentHelp}
	if err := SavePluginManifest(m); err != nil {
		t.Fatal(err)
	}
}

// The bare `entire agent-help` names, before the commands, the installed
// plugins whose release declared agent_help, and nothing when there are none.
func TestAgentHelpGuidedPluginsBlock(t *testing.T) { //nolint:paralleltest // mutates ENTIRE_PLUGIN_DIR via t.Setenv
	type plugin struct {
		name      string
		agentHelp *bool
	}
	for _, tc := range []struct {
		name    string
		plugins []plugin
		// localOver is a plugin a local install then replaces: the release's
		// manifest stays, but the bin entry is no longer the binary it
		// describes, and the local build may not answer agent-help.
		localOver string
		listed    []string
	}{
		{name: "none"},
		{name: "declared", plugins: []plugin{{"stream", new(true)}}, listed: []string{"stream"}},
		{name: "not declared", plugins: []plugin{{"stream", new(false)}}},
		{name: "field absent", plugins: []plugin{{"stream", nil}}},
		{name: "local install over release", plugins: []plugin{{"stream", new(true)}}, localOver: "stream"},
		{name: "built-in shadows", plugins: []plugin{{"status", new(true)}, {"stream", new(true)}}, listed: []string{"stream"}},
		// validatePluginName accepts these, but a shell would split them.
		{name: "shell-unsafe names", plugins: []plugin{{"foo;bar", new(true)}, {"foo bar", new(true)}, {"ok", new(true)}}, listed: []string{"ok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withPluginDir(t)
			for _, p := range tc.plugins {
				installReleaseTestPlugin(t, p.name, p.agentHelp)
			}
			if tc.localOver != "" {
				src := t.TempDir()
				bin := writePluginBinary(t, src, pluginBinaryPrefix+tc.localOver, filepath.Join(src, "args.txt"), 0)
				if _, err := InstallPluginFromPath(t.Context(), InstallPluginOptions{SourcePath: bin, Force: true}); err != nil {
					t.Fatal(err)
				}
			}

			out := renderAgentHelpTop(NewRootCmd(), agentHelpTestRepo, true)

			if len(tc.listed) == 0 {
				if strings.Contains(out, agentHelpGuidedPluginsHeading) {
					t.Errorf("guidance block printed:\n%s", out)
				}
				return
			}
			block := agentHelpGuidedPluginsHeading + "\n  entire agent-help " + strings.Join(tc.listed, "\n  entire agent-help ") + "\n"
			if i := strings.Index(out, block); i < 0 || i > strings.Index(out, "When to use entire") {
				t.Errorf("want this block before the commands:\n%s\ngot:\n%s", block, out)
			}
			for _, p := range tc.plugins {
				if !slices.Contains(tc.listed, p.name) && strings.Contains(out, "entire agent-help "+p.name) {
					t.Errorf("%q is named:\n%s", p.name, out)
				}
			}
		})
	}
}

// agent_help is read leniently: a value that isn't a boolean means no, and
// doesn't fail the whole entire-plugin.yml (which would fail the install).
func TestParsePluginMetadataAgentHelp(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"agent_help: true\n":  true,
		"agent_help: false\n": false,
		"agent_help: yes\n":   false,
		"agent_help: [1]\n":   false,
		"name: demo\n":        false,
	} {
		meta, err := ParsePluginMetadata([]byte(in))
		if err != nil {
			t.Errorf("ParsePluginMetadata(%q): %v", in, err)
			continue
		}
		if bool(meta.AgentHelp) != want {
			t.Errorf("ParsePluginMetadata(%q).AgentHelp = %v, want %v", in, meta.AgentHelp, want)
		}
	}
}
