package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// These behavior tests drive the shipped examples/plugins/models-updater plugin
// end to end: entire.http is stubbed with an httptest server (the command takes
// the URL as a positional arg), and the fs writes / kv state / exit codes are
// asserted from disk. This exercises the command (fetch/diff/write/--check),
// local-only preservation, and the session_start staleness nudge.

// modelsUpdaterName is the shipped example plugin under test.
const modelsUpdaterName = "models-updater"

const modelsUpstream = `{
  "sample_spec": {
    "max_tokens": 2048,
    "mode": "chat"
  },
  "gpt-4o": {
    "max_tokens": 4096,
    "input_cost_per_token": 0.0000025,
    "output_cost_per_token": 0.00001,
    "litellm_provider": "openai",
    "mode": "chat"
  },
  "claude-opus-4": {
    "max_tokens": 8192,
    "input_cost_per_token": 0.000015,
    "output_cost_per_token": 0.000075,
    "litellm_provider": "anthropic",
    "mode": "chat"
  }
}`

// modelsCached is an older cache: gpt-4o has different (higher) rates and there
// is one local-only id (entire-internal-model) absent upstream.
const modelsCached = `{
  "gpt-4o": {
    "max_tokens": 4096,
    "input_cost_per_token": 0.000005,
    "output_cost_per_token": 0.000015,
    "litellm_provider": "openai",
    "mode": "chat"
  },
  "entire-internal-model": {
    "max_tokens": 200000,
    "input_cost_per_token": 0.000001,
    "output_cost_per_token": 0.000002,
    "litellm_provider": "entire",
    "mode": "chat"
  }
}`

// The realistic fixtures mirror tricky shapes in the actual LiteLLM file:
// scientific-notation rates, arrays, nested objects, a string value that
// contains a brace, and a decoy field ("input_cost_per_token_above_128k_tokens")
// whose name has "input_cost_per_token" as a prefix and must NOT be mistaken for
// the real rate field.
const modelsRealisticCached = `{
  "gpt-4o": {
    "input_cost_per_token": 0.000005,
    "input_cost_per_token_above_128k_tokens": 0.00001,
    "output_cost_per_token": 0.000015,
    "supported_openai_params": ["temperature", "max_tokens"],
    "mode": "chat"
  }
}`

const modelsRealisticUpstream = `{
  "gpt-4o": {
    "input_cost_per_token": 3e-06,
    "input_cost_per_token_above_128k_tokens": 0.00001,
    "output_cost_per_token": 0.000015,
    "supported_openai_params": ["temperature", "max_tokens"],
    "metadata": { "note": "billed per {token}, commas, and : colons" },
    "mode": "chat"
  }
}`

func TestModelsUpdater_FirstFetchWrites(t *testing.T) {
	root := t.TempDir()
	reg := loadModelsUpdaterReg(t, root)
	url := serveBody(t, modelsUpstream)

	var code int
	out := captureStdout(t, func() {
		code, _ = reg.RunCommand(context.Background(), "models-update", []string{url})
	})

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
	}
	if !strings.Contains(out, "first copy") {
		t.Errorf("expected first-copy note, got:\n%s", out)
	}
	if got := readCache(t, root); got != modelsUpstream {
		t.Errorf("cache file not written verbatim from upstream:\n%s", got)
	}
	// The refresh marker was recorded.
	if kv := readKV(t); kv["last_updated_session"] == "" {
		t.Errorf("expected last_updated_session to be recorded, kv = %v", kv)
	}
}

func TestModelsUpdater_DriftAndPreserveLocalOnly(t *testing.T) {
	root := t.TempDir()
	writeCache(t, root, modelsCached)
	reg := loadModelsUpdaterReg(t, root)
	url := serveBody(t, modelsUpstream)

	var code int
	out := captureStdout(t, func() {
		code, _ = reg.RunCommand(context.Background(), "models-update", []string{url})
	})

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
	}
	// Drift is detected and reported (gpt-4o input + output changed = 2).
	if !strings.Contains(out, "Rate drift detected (2 change(s))") {
		t.Errorf("expected 2 drift changes reported, got:\n%s", out)
	}
	if !strings.Contains(out, "gpt-4o") {
		t.Errorf("expected gpt-4o drift line, got:\n%s", out)
	}
	// The local-only id is reported and preserved on disk.
	if !strings.Contains(out, "entire-internal-model") {
		t.Errorf("expected local-only id in report, got:\n%s", out)
	}
	got := readCache(t, root)
	if !strings.Contains(got, "entire-internal-model") {
		t.Errorf("local-only model erased from written cache:\n%s", got)
	}
	if !strings.Contains(got, "0.0000025") {
		t.Errorf("written cache missing refreshed gpt-4o rate:\n%s", got)
	}
	if !strings.Contains(got, "claude-opus-4") {
		t.Errorf("written cache missing new upstream model:\n%s", got)
	}
	// The merged file is still valid JSON (upstream bytes + spliced local-only).
	if !json.Valid([]byte(got)) {
		t.Errorf("merged cache is not valid JSON:\n%s", got)
	}
}

// TestModelsUpdater_HandlesRealisticJSONShape exercises the shipped plugin's
// entire.json.decode path against the awkward shapes present in the real LiteLLM
// file: scientific-notation rates, arrays, nested objects, a brace inside a
// string value, and a decoy field whose name prefixes the real rate field.
func TestModelsUpdater_HandlesRealisticJSONShape(t *testing.T) {
	root := t.TempDir()
	writeCache(t, root, modelsRealisticCached)
	reg := loadModelsUpdaterReg(t, root)
	url := serveBody(t, modelsRealisticUpstream)

	var code int
	out := captureStdout(t, func() {
		code, _ = reg.RunCommand(context.Background(), "models-update", []string{url})
	})

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
	}
	// Exactly one rate change: input 5e-06 -> 3e-06 (scientific notation parsed).
	// The decoy input_cost_per_token_above_128k_tokens is a distinct key that the
	// exact-field lookup ignores, and the unchanged output rate must NOT be
	// counted; the "{token}" string / array / nested object are decoded natively.
	if !strings.Contains(out, "Rate drift detected (1 change(s))") {
		t.Errorf("expected exactly 1 drift change, got:\n%s", out)
	}
	if !strings.Contains(out, "input 5e-06 -> 3e-06") {
		t.Errorf("expected scientific-notation input drift line, got:\n%s", out)
	}
	if got := readCache(t, root); !json.Valid([]byte(got)) {
		t.Errorf("written cache is not valid JSON:\n%s", got)
	}
}

func TestModelsUpdater_CheckDetectsDriftNonZeroNoWrite(t *testing.T) {
	root := t.TempDir()
	writeCache(t, root, modelsCached)
	reg := loadModelsUpdaterReg(t, root)
	url := serveBody(t, modelsUpstream)

	var code int
	out := captureStdout(t, func() {
		code, _ = reg.RunCommand(context.Background(), "models-update", []string{"--check", url})
	})

	if code != 1 {
		t.Fatalf("--check with drift: exit code = %d, want 1; output:\n%s", code, out)
	}
	if !strings.Contains(out, "nothing written") {
		t.Errorf("expected --check to note nothing written, got:\n%s", out)
	}
	// --check must not modify the cache.
	if got := readCache(t, root); got != modelsCached {
		t.Errorf("--check modified the cache file:\n%s", got)
	}
}

func TestModelsUpdater_CheckCleanWhenInSync(t *testing.T) {
	root := t.TempDir()
	writeCache(t, root, modelsUpstream) // cache already matches upstream
	reg := loadModelsUpdaterReg(t, root)
	url := serveBody(t, modelsUpstream)

	var code int
	out := captureStdout(t, func() {
		code, _ = reg.RunCommand(context.Background(), "models-update", []string{"--check", url})
	})

	if code != 0 {
		t.Fatalf("--check with no drift: exit code = %d, want 0; output:\n%s", code, out)
	}
	if !strings.Contains(out, "No rate drift") {
		t.Errorf("expected no-drift note, got:\n%s", out)
	}
}

func TestModelsUpdater_HTTPErrorExitsNonZero(t *testing.T) {
	root := t.TempDir()
	reg := loadModelsUpdaterReg(t, root)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	var code int
	out := captureStdout(t, func() {
		code, _ = reg.RunCommand(context.Background(), "models-update", []string{srv.URL})
	})
	if code != 1 {
		t.Fatalf("HTTP 500: exit code = %d, want 1; output:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, ".entire", "models.json")); err == nil {
		t.Error("cache should not be written on HTTP error")
	}
}

func TestModelsUpdater_SessionStartNudgesWhenNeverFetched(t *testing.T) {
	root := t.TempDir()
	reg := loadModelsUpdaterReg(t, root)
	logs := captureSlog(t)

	reg.FireObserver(context.Background(), HookSessionStart, nil)

	if !strings.Contains(logs.String(), "never been fetched") {
		t.Errorf("expected never-fetched nudge, got logs:\n%s", logs.String())
	}
	// The logical session clock was bumped even on the never-fetched path.
	if kv := readKV(t); kv["session_count"] != "1" {
		t.Errorf("session_count = %q, want 1; kv = %v", kv["session_count"], kv)
	}
}

func TestModelsUpdater_SessionStartNudgesWhenStale(t *testing.T) {
	root := t.TempDir()
	reg := loadModelsUpdaterReg(t, root)
	// Last refreshed 30 sessions ago (>= the 25-session staleness threshold).
	seedKV(t, map[string]string{
		"session_count":        "30",
		"last_updated_session": "0",
	})
	logs := captureSlog(t)

	reg.FireObserver(context.Background(), HookSessionStart, nil)

	if !strings.Contains(logs.String(), "last refreshed") {
		t.Errorf("expected stale nudge, got logs:\n%s", logs.String())
	}
	if kv := readKV(t); kv["session_count"] != "31" {
		t.Errorf("session_count = %q, want 31", kv["session_count"])
	}
}

func TestModelsUpdater_SessionStartQuietWhenFresh(t *testing.T) {
	root := t.TempDir()
	reg := loadModelsUpdaterReg(t, root)
	// Refreshed this session: nothing stale.
	seedKV(t, map[string]string{
		"session_count":        "5",
		"last_updated_session": "5",
	})
	logs := captureSlog(t)

	reg.FireObserver(context.Background(), HookSessionStart, nil)

	if s := logs.String(); strings.Contains(s, "models-update") {
		t.Errorf("expected no nudge on the fresh path, got logs:\n%s", s)
	}
	if kv := readKV(t); kv["session_count"] != "6" {
		t.Errorf("session_count = %q, want 6", kv["session_count"])
	}
}

// --- helpers ---

// loadModelsUpdaterReg loads the real examples/plugins/models-updater plugin
// with the http+fs grant and worktreeRoot=root, into a fresh registry. It
// points ENTIRE_PLUGIN_DIR at a per-test temp dir so kv state is isolated.
func loadModelsUpdaterReg(t *testing.T, root string) *Registry {
	t.Helper()
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())
	dir := filepath.Join(examplesDir(t), modelsUpdaterName)
	grant := settings.PluginSettings{
		Enabled:      true,
		Capabilities: []string{settings.PluginCapabilityHTTP, settings.PluginCapabilityFS},
	}
	p, err := LoadPlugin(context.Background(), dir, SourceUser, root, grant)
	if err != nil {
		t.Fatalf("LoadPlugin(models-updater) error = %v", err)
	}
	reg := &Registry{}
	reg.Add(p)
	t.Cleanup(reg.Close)
	return reg
}

// serveBody starts an httptest server that returns body for any request and
// returns its URL. This is how entire.http is "stubbed": the command accepts
// the URL as a positional argument.
func serveBody(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// captureStdout redirects os.Stdout while fn runs and returns what was written.
// entire.print writes to os.Stdout, so this captures a command's user output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		if _, cerr := io.Copy(&buf, r); cerr != nil {
			buf.WriteString("\n[captureStdout: copy error: " + cerr.Error() + "]")
		}
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	out := <-done
	_ = r.Close()
	return out
}

// captureSlog installs a capturing slog default handler (the logging package
// falls back to slog.Default() when uninitialized, which it is in this package's
// tests) and restores it on cleanup. entire.log.* lines land in the buffer.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func writeCache(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".entire")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}
}

func readCache(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, ".entire", "models.json"))
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	return string(b)
}

func seedKV(t *testing.T, data map[string]string) {
	t.Helper()
	dir, err := PluginDataDir(modelsUpdaterName)
	if err != nil {
		t.Fatalf("PluginDataDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal kv: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kv.json"), b, 0o600); err != nil {
		t.Fatalf("write kv: %v", err)
	}
}

func readKV(t *testing.T) map[string]string {
	t.Helper()
	dir, err := PluginDataDir(modelsUpdaterName)
	if err != nil {
		t.Fatalf("PluginDataDir: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "kv.json"))
	if err != nil {
		t.Fatalf("read kv: %v", err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse kv: %v", err)
	}
	return m
}
