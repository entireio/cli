package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/runnerdefaults"
)

func TestRunnerDefaults_AreValidAndComplete(t *testing.T) {
	t.Parallel()

	files, err := runnerdefaults.Files()
	if err != nil {
		t.Fatalf("runnerdefaults.Files: %v", err)
	}
	if len(files) < 7 {
		t.Fatalf("expected at least 7 default runners, got %d", len(files))
	}
	for _, f := range files {
		var doc struct {
			ID     string `json:"id"`
			Output struct {
				ResultType string `json:"result_type"`
			} `json:"output"`
			Prompt struct {
				Template string `json:"template"`
			} `json:"prompt"`
		}
		if err := json.Unmarshal(f.Data, &doc); err != nil {
			t.Errorf("%s: invalid JSON: %v", f.Name, err)
			continue
		}
		if doc.ID == "" || doc.Output.ResultType == "" || doc.Prompt.Template == "" {
			t.Errorf("%s: missing contract fields (id=%q result_type=%q template_empty=%v)",
				f.Name, doc.ID, doc.Output.ResultType, doc.Prompt.Template == "")
			continue
		}
		// A default must be a *working* minimal prompt: its template has to spell
		// out the output contract its adapter expects, else it produces nothing
		// usable when left un-tailored.
		contractToken := map[string]string{
			"trail_monitor":        `"value"`,
			"code_review_comments": `"comments"`,
			"trail_review_focus":   `"files"`,
			"trail_summary":        "Problem",
		}[doc.Output.ResultType]
		if contractToken == "" {
			t.Errorf("%s: unknown result_type %q (no working-contract check)", f.Name, doc.Output.ResultType)
		} else if !strings.Contains(doc.Prompt.Template, contractToken) {
			t.Errorf("%s: template missing its output contract %q — not a working prompt", f.Name, contractToken)
		}
	}
}

func TestWriteTuneDebug(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "nested", "debug") // exercises MkdirAll
	var errOut bytes.Buffer
	writeTuneDebug(&errOut, dir, "prompt.txt", "hello-prompt")

	got, err := os.ReadFile(filepath.Join(dir, "prompt.txt"))
	if err != nil {
		t.Fatalf("reading debug file: %v", err)
	}
	if string(got) != "hello-prompt" {
		t.Errorf("debug content = %q, want %q", got, "hello-prompt")
	}
	if !strings.Contains(errOut.String(), "debug: wrote") {
		t.Errorf("expected a 'debug: wrote' notice, got %q", errOut.String())
	}
}

func TestEnsureRunnersPresent_CreatesDefaultsWhenEmpty(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	var out, errOut bytes.Buffer

	created, err := ensureRunnersPresent(&out, &errOut, repoRoot, true /* assumeYes */)
	if err != nil {
		t.Fatalf("ensureRunnersPresent: %v", err)
	}
	if len(created) < 7 {
		t.Fatalf("expected >=7 created runner IDs, got %d: %v", len(created), created)
	}

	written, err := filepath.Glob(filepath.Join(repoRoot, ".entire", "runners", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(written) < 7 {
		t.Fatalf("expected the default set written, got %d files", len(written))
	}
	// And every written file is loadable by the tuner.
	runners, err := loadTuneRunners(repoRoot, "")
	if err != nil {
		t.Fatalf("loadTuneRunners after scaffold: %v", err)
	}
	if len(runners) != len(written) {
		t.Errorf("loadTuneRunners saw %d, wrote %d", len(runners), len(written))
	}
}

func TestEnsureRunnersPresent_NoopWhenRunnersExist(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	dir := filepath.Join(repoRoot, ".entire", "runners")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trail-risk.json"),
		[]byte(`{"id":"trail-risk","prompt":{"template":"x"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	created, err := ensureRunnersPresent(&bytes.Buffer{}, &bytes.Buffer{}, repoRoot, true)
	if err != nil {
		t.Fatalf("ensureRunnersPresent: %v", err)
	}
	if len(created) != 0 {
		t.Errorf("expected no created runners when they already exist, got %v", created)
	}
	// No defaults should have been scaffolded over the existing runner.
	after, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Errorf("expected the existing single runner untouched, got %d files", len(after))
	}
}
