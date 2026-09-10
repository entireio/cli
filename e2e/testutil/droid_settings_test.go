package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDroidSettingsPreserveHooksWithoutBYOK(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "settings.json")
	const hooks = `{"SessionStart":[{"hooks":[{"type":"command","command":"entire hooks factoryai-droid session-start"}]}]}`
	if err := os.WriteFile(path, []byte(`{"hooks":`+hooks+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeDroidRepoSettings(path); err != nil {
		t.Fatal(err)
	}
	settings, err := loadJSONMap(path, "test settings")
	if err != nil {
		t.Fatal(err)
	}
	var model string
	if err := json.Unmarshal(settings["model"], &model); err != nil {
		t.Fatal(err)
	}
	if model != "claude-haiku-4-5-20251001" {
		t.Fatalf("expected Factory-managed Haiku, got %q", model)
	}
	if _, exists := settings["customModels"]; exists {
		t.Fatal("managed model must not need custom model credentials")
	}
	var gotHooks, wantHooks any
	if err := json.Unmarshal(settings["hooks"], &gotHooks); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(hooks), &wantHooks); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotHooks, wantHooks) {
		t.Fatalf("hooks changed: %s", settings["hooks"])
	}
}
