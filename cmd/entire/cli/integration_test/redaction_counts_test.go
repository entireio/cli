//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

type explainRedactionCounts struct {
	Replacements      int            `json:"replacements"`
	Layers            map[string]int `json:"layers"`
	CustomRulesActive int            `json:"custom_rules_active"`
}

type explainRedactionEnvelope struct {
	Redaction *explainRedactionCounts `json:"redaction"`
	Sessions  []struct {
		Redaction *explainRedactionCounts `json:"redaction"`
	} `json:"sessions"`
}

func explainRedaction(t *testing.T, env *TestEnv, cpID string) explainRedactionEnvelope {
	t.Helper()
	out := env.RunCLI("checkpoint", "explain", "--checkpoint", cpID, "--json")
	var envelope explainRedactionEnvelope
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("parse explain --json: %v\n%s", err, out)
	}
	if len(envelope.Sessions) != 1 || envelope.Sessions[0].Redaction == nil || envelope.Redaction == nil {
		t.Fatalf("explain --json must report session and checkpoint redaction counts:\n%s", out)
	}
	return envelope
}

// TestRedactionCounts_FullHookFlow drives the real hook binary through a
// mid-turn commit (condensation) and the Stop that finalizes the checkpoint
// with the full transcript, and reads the counts back through
// `entire checkpoint explain`. It proves the counts reach checkpoint metadata
// on both write paths, that a custom rule is reported as having run, and that
// neither the redacted values nor the rule's name or pattern are stored.
func TestRedactionCounts_FullHookFlow(t *testing.T) {
	t.Parallel()

	const (
		ruleName       = "project_falcon"
		customMatch    = "FALCON-482913"
		customMatch2   = "FALCON-771204"
		entropySecret  = "Xk9mP2qR7vL4nW8jT3hY6bF1cD5gA0sZ"
		entropySecret2 = "Qw3eR5tY7uI9oP1aS3dF5gH7jK9lZ2xC"
	)

	env := NewFeatureBranchEnv(t)
	env.PatchSettings(map[string]any{
		"redaction": map[string]any{
			"custom_redactions": map[string]any{ruleName: "FALCON-[0-9]{6}"},
		},
	})
	session := env.NewSession()

	prompt := "Deploy the build with token " + scannerSelectionPAT
	lines := []string{
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"` + prompt + `"},"timestamp":"2026-01-01T00:00:00Z"}`,
		`{"uuid":"a1","type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"grep ` + customMatch + ` build.log"}}]},"timestamp":"2026-01-01T00:00:01Z"}`,
		`{"uuid":"u2","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"found value ` + entropySecret + ` in build.log"}]},"timestamp":"2026-01-01T00:00:02Z"}`,
		`{"uuid":"a2","type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Write","input":{"file_path":"feature.go","content":"package main\n"}}]},"timestamp":"2026-01-01T00:00:03Z"}`,
		`{"uuid":"u3","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"Success"}]},"timestamp":"2026-01-01T00:00:04Z"}`,
	}
	writeTranscript := func() {
		t.Helper()
		if err := os.WriteFile(session.TranscriptPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
	}
	writeTranscript()

	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, prompt, session.TranscriptPath); err != nil {
		t.Fatalf("UserPromptSubmit: %v", err)
	}
	env.WriteFile("feature.go", "package main\n")
	env.GitCommitWithHooks("add feature", "feature.go")

	cpID := env.GetLatestCheckpointIDFromHistory()
	if cpID == "" {
		t.Fatal("no checkpoint id found in history")
	}

	afterCommit := explainRedaction(t, env, cpID)
	counts := afterCommit.Sessions[0].Redaction
	t.Logf("after commit: %+v", *counts)
	if counts.Layers["custom"] != 1 || counts.CustomRulesActive != 1 {
		t.Fatalf("the custom rule must be reported as run and matched once, got %+v", *counts)
	}
	if counts.Layers["betterleaks"] < 1 || counts.Layers["entropy"] < 1 || counts.Replacements < 3 {
		t.Fatalf("token, custom match and entropy value must all be counted, got %+v", *counts)
	}
	if !reflect.DeepEqual(afterCommit.Redaction, counts) {
		t.Fatalf("single-session checkpoint total %+v must equal its session's %+v", *afterCommit.Redaction, *counts)
	}

	// The rest of the turn lands after the commit; Stop finalizes the
	// checkpoint with the whole transcript, and the counts must follow.
	lines = append(lines,
		`{"uuid":"a3","type":"assistant","message":{"content":[{"type":"text","text":"also saw `+customMatch2+` and `+entropySecret2+`"}]},"timestamp":"2026-01-01T00:00:05Z"}`)
	writeTranscript()
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	afterStop := explainRedaction(t, env, cpID).Sessions[0].Redaction
	t.Logf("after stop: %+v", *afterStop)
	if afterStop.Layers["custom"] != 2 || afterStop.Replacements != counts.Replacements+2 {
		t.Fatalf("finalize must recount the full transcript: before %+v, after %+v", *counts, *afterStop)
	}

	text := env.RunCLI("checkpoint", "explain", "--checkpoint", cpID)
	t.Logf("explain:\n%s", text)
	if !strings.Contains(text, "replacements in this session so far") || !strings.Contains(text, "custom 2 (1 rule)") {
		t.Errorf("explain text must show the redaction row:\n%s", text)
	}

	base := ShardedCheckpointPath(cpID)
	for _, path := range []string{base + "/" + paths.MetadataFileName, base + "/0/" + paths.MetadataFileName} {
		blob, ok := env.ReadFileFromBranch(paths.MetadataBranchName, path)
		if !ok {
			t.Fatalf("%s missing", path)
		}
		for _, leaked := range []string{customMatch, customMatch2, entropySecret, entropySecret2, scannerSelectionPAT, ruleName, "FALCON"} {
			if strings.Contains(blob, leaked) {
				t.Errorf("%s stores %q; counts must hold no values or rule text", path, leaked)
			}
		}
	}
}
