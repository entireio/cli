//go:build integration

package integration

import (
	"os"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// TestStructuredFieldRedaction_FullHookFlow drives the real hook binary through
// a Claude Code session that carries secrets only structured fields reveal,
// then reads the checkpoint back:
//   - a hex API key under "api_key" in a tool input (no layer flags the hex
//     alone; its key is what marks it),
//   - a GitHub token in an object nested under "valid", a key the id skip
//     rule matches,
//   - a prompt whose first word is that token, which the slash-command skill
//     event used to copy into metadata.json.
func TestStructuredFieldRedaction_FullHookFlow(t *testing.T) {
	// Hook subprocesses share settings/env; do not run in parallel.
	const (
		hexAPIKey      = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
		captureControl = "CAPTURE_CONTROL_MARKER_3136"
	)

	env := NewFeatureBranchEnv(t)
	session := env.NewSession()

	prompt := "/" + scannerSelectionPAT + " is my token, deploy with it. " + captureControl
	transcript := strings.Join([]string{
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"` + prompt + `"},"timestamp":"2026-01-01T00:00:00Z"}`,
		`{"uuid":"a1","type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"mcp__acme__deploy","input":{"api_key":"` + hexAPIKey + `","valid":{"token":"` + scannerSelectionPAT + `"},"note":"` + captureControl + `"}}]},"timestamp":"2026-01-01T00:00:01Z"}`,
		`{"uuid":"u2","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"deployed"}]},"timestamp":"2026-01-01T00:00:02Z"}`,
		`{"uuid":"a2","type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Write","input":{"file_path":"feature.go","content":"package main\n"}}]},"timestamp":"2026-01-01T00:00:03Z"}`,
		`{"uuid":"u3","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"Success"}]},"timestamp":"2026-01-01T00:00:04Z"}`,
		`{"uuid":"a3","type":"assistant","message":{"content":[{"type":"text","text":"done"}]},"timestamp":"2026-01-01T00:00:05Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(session.TranscriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, prompt, session.TranscriptPath); err != nil {
		t.Fatalf("UserPromptSubmit: %v", err)
	}
	env.WriteFile("feature.go", "package main\n")
	env.GitCommitWithHooks("add feature", "feature.go")
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	cpID := env.GetLatestCheckpointIDFromHistory()
	if cpID == "" {
		t.Fatal("no checkpoint id found in history")
	}
	sessionPath := ShardedCheckpointPath(cpID) + "/0/"
	read := func(name string) string {
		t.Helper()
		content, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+name)
		if !ok {
			t.Fatalf("%s missing at %s", name, sessionPath)
		}
		return content
	}

	full := read(paths.TranscriptFileName)
	if !strings.Contains(full, captureControl) {
		t.Fatalf("capture-control marker missing from full.jsonl; the secret checks below would be inconclusive:\n%s", full)
	}
	if strings.Contains(full, hexAPIKey) {
		t.Errorf("api_key value survived into full.jsonl:\n%s", full)
	}
	if strings.Contains(full, scannerSelectionPAT) {
		t.Errorf("token nested under a skip key survived into full.jsonl:\n%s", full)
	}

	prompts := read(paths.PromptFileName)
	if strings.Contains(prompts, scannerSelectionPAT) || !strings.Contains(prompts, "REDACTED") {
		t.Errorf("prompt.txt should carry the redacted prompt, got:\n%s", prompts)
	}

	metadataJSON := read(paths.MetadataFileName)
	if strings.Contains(metadataJSON, scannerSelectionPAT) {
		t.Errorf("token from the prompt's first word survived into metadata.json:\n%s", metadataJSON)
	}
	for _, event := range readSessionMetadata(t, env, cpID).SkillEvents {
		if event.Source.Signal == agent.SkillSignalPromptSlashCommand {
			t.Errorf("a prompt starting with a secret recorded a slash-command skill event: %+v", event)
		}
	}
}
