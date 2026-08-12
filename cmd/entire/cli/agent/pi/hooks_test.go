package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Note: t.Parallel is incompatible with t.Chdir.

func TestInstallHooks_FreshInstall(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	count, err := (&PiAgent{}).InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}

	path := filepath.Join(dir, ".pi", "extensions", "entire", "index.ts")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("extension not written: %v", err)
	}
	body := string(data)

	if !strings.Contains(body, `const ENTIRE_CMD = 'entire'`) {
		t.Error("production ENTIRE_CMD missing")
	}
	if !strings.Contains(body, "hooks pi ") {
		t.Error("missing call to `entire hooks pi`")
	}
	if !strings.Contains(body, entireMarker) {
		t.Error("entireMarker missing")
	}
	if strings.Contains(body, "go run") {
		t.Error("production extension should not contain 'go run'")
	}
	// The nesting guard keeps a subagent's nested `pi` process from forwarding its
	// lifecycle as the user's session.
	if !strings.Contains(body, "process.env."+piNestedEnvVar) {
		t.Error("nested-invocation guard missing from installed extension")
	}
}

func TestInstallHooks_LocalDev(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if _, err := (&PiAgent{}).InstallHooks(context.Background(), true, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".pi", "extensions", "entire", "index.ts"))
	if err != nil {
		t.Fatal(err)
	}
	// Assert the exact, well-formed line. The launcher value carries its own
	// shell quotes, so the template must wrap the placeholder in single quotes;
	// wrapping in double quotes yields the malformed `""$(...)"/..."` (a broken
	// JS string literal). A substring check alone would pass on that broken
	// output, so pin the whole line.
	if !strings.Contains(string(data), `const ENTIRE_CMD = '"$(git rev-parse --show-toplevel)"/scripts/entire-dev'`) {
		t.Errorf("local-dev ENTIRE_CMD malformed; got:\n%s", data)
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	a := &PiAgent{}

	c1, err := a.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != 1 {
		t.Errorf("first install count = %d", c1)
	}
	c2, err := a.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if c2 != 0 {
		t.Errorf("second install (idempotent) count = %d", c2)
	}
}

func TestInstallHooks_RewritesOnModeChange(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	a := &PiAgent{}
	if _, err := a.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	c, err := a.InstallHooks(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if c != 1 {
		t.Errorf("expected rewrite on mode change, got %d", c)
	}
}

func TestUninstallHooks(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	a := &PiAgent{}
	if _, err := a.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if !a.AreHooksInstalled(context.Background()) {
		t.Fatal("AreHooksInstalled should be true after install")
	}
	if err := a.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("UninstallHooks: %v", err)
	}
	if a.AreHooksInstalled(context.Background()) {
		t.Error("AreHooksInstalled should be false after uninstall")
	}
	// Idempotent uninstall.
	if err := a.UninstallHooks(context.Background()); err != nil {
		t.Errorf("second uninstall: %v", err)
	}
}

func TestAreHooksInstalled_RejectsForeignFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, ".pi", "extensions", "entire", "index.ts")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("// user's own extension\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if (&PiAgent{}).AreHooksInstalled(context.Background()) {
		t.Error("should not claim a non-Entire file")
	}
}

func TestInstallHooks_RefusesForeignFileWithoutForce(t *testing.T) {
	// User has their own extension at the same path. Without --force we must
	// not clobber it. With --force we replace it.
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, ".pi", "extensions", "entire", "index.ts")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	userContent := []byte("// user's own extension\nconsole.log('mine');\n")
	if err := os.WriteFile(path, userContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// Without force: should refuse, leave file untouched.
	_, err := (&PiAgent{}).InstallHooks(context.Background(), false, false)
	if err == nil {
		t.Fatal("expected error when foreign file exists and force=false")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(userContent) {
		t.Errorf("foreign file was modified: %q", got)
	}

	// With force: should overwrite.
	if _, err := (&PiAgent{}).InstallHooks(context.Background(), false, true); err != nil {
		t.Fatalf("force install failed: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), entireMarker) {
		t.Error("force install should write Entire-owned file")
	}
}

// TestCheckHookConfig_CommittedExtensionGoesStale is the case this check
// exists for. Repos commonly commit .pi/extensions/entire/index.ts so every
// clone gets checkpointing without each person running `entire agent add pi`.
// The committed copy then goes stale as the template evolves, while
// AreHooksInstalled keeps reporting it installed (the marker is still there)
// and the extension's own fireHook swallows every error — so without a drift
// check the repo reads as healthy while its hooks silently no-op.
func TestCheckHookConfig_CommittedExtensionGoesStale(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ctx := context.Background()
	a := &PiAgent{}

	if _, err := a.InstallHooks(ctx, false, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if got := a.CheckHookConfig(ctx); got != agent.HooksCurrent {
		t.Fatalf("fresh install: CheckHookConfig = %v, want HooksCurrent", got)
	}

	// Simulate the template moving on under a committed extension: keep the
	// marker (so it is still recognisably ours) but change the body.
	path := filepath.Join(dir, ".pi", "extensions", "entire", "index.ts")
	stale := "// " + entireMarker + "\n// an older release wrote this\n"
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	if !a.AreHooksInstalled(ctx) {
		t.Error("AreHooksInstalled = false; a stale-but-marked extension is still installed")
	}
	if got := a.CheckHookConfig(ctx); got != agent.HooksOutdated {
		t.Errorf("stale extension: CheckHookConfig = %v, want HooksOutdated", got)
	}
}

// TestCheckHookConfig_CRLFCheckoutIsNotDrift guards the Windows false
// positive. The extension is generated with LF but is typically committed, so
// a checkout under git's default core.autocrlf=true on Windows hands us CRLF.
// Byte equality never holds there, and the user cannot clear the warning:
// InstallHooks writes LF back and the next checkout re-converts it.
func TestCheckHookConfig_CRLFCheckoutIsNotDrift(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ctx := context.Background()
	a := &PiAgent{}

	if _, err := a.InstallHooks(ctx, false, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	path := filepath.Join(dir, ".pi", "extensions", "entire", "index.ts")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	crlf := strings.ReplaceAll(string(data), "\n", "\r\n")
	if err := os.WriteFile(path, []byte(crlf), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := a.CheckHookConfig(ctx); got != agent.HooksCurrent {
		t.Errorf("CRLF checkout: CheckHookConfig = %v, want HooksCurrent", got)
	}
}

// TestCheckHookConfig_LocalDevIsNotDrift guards the false positive that would
// make this check useless in practice: a developer who enabled with --local-dev
// has a legitimately different file (the entire command is substituted), and
// must not be nagged that their hooks are out of date.
func TestCheckHookConfig_LocalDevIsNotDrift(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ctx := context.Background()
	a := &PiAgent{}

	if _, err := a.InstallHooks(ctx, true, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if got := a.CheckHookConfig(ctx); got != agent.HooksCurrent {
		t.Errorf("local-dev install: CheckHookConfig = %v, want HooksCurrent", got)
	}
}

func TestCheckHookConfig_AbsentAndForeign(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ctx := context.Background()
	a := &PiAgent{}

	if got := a.CheckHookConfig(ctx); got != agent.HooksAbsent {
		t.Errorf("no extension: CheckHookConfig = %v, want HooksAbsent", got)
	}

	// A foreign file at our path is not ours to call stale: InstallHooks
	// refuses to overwrite it, so reporting drift would nag about a file the
	// CLI will not touch.
	path := filepath.Join(dir, ".pi", "extensions", "entire", "index.ts")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("// someone else's extension\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := a.CheckHookConfig(ctx); got != agent.HooksAbsent {
		t.Errorf("foreign file: CheckHookConfig = %v, want HooksAbsent", got)
	}
}
