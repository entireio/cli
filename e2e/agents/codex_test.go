package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSeedCodexHome_WritesAPIKeyAuth(t *testing.T) {
	home := t.TempDir()
	projectDir := filepath.Join(t.TempDir(), "repo")
	t.Setenv("OPENAI_API_KEY", "sk-test-key")
	t.Setenv("E2E_CODEX_MODEL", "gpt-5.4")

	if err := os.MkdirAll(projectDir, 0o750); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	if err := seedCodexHome(home, projectDir); err != nil {
		t.Fatalf("seedCodexHome: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}

	var auth struct {
		AuthMode     string `json:"auth_mode"`
		OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		t.Fatalf("unmarshal auth.json: %v", err)
	}

	if auth.AuthMode != "apikey" {
		t.Fatalf("auth_mode = %q, want %q", auth.AuthMode, "apikey")
	}
	if auth.OpenAIAPIKey != "sk-test-key" {
		t.Fatalf("OPENAI_API_KEY = %q, want %q", auth.OpenAIAPIKey, "sk-test-key")
	}
}

// Pane captures from CI run 35841876167, where Codex 0.156.1 drew a
// model-migration dialog over the composer at startup. Its selected row
// renders the same "›" the composer does, so PromptPattern alone cannot tell
// the two screens apart.
const codexMigrationDialogPane = `
  Meet GPT-6 Luna

  Our latest Luna is significantly more efficient, so your usage limits go
  even further. Reach for it for any job that doesn't require frontier
  intelligence.

› 1. Try new model
  2. Use existing model

  enter/esc confirm · ctrl+c quit`

const codexMigrationDialogExistingSelectedPane = `
  Meet GPT-6 Luna

  Our latest Luna is significantly more efficient, so your usage limits go
  even further. Reach for it for any job that doesn't require frontier
  intelligence.

  1. Try new model
› 2. Use existing model

  enter/esc confirm · ctrl+c quit`

const codexComposerPane = `
╭───────────────────────────────────────────────────────╮
│ >_ OpenAI Codex (v0.156.1)                            │
│                                                       │
│ model:       GPT-6-Luna medium   /model to change     │
│ directory:   ~/…/entire-e2e-repos/e2e-repo-2205977439 │
│ permissions: YOLO mode                                │
╰───────────────────────────────────────────────────────╯

  Tip: Use /fork to branch the current chat into a new thread.


› Ask Codex to do anything

  GPT-6-Luna medium · ~/work/cli/cli/e2e/artifacts/2026-09-23T09-17-10/entire-c…`

// A pane from a session that already has history — the shape a resumed
// session reaches, where the composer sits under earlier turns.
const codexComposerAfterTurnPane = `
↳ Hook · Entire CLI will link this conversation to your next commit.

• Created docs/hello.md. The change is uncommitted.

  done 7:51 PM


› Ask Codex to do anything

  gpt-5.6-luna default · ~/work/cli/cli/e2e/artifacts/2026-09-22T19-48-17/entir…`

// An older dialog whose wording the previous detection keyed on. It offers no
// model choice, so it is answered by its default.
const codexLegacyDialogPane = `
  Codex can access this folder

  Use ↑/↓ to move, press enter to confirm

› Yes, allow
  No, exit`

func TestCodexStartupReady(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pane string
		want bool
	}{
		{"fresh composer", codexComposerPane, true},
		{"composer under earlier turns", codexComposerAfterTurnPane, true},
		{"model migration dialog", codexMigrationDialogPane, false},
		{"legacy confirm dialog", codexLegacyDialogPane, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := codexStartupReady(tc.pane); got != tc.want {
				t.Fatalf("codexStartupReady() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCodexStartupOffersExistingModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pane string
		want bool
	}{
		{"model migration dialog", codexMigrationDialogPane, true},
		{"legacy confirm dialog", codexLegacyDialogPane, false},
		{"composer", codexComposerPane, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := codexStartupOffersExistingModel(tc.pane); got != tc.want {
				t.Fatalf("codexStartupOffersExistingModel() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCodexStartupSelectionIsExistingModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pane string
		want bool
	}{
		{"upgrade option selected", codexMigrationDialogPane, false},
		{"existing model selected", codexMigrationDialogExistingSelectedPane, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := codexStartupSelectionIsExistingModel(tc.pane); got != tc.want {
				t.Fatalf("codexStartupSelectionIsExistingModel() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Constructed, not captured: Codex only renders the opt-out row while the
// configured model is still one of its presets. A retired model leaves the
// upgrade as the only answer.
const codexMigrationDialogNoOptOutPane = `
  Meet GPT-6 Sol

  Our latest Sol is more intelligent and more efficient so your usage limits
  go further.

› Try new model

  enter/esc confirm · ctrl+c quit`

func TestCodexStartupOffersUpgrade(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pane string
		want bool
	}{
		{"model migration dialog", codexMigrationDialogPane, true},
		{"model migration dialog without opt-out", codexMigrationDialogNoOptOutPane, true},
		{"legacy confirm dialog", codexLegacyDialogPane, false},
		{"composer", codexComposerPane, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := codexStartupOffersUpgrade(tc.pane); got != tc.want {
				t.Fatalf("codexStartupOffersUpgrade() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A migration dialog with no opt-out row must not read as one that can be
// answered safely: every answer there changes the model.
func TestCodexStartupOffersExistingModel_MigrationWithoutOptOut(t *testing.T) {
	t.Parallel()

	if codexStartupOffersExistingModel(codexMigrationDialogNoOptOutPane) {
		t.Fatal("codexStartupOffersExistingModel() = true, want false")
	}
	if codexStartupSelectionIsExistingModel(codexMigrationDialogNoOptOutPane) {
		t.Fatal("codexStartupSelectionIsExistingModel() = true, want false")
	}
}
