package strategy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// resolveOPFDecision is the pure-logic core: env > settings > prompt >
// non-TTY auto-run. Table-driven because every case is just a different
// (env, setting, tty, prompter) input combination.
func TestResolveOPFDecision_Precedence(t *testing.T) {
	t.Parallel()

	const promptCalled = OPFDecision(-1) // sentinel; only used when prompter fires
	promptYes := func() (OPFDecision, error) { return OPFRun, nil }
	promptNo := func() (OPFDecision, error) { return OPFSkip, nil }
	promptAbort := func() (OPFDecision, error) { return OPFAbort, nil }
	promptErr := func() (OPFDecision, error) { return OPFAbort, errors.New("boom") }
	promptNever := func() (OPFDecision, error) {
		t.Helper()
		t.Fatal("prompter must not be called")
		return promptCalled, nil
	}

	cases := []struct {
		name           string
		env            string
		promptDefault  string
		hasTTY         bool
		prompter       func() (OPFDecision, error)
		want           OPFDecision
		wantErr        bool
		wantErrMessage string
	}{
		// Env wins everywhere
		{name: "env_yes_wins_over_setting_never", env: "yes", promptDefault: settings.OPFPromptNever, hasTTY: true, prompter: promptNever, want: OPFRun},
		{name: "env_no_wins_over_setting_always", env: "no", promptDefault: settings.OPFPromptAlways, hasTTY: true, prompter: promptNever, want: OPFSkip},
		{name: "env_yes_case_insensitive", env: "YES", promptDefault: "", hasTTY: false, prompter: promptNever, want: OPFRun},
		{name: "env_no_with_whitespace", env: "  no  ", promptDefault: "", hasTTY: false, prompter: promptNever, want: OPFSkip},
		// Setting wins over prompt
		{name: "setting_never_skips_prompt", env: "", promptDefault: settings.OPFPromptNever, hasTTY: true, prompter: promptNever, want: OPFSkip},
		{name: "setting_always_skips_prompt", env: "", promptDefault: settings.OPFPromptAlways, hasTTY: true, prompter: promptNever, want: OPFRun},
		// Non-TTY fallback: run (matches the "if enabled, just run" semantics)
		{name: "no_tty_auto_runs", env: "", promptDefault: "", hasTTY: false, prompter: promptNever, want: OPFRun},
		{name: "no_tty_ignores_ask_setting", env: "", promptDefault: settings.OPFPromptAsk, hasTTY: false, prompter: promptNever, want: OPFRun},
		// TTY + ask → prompter is called
		{name: "tty_ask_user_chose_yes", env: "", promptDefault: settings.OPFPromptAsk, hasTTY: true, prompter: promptYes, want: OPFRun},
		{name: "tty_ask_user_chose_no", env: "", promptDefault: settings.OPFPromptAsk, hasTTY: true, prompter: promptNo, want: OPFSkip},
		{name: "tty_ask_user_aborted", env: "", promptDefault: settings.OPFPromptAsk, hasTTY: true, prompter: promptAbort, want: OPFAbort},
		// TTY + empty setting == ask
		{name: "tty_empty_setting_treated_as_ask", env: "", promptDefault: "", hasTTY: true, prompter: promptYes, want: OPFRun},
		// Prompter errors propagate
		{name: "prompter_error", env: "", promptDefault: "", hasTTY: true, prompter: promptErr, want: OPFAbort, wantErr: true, wantErrMessage: "boom"},
		// Unrecognized env values fall through to next layer
		{name: "env_bogus_falls_through_to_setting", env: "maybe", promptDefault: settings.OPFPromptAlways, hasTTY: true, prompter: promptNever, want: OPFRun},
		{name: "env_empty_falls_through_to_setting", env: "", promptDefault: settings.OPFPromptAlways, hasTTY: true, prompter: promptNever, want: OPFRun},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveOPFDecision(tc.env, tc.promptDefault, tc.hasTTY, tc.prompter)
			if tc.wantErr {
				require.Error(t, err)
				if tc.wantErrMessage != "" {
					require.Contains(t, err.Error(), tc.wantErrMessage)
				}
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.want, got, "decision")
		})
	}
}

// TestPersistOPFPromptDefaultAlways_WritesNestedField verifies that the
// "Always" branch updates redaction.openai_privacy_filter.prompt_default
// in .entire/settings.local.json without disturbing other fields.
//
// Modifies process cwd (no t.Parallel), but uses t.Chdir so subsequent
// tests see the reverted cwd.
func TestPersistOPFPromptDefaultAlways_WritesNestedField(t *testing.T) {
	tempDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, paths.EntireDir), 0o755))
	// Seed an existing settings.local.json with some unrelated content
	// so we can verify it survives the write.
	existing := `{
  "enabled": true,
  "redaction": {
    "openai_privacy_filter": {
      "categories": {"private_person": true}
    }
  }
}`
	localPath := filepath.Join(tempDir, paths.EntireDir, "settings.local.json")
	require.NoError(t, os.WriteFile(localPath, []byte(existing), 0o644))
	t.Chdir(tempDir)

	require.NoError(t, persistOPFPromptDefaultAlways(context.Background()))

	got, err := os.ReadFile(localPath)
	require.NoError(t, err)

	// Parse and verify structure: enabled stays true, categories stay,
	// new prompt_default key present with "always".
	var parsed struct {
		Enabled   bool `json:"enabled"`
		Redaction struct {
			OPF struct {
				Categories    map[string]bool `json:"categories"`
				PromptDefault string          `json:"prompt_default"`
			} `json:"openai_privacy_filter"`
		} `json:"redaction"`
	}
	require.NoError(t, json.Unmarshal(got, &parsed))
	require.True(t, parsed.Enabled, "existing enabled field must survive")
	require.True(t, parsed.Redaction.OPF.Categories["private_person"], "existing categories must survive")
	require.Equal(t, settings.OPFPromptAlways, parsed.Redaction.OPF.PromptDefault)
}

// TestPersistOPFPromptDefaultAlways_CreatesFileFromScratch covers the
// fresh-install path where .entire/settings.local.json doesn't exist yet.
func TestPersistOPFPromptDefaultAlways_CreatesFileFromScratch(t *testing.T) {
	tempDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, paths.EntireDir), 0o755))
	t.Chdir(tempDir)

	require.NoError(t, persistOPFPromptDefaultAlways(context.Background()))

	localPath := filepath.Join(tempDir, paths.EntireDir, "settings.local.json")
	got, err := os.ReadFile(localPath)
	require.NoError(t, err, "settings.local.json should be created")

	var parsed struct {
		Redaction struct {
			OPF struct {
				PromptDefault string `json:"prompt_default"`
			} `json:"openai_privacy_filter"`
		} `json:"redaction"`
	}
	require.NoError(t, json.Unmarshal(got, &parsed))
	require.Equal(t, settings.OPFPromptAlways, parsed.Redaction.OPF.PromptDefault)
}

// TestPrePush_OPFProgressUsesConfiguredWriter pins the test-noise escape hatch:
// PrePush still emits the non-interactive OPF progress notice in production,
// but tests can redirect it away from process stderr.
func TestPrePush_OPFProgressUsesConfiguredWriter(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, paths.EntireDir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, paths.EntireDir, "settings.json"), []byte(`{
  "enabled": true,
  "redaction": {
    "openai_privacy_filter": {
      "enabled": true,
      "categories": {"private_person": true}
    }
  }
}`), 0o644))
	t.Chdir(tmpDir)
	configureFakeOPF(t, &fakeOPFForRewrite{})

	var out bytes.Buffer
	withOPFPrePushProgressWriterForTest(t, &out)

	require.NoError(t, (&ManualCommitStrategy{}).PrePush(t.Context(), "origin"))
	require.Contains(t, out.String(), "OpenAI Privacy Filter: scanning checkpoints before push")
}

func withOPFPrePushProgressWriterForTest(t testing.TB, w io.Writer) {
	t.Helper()
	previous := opfPrePushProgressWriter
	opfPrePushProgressWriter = w
	t.Cleanup(func() {
		opfPrePushProgressWriter = previous
	})
}
