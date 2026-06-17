package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestWarnIfShadowsBuiltin verifies the install-time hint fires for names that
// match a built-in command (the runtime resolver always picks the built-in
// over a managed plugin) and stays silent for plugin-only names.
func TestWarnIfShadowsBuiltin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		install   string
		wantWarn  bool
		wantSubst string
	}{
		{name: "shadows built-in", install: "status", wantWarn: true, wantSubst: "shadows the built-in"},
		{name: "plugin-only name", install: "pgr", wantWarn: false},
		{name: "shadows help (cobra-injected)", install: "help", wantWarn: true, wantSubst: "shadows the built-in"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := &cobra.Command{Use: "entire"}
			root.AddCommand(&cobra.Command{Use: "status"})
			plugin := &cobra.Command{Use: "plugin"}
			install := &cobra.Command{Use: "install"}
			plugin.AddCommand(install)
			root.AddCommand(plugin)

			var stderr bytes.Buffer
			install.SetErr(&stderr)
			install.SetOut(&bytes.Buffer{})

			warnIfShadowsBuiltin(install, tc.install)

			got := stderr.String()
			if tc.wantWarn {
				if !strings.Contains(got, tc.wantSubst) {
					t.Errorf("expected warning containing %q, got %q", tc.wantSubst, got)
				}
			} else if got != "" {
				t.Errorf("expected no warning, got %q", got)
			}
		})
	}
}
