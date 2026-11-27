package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installReleaseTestPlugin installs entire-<name> as a release install does:
// the binary in pkg/<name>/, the bin entry a symlink to it, and a manifest.
// Skips on Windows (shell-script plugins).
func installReleaseTestPlugin(t *testing.T, name string, agentHelp bool) {
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

func agentHelpTopText(t *testing.T) string {
	t.Helper()
	return renderAgentHelpTop(NewRootCmd(), agentHelpTestRepo, true)
}

func TestAgentHelpNamesPluginsWithGuidance(t *testing.T) { //nolint:paralleltest // mutates ENTIRE_PLUGIN_DIR via t.Setenv
	withPluginDir(t)
	installReleaseTestPlugin(t, "stream", true)
	installReleaseTestPlugin(t, "quiet", false) // doesn't declare it
	installReleaseTestPlugin(t, "status", true) // a built-in answers status

	out := agentHelpTopText(t)

	block := agentHelpGuidedPluginsHeading + "\n  entire agent-help stream\n"
	if !strings.Contains(out, block) {
		t.Fatalf("no guidance block for stream:\n%s", out)
	}
	if strings.Contains(out, "entire agent-help quiet") || strings.Contains(out, "entire agent-help status") {
		t.Errorf("names a plugin that doesn't declare guidance, or a built-in:\n%s", out)
	}
	if strings.Index(out, block) > strings.Index(out, "When to use entire") {
		t.Error("the block should come before the commands")
	}
}

func TestAgentHelpNoGuidanceBlock(t *testing.T) { //nolint:paralleltest // mutates ENTIRE_PLUGIN_DIR via t.Setenv
	cases := map[string]func(t *testing.T){
		"no plugins": func(*testing.T) {},
		"not declared": func(t *testing.T) {
			installReleaseTestPlugin(t, "stream", false)
		},
		// A manifest written before the field existed reads as not declared.
		"manifest without the field": func(t *testing.T) {
			installReleaseTestPlugin(t, "stream", false)
			dir, err := PluginPkgDir("stream")
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(dir, pluginManifestFileName))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "agent_help") {
				t.Fatalf("manifest has the field:\n%s", data)
			}
		},
		// A local install over a release keeps the release's manifest, but
		// the bin entry is no longer the release binary, which the manifest
		// describes: the local build may not answer agent-help.
		"local install over a release": func(t *testing.T) {
			installReleaseTestPlugin(t, "stream", true)
			src := t.TempDir()
			bin := writePluginBinary(t, src, pluginBinaryPrefix+"stream", filepath.Join(src, "args.txt"), 0)
			if _, err := InstallPluginFromPath(t.Context(), InstallPluginOptions{SourcePath: bin, Force: true}); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			withPluginDir(t)
			setup(t)
			if out := agentHelpTopText(t); strings.Contains(out, agentHelpGuidedPluginsHeading) {
				t.Errorf("guidance block printed:\n%s", out)
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
