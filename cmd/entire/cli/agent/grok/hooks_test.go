package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// hookRepo makes a temp dir the working directory so hooksPath resolves there.
// It cannot run in parallel: paths.WorktreeRoot falls back to the process CWD.
func hookRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

func installedPath(dir string) string { return filepath.Join(dir, hooksFileRelPath) }

func TestInstallHooks_WritesOwnedFile(t *testing.T) {
	dir := hookRepo(t)
	g := &GrokAgent{}
	ctx := context.Background()

	n, err := g.InstallHooks(ctx, false)
	if err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if n != 1 {
		t.Errorf("installed = %d, want 1", n)
	}

	data, err := os.ReadFile(installedPath(dir))
	if err != nil {
		t.Fatalf("read installed hooks: %v", err)
	}

	var file grokHooksFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("installed hooks are not valid JSON: %v", err)
	}

	// Every verb we advertise must appear under its Grok event key, or the
	// `entire hooks grok <verb>` subcommand exists with nothing invoking it.
	for _, verb := range g.HookNames() {
		event, ok := grokEventForHook[verb]
		if !ok {
			t.Errorf("verb %q has no Grok event mapping", verb)
			continue
		}
		groups, ok := file.Hooks[event]
		if !ok || len(groups) == 0 || len(groups[0].Hooks) == 0 {
			t.Errorf("event %q missing from installed config", event)
			continue
		}
		cmd := groups[0].Hooks[0].Command
		if !strings.Contains(cmd, "entire hooks grok "+verb) {
			t.Errorf("event %q command = %q, want it to invoke verb %q", event, cmd, verb)
		}
	}
}

// TestInstallHooks_CommandsNameTheEntireBinary guards the rule that a hook must
// never run something out of the working tree: the command is executed on every
// agent turn, so a repo-relative path would let any checked-out branch run code.
func TestInstallHooks_CommandsNameTheEntireBinary(t *testing.T) {
	dir := hookRepo(t)
	g := &GrokAgent{}

	if _, err := g.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	data, err := os.ReadFile(installedPath(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var file grokHooksFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("parse: %v", err)
	}
	for event, groups := range file.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if !agent.IsManagedHookCommand(h.Command) {
					t.Errorf("event %q: %q is not recognised as an Entire-managed command", event, h.Command)
				}
				if strings.Contains(h.Command, dir) {
					t.Errorf("event %q: command references the working tree: %q", event, h.Command)
				}
			}
		}
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	hookRepo(t)
	g := &GrokAgent{}
	ctx := context.Background()

	if _, err := g.InstallHooks(ctx, false); err != nil {
		t.Fatalf("first install: %v", err)
	}
	n, err := g.InstallHooks(ctx, false)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if n != 0 {
		t.Errorf("second install wrote %d, want 0 (already current)", n)
	}
}

func TestInstallHooks_RewritesStaleEntireConfig(t *testing.T) {
	dir := hookRepo(t)
	g := &GrokAgent{}
	ctx := context.Background()

	// A config an older CLI could have written: ours by marker, but stale.
	stale := `{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"entire hooks grok stop"}]}]}}`
	if err := os.MkdirAll(filepath.Dir(installedPath(dir)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(installedPath(dir), []byte(stale), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	if got := g.CheckHookConfig(ctx); got != agent.HooksOutdated {
		t.Errorf("CheckHookConfig = %v, want HooksOutdated", got)
	}
	if _, err := g.InstallHooks(ctx, false); err != nil {
		t.Fatalf("InstallHooks over stale config: %v", err)
	}
	if got := g.CheckHookConfig(ctx); got != agent.HooksCurrent {
		t.Errorf("after reinstall CheckHookConfig = %v, want HooksCurrent", got)
	}
}

// TestInstallHooks_RefusesForeignFile protects a user-authored entire.json that
// happens to sit at our path.
func TestInstallHooks_RefusesForeignFile(t *testing.T) {
	dir := hookRepo(t)
	g := &GrokAgent{}
	ctx := context.Background()

	foreign := `{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"make lint"}]}]}}`
	if err := os.MkdirAll(filepath.Dir(installedPath(dir)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(installedPath(dir), []byte(foreign), 0o644); err != nil {
		t.Fatalf("write foreign: %v", err)
	}

	if _, err := g.InstallHooks(ctx, false); err == nil {
		t.Error("expected an error installing over a foreign file")
	}
	after, err := os.ReadFile(installedPath(dir))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) != foreign {
		t.Error("foreign file was modified")
	}

	// --force is the documented escape hatch.
	if _, err := g.InstallHooks(ctx, true); err != nil {
		t.Fatalf("forced install: %v", err)
	}
	if !hooksInstalled(ctx, t, g) {
		t.Error("hooks not installed after --force")
	}
}

// hooksInstalled wraps AreHooksInstalled, failing the test on an error so the
// assertions below stay about the bool.
func hooksInstalled(ctx context.Context, t *testing.T, g *GrokAgent) bool {
	t.Helper()
	ok, err := g.AreHooksInstalled(ctx)
	if err != nil {
		t.Fatalf("AreHooksInstalled: %v", err)
	}
	return ok
}

func TestAreHooksInstalledAndUninstall(t *testing.T) {
	dir := hookRepo(t)
	g := &GrokAgent{}
	ctx := context.Background()

	if hooksInstalled(ctx, t, g) {
		t.Error("hooks reported installed in a fresh repo")
	}
	if got := g.CheckHookConfig(ctx); got != agent.HooksAbsent {
		t.Errorf("CheckHookConfig = %v, want HooksAbsent", got)
	}

	if _, err := g.InstallHooks(ctx, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if !hooksInstalled(ctx, t, g) {
		t.Error("hooks not detected after install")
	}
	if got := g.CheckHookConfig(ctx); got != agent.HooksCurrent {
		t.Errorf("CheckHookConfig = %v, want HooksCurrent", got)
	}

	if err := g.UninstallHooks(ctx); err != nil {
		t.Fatalf("UninstallHooks: %v", err)
	}
	if hooksInstalled(ctx, t, g) {
		t.Error("hooks still detected after uninstall")
	}
	if _, err := os.Stat(installedPath(dir)); !os.IsNotExist(err) {
		t.Error("hooks file still present after uninstall")
	}
}

// TestUninstallHooks_LeavesForeignFile is the counterpart to the install
// refusal: uninstall must not delete a file we never owned.
func TestUninstallHooks_LeavesForeignFile(t *testing.T) {
	dir := hookRepo(t)
	g := &GrokAgent{}

	foreign := `{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"make lint"}]}]}}`
	if err := os.MkdirAll(filepath.Dir(installedPath(dir)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(installedPath(dir), []byte(foreign), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := g.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks: %v", err)
	}
	if _, err := os.Stat(installedPath(dir)); err != nil {
		t.Error("foreign file was removed")
	}
}

func TestUninstallHooks_NoConfigIsNotAnError(t *testing.T) {
	hookRepo(t)
	g := &GrokAgent{}
	if err := g.UninstallHooks(context.Background()); err != nil {
		t.Errorf("UninstallHooks on a fresh repo: %v", err)
	}
}

func TestHookNames_MatchEventMapping(t *testing.T) {
	t.Parallel()

	g := &GrokAgent{}
	names := g.HookNames()
	if len(names) != len(grokEventForHook) {
		t.Errorf("HookNames has %d entries, grokEventForHook has %d", len(names), len(grokEventForHook))
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate hook verb %q", n)
		}
		seen[n] = true
		if _, ok := grokEventForHook[n]; !ok {
			t.Errorf("verb %q has no Grok event mapping", n)
		}
	}
}

// TestRenderHooks_IsDeterministic matters because CheckHookConfig compares the
// installed file byte-for-byte against a fresh render; map iteration order
// leaking into the output would report permanent phantom drift.
func TestRenderHooks_IsDeterministic(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	first, err := renderHooks(ctx)
	if err != nil {
		t.Fatalf("renderHooks: %v", err)
	}
	for range 10 {
		next, err := renderHooks(ctx)
		if err != nil {
			t.Fatalf("renderHooks: %v", err)
		}
		if next != first {
			t.Fatal("renderHooks output varies between calls")
		}
	}
}

// TestInstallHooks_TimeoutsMatchTheirTier guards a silent
// checkpoint-dropping regression.
//
// Stop and SubagentStop are blocking gates that Grok gives a 600s timeout;
// every other event defaults to 5s. Writing an explicit short timeout on the
// gates overrides that, and because Grok fails open on timeout the turn's
// checkpoint would simply vanish — no error, no log. Turn-end is exactly where
// the expensive work (redaction, condensation, checkpoint write) happens, so
// the gates must carry no timeout at all.
func TestInstallHooks_TimeoutsMatchTheirTier(t *testing.T) {
	dir := hookRepo(t)
	g := &GrokAgent{}

	if _, err := g.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	data, err := os.ReadFile(installedPath(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var file grokHooksFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("parse: %v", err)
	}

	for verb, event := range grokEventForHook {
		groups := file.Hooks[event]
		if len(groups) == 0 || len(groups[0].Hooks) == 0 {
			t.Errorf("%s: no hook installed", event)
			continue
		}
		if got, want := groups[0].Hooks[0].Timeout, hookTimeout(verb); got != want {
			t.Errorf("%s: timeout = %d, want %d", event, got, want)
		}
	}

	// The gate events must not emit the key at all, not merely emit zero.
	if bytes.Contains(data, []byte(`"timeout": 0`)) {
		t.Error(`rendered config contains "timeout": 0; the field should be omitted entirely`)
	}
}

// TestHookTimeout_TurnEndingHooksGetTheLongBudget covers all three tiers.
//
// The subtle one is StopCancelled/StopFailure: they map to TurnEnd and run the
// same redaction/condensation/checkpoint path as Stop, but Grok treats them as
// ordinary events with a 5s default rather than 600s gates. Omitting their
// timeout would leave them at 5s — worse than doing nothing — so they need the
// long budget written explicitly.
func TestHookTimeout_TurnEndingHooksGetTheLongBudget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		verb string
		want int
	}{
		{HookNameStop, 0},                              // Grok's own 600s gate
		{HookNameSubagentStop, 0},                      // Grok's own 600s gate
		{HookNameStopCancelled, turnEndTimeoutSeconds}, // 5s default; must be raised
		{HookNameStopFailure, turnEndTimeoutSeconds},   // 5s default; must be raised
		{HookNameSessionStart, hookTimeoutSeconds},     // observational
		{HookNameUserPromptSubmit, hookTimeoutSeconds}, // observational
		{HookNamePostToolUse, hookTimeoutSeconds},      // observational
	}
	for _, tt := range tests {
		if got := hookTimeout(tt.verb); got != tt.want {
			t.Errorf("hookTimeout(%s) = %d, want %d", tt.verb, got, tt.want)
		}
	}
}

// TestInstallHooks_NoTurnEndingHookIsLeftOnGroksShortDefault is the property
// behind the table above: nothing that ends a turn may run on Grok's 5s
// default, because Grok fails open and the checkpoint disappears silently.
func TestInstallHooks_NoTurnEndingHookIsLeftOnGroksShortDefault(t *testing.T) {
	dir := hookRepo(t)
	g := &GrokAgent{}
	if _, err := g.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	data, err := os.ReadFile(installedPath(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var file grokHooksFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Every verb whose ParseHookEvent yields TurnEnd.
	for _, verb := range []string{HookNameStop, HookNameStopCancelled, HookNameStopFailure} {
		groups := file.Hooks[grokEventForHook[verb]]
		if len(groups) == 0 || len(groups[0].Hooks) == 0 {
			t.Fatalf("%s: not installed", verb)
		}
		got := groups[0].Hooks[0].Timeout
		if got != 0 && got < turnEndTimeoutSeconds {
			t.Errorf("%s: timeout = %d, too short for turn-end work (want 0 or >= %d)",
				verb, got, turnEndTimeoutSeconds)
		}
	}
}
