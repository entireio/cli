package cloudenv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnsureCursorEnvironment_DoesNotCreateFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ctx := t.Context()

	res, err := EnsureCursorEnvironment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatal("must not invent .cursor/environment.json")
	}
	if !strings.Contains(res.Message, EnvironmentJSONRel) {
		t.Fatalf("missing hint, got %q", res.Message)
	}
	if _, err := os.Stat(filepath.Join(dir, ".cursor", "environment.json")); !os.IsNotExist(err) {
		t.Fatal("environment.json was created")
	}
	if _, err := os.Stat(filepath.Join(dir, ".entire", InstallCLIScriptName)); err != nil {
		t.Fatalf("expected install helper: %v", err)
	}
}

func TestEnsureCursorEnvironment_AppendsInstall(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ctx := t.Context()
	envDir := filepath.Join(dir, ".cursor")
	if err := os.MkdirAll(envDir, 0o750); err != nil {
		t.Fatal(err)
	}
	original := `{
  "name": "app",
  "install": "npm ci",
  "unknownField": true
}
`
	if err := os.WriteFile(filepath.Join(envDir, "environment.json"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := EnsureCursorEnvironment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("expected environment.json to be patched")
	}

	raw := readRawEnv(t, dir)
	if _, ok := raw["unknownField"]; !ok {
		t.Fatal("unknown top-level field was dropped")
	}
	var install string
	if err := json.Unmarshal(raw["install"], &install); err != nil {
		t.Fatal(err)
	}
	if install != "npm ci && "+InstallCLIStep {
		t.Fatalf("install = %q", install)
	}

	res2, err := EnsureCursorEnvironment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Changed {
		t.Fatal("second ensure should be a no-op")
	}
}

func TestEnsureCursorEnvironment_SkipsWhenReferencedScriptInstallsEntire(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ctx := t.Context()
	envDir := filepath.Join(dir, ".cursor")
	if err := os.MkdirAll(envDir, 0o750); err != nil {
		t.Fatal(err)
	}
	script := "#!/usr/bin/env bash\nsudo ln -sf \"${REPO_ROOT}/entire\" /usr/local/bin/entire\n"
	if err := os.WriteFile(filepath.Join(envDir, "install.sh"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	env := `{"name":"Entire CLI","install":"bash .cursor/install.sh"}` + "\n"
	envPath := filepath.Join(envDir, "environment.json")
	if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := EnsureCursorEnvironment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatal("must not rewrite an install that already puts entire on PATH")
	}
	after, err := os.ReadFile(envPath) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != env {
		t.Fatalf("environment.json rewritten:\n%s", after)
	}
}

func TestRemoveCursorEnvironment_StripsStepOnly(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ctx := t.Context()
	envDir := filepath.Join(dir, ".cursor")
	if err := os.MkdirAll(envDir, 0o750); err != nil {
		t.Fatal(err)
	}
	env := `{"name":"app","install":"npm ci && bash .entire/install-cli.sh"}` + "\n"
	if err := os.WriteFile(filepath.Join(envDir, "environment.json"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RemoveCursorEnvironment(ctx); err != nil {
		t.Fatal(err)
	}
	raw := readRawEnv(t, dir)
	var install string
	if err := json.Unmarshal(raw["install"], &install); err != nil {
		t.Fatal(err)
	}
	if install != "npm ci" {
		t.Fatalf("install = %q, want npm ci", install)
	}
	if _, ok := raw["name"]; !ok {
		t.Fatal("name field dropped")
	}
}

func TestEnsureCursorEnvironment_MalformedJSONDoesNotFail(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ctx := t.Context()
	envDir := filepath.Join(dir, ".cursor")
	if err := os.MkdirAll(envDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "environment.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := EnsureCursorEnvironment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatal("must not rewrite unparseable environment.json")
	}
	if !strings.Contains(res.Message, "Could not parse") {
		t.Fatalf("expected parse warning, got %q", res.Message)
	}
}

func readRawEnv(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".cursor", "environment.json")) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCommittedCursorEnvironmentAlreadyInstallsEntire(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".cursor", "environment.json")) //nolint:gosec // module root
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var install string
	if err := json.Unmarshal(raw["install"], &install); err != nil {
		t.Fatal(err)
	}
	if !MentionsEntireInstall(install, root) {
		t.Fatalf("this repo's %s install %q does not put entire on PATH; Cloud Agent hooks would no-op",
			EnvironmentJSONRel, install)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
