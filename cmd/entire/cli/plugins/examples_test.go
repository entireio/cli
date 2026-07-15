package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// examplesDir resolves the repo-root examples/plugins dir relative to this
// package's working directory (cmd/entire/cli/plugins) at test time.
func examplesDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "..", "examples", "plugins")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("examples dir not found: %v", err)
	}
	return dir
}

func TestExamplePlugins_LoadCleanly(t *testing.T) {
	base := examplesDir(t)
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())

	cases := []struct {
		name     string
		grant    settings.PluginSettings
		wantHook string
		wantCmd  string
	}{
		{
			name:     "checkpoint-notify",
			grant:    settings.PluginSettings{Enabled: true, Capabilities: []string{settings.PluginCapabilityExec}},
			wantHook: HookCheckpointSaved,
			wantCmd:  "notify-stats",
		},
		{
			name:    "models-updater",
			grant:   settings.PluginSettings{Enabled: true, Capabilities: []string{settings.PluginCapabilityHTTP, settings.PluginCapabilityFS}},
			wantCmd: "models-update",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(base, tc.name)
			p, err := LoadPlugin(context.Background(), dir, SourceUser, "", tc.grant)
			if err != nil {
				t.Fatalf("LoadPlugin(%s) error = %v", tc.name, err)
			}
			defer p.Close()

			if tc.wantHook != "" && !p.HasHook(tc.wantHook) {
				t.Errorf("%s did not register hook %q", tc.name, tc.wantHook)
			}
			if tc.wantCmd != "" {
				if _, ok := p.commands[tc.wantCmd]; !ok {
					t.Errorf("%s did not register command %q", tc.name, tc.wantCmd)
				}
			}
		})
	}
}
