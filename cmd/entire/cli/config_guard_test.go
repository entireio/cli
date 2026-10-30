package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/entireio/cli/internal/entireclient/userdirs"

	"github.com/spf13/cobra"
)

// configCheckEntry is one ledger row.
type configCheckEntry struct {
	mode string
	// subtree covers every descendant too.
	subtree bool
	reason  string
}

// configCheckLedger pins every command whose effective config.json check
// mode is not the default, and why. TestConfigCheckLedger fails on a
// command missing from it, so a child added under an exempt group, or a new
// exemption, needs an entry here.
//
// Deliberately absent: status, disable and the rest. A broken config.json
// should reach the user as early as possible, and those are the commands
// they run.
var configCheckLedger = map[string]configCheckEntry{
	"entire hooks":                      {configCheckSkip, true, "agent and git hooks must never exit non-zero"},
	"entire __send_analytics":           {configCheckSkip, false, "detached; nobody sees its failure"},
	"entire __sweep_sessions":           {configCheckSkip, false, "detached from session-start; nobody sees its failure"},
	"entire __refresh_trail_enablement": {configCheckSkip, false, "detached; fails before the logger starts"},
	"entire curl-bash-post-install":     {configCheckSkip, false, "install.sh runs it under set -e; a re-install must not report failure"},
	"entire completion":                 {configCheckSkip, true, "users eval it from a shell rc; never touches the token store"},
	"entire help":                       {configCheckSkip, false, "prints help; never touches the token store"},
	"entire version":                    {configCheckSkip, false, "prints build information; never touches the token store"},
	"entire mcp":                        {configCheckSkip, false, "agent hosts start it unattended and show only \"server failed\""},
	"entire agent-help":                 {configCheckSkip, false, "agents read it unattended; never touches the token store"},
	"entire doctor":                     {configCheckSkip, false, "diagnostic; reports config.json itself and exits non-zero"},
	"entire doctor trace":               {configCheckSkip, false, "reads local hook traces only"},
	"entire doctor logs":                {configCheckSkip, false, "reads .entire/logs only"},
	"entire doctor bundle":              {configCheckSkip, false, "bundles local diagnostics only"},
	"entire doctor migrate-checkpoints": {configCheckSkip, false, "rewrites local checkpoint refs; pushes go through the checked remote helper"},
	"entire login":                      {configCheckAlways, false, "saves to the store even with ENTIRE_TOKEN set"},
	"entire logout":                     {configCheckAlways, false, "deletes from the store even with ENTIRE_TOKEN set"},
	"entire auth login":                 {configCheckAlways, false, "saves to the store even with ENTIRE_TOKEN set"},
	"entire auth logout":                {configCheckAlways, false, "deletes from the store even with ENTIRE_TOKEN set"},
}

// ledgerEntryFor finds the row covering cmd.
func ledgerEntryFor(cmd *cobra.Command) (configCheckEntry, bool) {
	if e, ok := configCheckLedger[cmd.CommandPath()]; ok {
		return e, true
	}
	for c := cmd.Parent(); c != nil; c = c.Parent() {
		if e, ok := configCheckLedger[c.CommandPath()]; ok && e.subtree {
			return e, true
		}
	}
	return configCheckEntry{}, false
}

// allCommands lists root and every descendant.
func allCommands(root *cobra.Command) []*cobra.Command {
	all := []*cobra.Command{root}
	for i := 0; i < len(all); i++ {
		all = append(all, all[i].Commands()...)
	}
	return all
}

func TestConfigCheckLedger(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	root.InitDefaultHelpCmd()
	seen := map[string]bool{}
	for _, cmd := range allCommands(root) {
		path := cmd.CommandPath()
		seen[path] = true
		mode := configCheckMode(cmd)
		entry, ok := ledgerEntryFor(cmd)
		switch {
		case mode != "" && !ok:
			t.Errorf("%q has config check mode %q but no configCheckLedger entry covers it.\n"+
				"Add an entry saying why, or move it out of the exempt group.", path, mode)
		case ok && entry.mode != mode:
			t.Errorf("%q has config check mode %q, ledger says %q", path, mode, entry.mode)
		}
		// Detached internals must never fail.
		if cmd.Parent() == root && strings.HasPrefix(cmd.Name(), "__") && mode != configCheckSkip {
			t.Errorf("%q is a detached internal command; exempt it from the config check", path)
		}
	}
	for path, entry := range configCheckLedger {
		if !seen[path] {
			t.Errorf("configCheckLedger lists %q, but no such command exists", path)
		}
		if strings.TrimSpace(entry.reason) == "" {
			t.Errorf("entry for %q has no reason", path)
		}
	}
}

// writeBadConfig isolates a broken config.json.
func writeBadConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"tokenstore": "file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	unsetEnv(t, auth.EnvTokenVar)
	// Keep regressions off the shared store.
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))
}

// executeRoot runs the real root command.
func executeRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRootCmd()
	root.SetIn(strings.NewReader(""))
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

// Every command's guard matches its mode.
func TestCheckConfigBeforeRun_EveryCommand(t *testing.T) {
	writeBadConfig(t)

	root := NewRootCmd()
	root.InitDefaultHelpCmd()
	commands := allCommands(root)
	check := func(envToken bool) {
		for _, cmd := range commands {
			mode := configCheckMode(cmd)
			wantErr := mode == configCheckAlways || (mode == "" && !envToken)
			err := checkConfigBeforeRun(cmd)
			if gotErr := errors.Is(err, tokenstore.ErrBadConfig); gotErr != wantErr {
				t.Errorf("%q (ENTIRE_TOKEN set: %v): err = %v, want ErrBadConfig %v", cmd.CommandPath(), envToken, err, wantErr)
			}
		}
	}
	check(false)
	t.Setenv(auth.EnvTokenVar, "x")
	check(true)
}

// Root's pre-run lets every skipped command through.
func TestRoot_PreRunSkipsExemptCommands(t *testing.T) {
	writeBadConfig(t)
	t.Chdir(t.TempDir())

	root := NewRootCmd()
	root.InitDefaultHelpCmd()
	for _, cmd := range allCommands(root) {
		if configCheckMode(cmd) != configCheckSkip {
			continue
		}
		cmd.SetContext(t.Context())
		if err := root.PersistentPreRunE(cmd, nil); err != nil {
			t.Errorf("%q: pre-run err = %v, want nil", cmd.CommandPath(), err)
		}
	}
}

// A bad config.json fails before RunE.
func TestRoot_BadConfigFailsEarly(t *testing.T) {
	writeBadConfig(t)
	t.Chdir(t.TempDir())

	// status is deliberate: fail as early as possible.
	for _, args := range [][]string{{"auth", "status"}, {"login", "--device"}, {"status"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, err := executeRoot(t, args...)
			if !errors.Is(err, tokenstore.ErrBadConfig) {
				t.Fatalf("err = %v, want ErrBadConfig", err)
			}
			if out != "" {
				t.Errorf("command ran and printed %q", out)
			}
		})
	}
}

// login/logout check despite ENTIRE_TOKEN.
func TestRoot_EnvTokenStillChecksLogin(t *testing.T) {
	writeBadConfig(t)
	t.Setenv(auth.EnvTokenVar, "x")
	t.Chdir(t.TempDir())

	for _, args := range [][]string{{"login", "--device"}, {"auth", "login", "--device"}, {"logout"}, {"auth", "logout"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, err := executeRoot(t, args...)
			if !errors.Is(err, tokenstore.ErrBadConfig) {
				t.Fatalf("err = %v, want ErrBadConfig", err)
			}
			if out != "" {
				t.Errorf("command ran and printed %q", out)
			}
		})
	}
}

// Exempt commands run despite a bad config.json.
func TestRoot_ExemptCommandsRun(t *testing.T) {
	writeBadConfig(t)
	t.Chdir(t.TempDir())

	for _, args := range [][]string{{"completion", "zsh"}, {"hooks", "git", "pre-push", "origin"}, {"help"}, {"version"}, {"agent-help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := executeRoot(t, args...); err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

// Doctor reports a bad config.json.
func TestDoctor_ReportsBadConfig(t *testing.T) {
	writeBadConfig(t)

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err := checkUserConfig(cmd)
	if !errors.Is(err, tokenstore.ErrBadConfig) {
		t.Fatalf("err = %v, want ErrBadConfig", err)
	}
	for _, want := range []string{"Per-user config: INVALID", `unknown key "tokenstore"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor output missing %q:\n%s", want, out.String())
		}
	}
}
