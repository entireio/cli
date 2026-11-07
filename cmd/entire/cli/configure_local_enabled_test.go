package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// A repository disabled in settings.json stays disabled when a local setting
// changes: the local file only gains "enabled" when it already had it.
func TestConfigureLocalKeepsProjectDisabled(t *testing.T) {
	for name, tc := range map[string]struct {
		local     string // existing settings.local.json; "" for none
		args      []string
		wantLocal string // "enabled" in the local file afterwards; "" for absent
	}{
		"telemetry, no local file":          {args: []string{"--local", "--telemetry=false"}},
		"summary timeout, no local file":    {args: []string{"--local", "--summarize-timeout-seconds", "600"}},
		"telemetry, local file without key": {local: `{"log_level":"debug"}`, args: []string{"--local", "--telemetry=false"}},
		"telemetry, local file disabled":    {local: `{"enabled":false}`, args: []string{"--local", "--telemetry=false"}, wantLocal: "false"},
	} {
		t.Run(name, func(t *testing.T) {
			setupTestRepo(t)
			writeSettings(t, `{"enabled": false}`)
			if tc.local != "" {
				require.NoError(t, os.WriteFile(EntireSettingsLocalFile, []byte(tc.local), 0o644))
			}

			cmd := newSetupCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)
			require.NoError(t, cmd.Execute())

			data, err := os.ReadFile(filepath.Clean(EntireSettingsLocalFile))
			require.NoError(t, err)
			var raw map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &raw))
			require.Equal(t, tc.wantLocal, string(raw["enabled"]), "local file: %s", data)

			merged, err := settings.Load(context.Background())
			require.NoError(t, err)
			require.False(t, merged.Enabled, "the project file's disable still applies")
		})
	}
}
