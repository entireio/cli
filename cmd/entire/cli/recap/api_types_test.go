package recap

import (
	"encoding/json"
	"testing"
)

func TestCheckpointAnalysisResponse_Unmarshal(t *testing.T) {
	t.Parallel()
	raw := `{
		"pipeline_version": "2026-04-10.v3",
		"totalSteps": 7,
		"totalFilesChanged": 3,
		"total_transcript_tokens": 45000,
		"extraction": {
			"labels": ["feature_build", "testing"],
			"blocks": []
		},
		"toolProfile": {
			"categories": {
				"shell":    {"count": 4, "durationMs": 18000},
				"fileOps":  {"count": 12},
				"search":   {"count": 3, "durationMs": 2400},
				"mcp":      {"count": 0},
				"agent":    {"count": 1},
				"other":    {"count": 0}
			},
			"total": 20
		},
		"skillsUsed": ["skill-a", "skill-b"],
		"modelsUsed": ["sonnet-4"]
	}`
	var resp CheckpointAnalysisResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.PipelineVersion != "2026-04-10.v3" {
		t.Errorf("PipelineVersion = %q", resp.PipelineVersion)
	}
	if len(resp.Extraction.Labels) != 2 || resp.Extraction.Labels[0] != "feature_build" {
		t.Errorf("Labels = %v", resp.Extraction.Labels)
	}
	if resp.ToolProfile == nil {
		t.Fatal("ToolProfile nil")
	}
	shell := resp.ToolProfile.Categories["shell"]
	if shell.Count != 4 || shell.DurationMs != 18000 {
		t.Errorf("shell = %+v", shell)
	}
	fileOps := resp.ToolProfile.Categories["fileOps"]
	if fileOps.Count != 12 || fileOps.DurationMs != 0 {
		t.Errorf("fileOps = %+v (expected Count=12, DurationMs=0)", fileOps)
	}
}

func TestCanonicalLabels(t *testing.T) {
	t.Parallel()
	want := []string{
		"feature_build", "bug_fix", "refactor", "performance", "optimization",
		"investigation", "configuration", "testing", "deployment", "recovery",
		"planning", "security_fix", "documentation", "migration", "telemetry",
		"observability", "enhancement", "dependencies",
	}
	if len(CanonicalLabels) != len(want) {
		t.Fatalf("CanonicalLabels len = %d, want %d", len(CanonicalLabels), len(want))
	}
	for i := range want {
		if CanonicalLabels[i] != want[i] {
			t.Errorf("CanonicalLabels[%d] = %q, want %q", i, CanonicalLabels[i], want[i])
		}
	}
}

func TestIsCanonicalLabel(t *testing.T) {
	t.Parallel()
	if !IsCanonicalLabel("feature_build") {
		t.Error("feature_build should be canonical")
	}
	if IsCanonicalLabel("build") {
		t.Error("build (short form) should NOT be canonical")
	}
	if IsCanonicalLabel("") {
		t.Error("empty string should NOT be canonical")
	}
}
