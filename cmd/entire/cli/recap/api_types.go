package recap

// CanonicalLabels is the full 18-value taxonomy from api/src/types.ts
// (CHECKPOINT_LABELS). Order matters: it defines the canonical display
// order when rendering mode panels.
var CanonicalLabels = []string{
	"feature_build", "bug_fix", "refactor", "performance", "optimization",
	"investigation", "configuration", "testing", "deployment", "recovery",
	"planning", "security_fix", "documentation", "migration", "telemetry",
	"observability", "enhancement", "dependencies",
}

var canonicalSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(CanonicalLabels))
	for _, l := range CanonicalLabels {
		m[l] = struct{}{}
	}
	return m
}()

// IsCanonicalLabel reports whether s is one of the 18 server labels.
// Use this to silently drop unknown labels from api responses so a
// server-side taxonomy expansion doesn't crash the CLI.
func IsCanonicalLabel(s string) bool {
	_, ok := canonicalSet[s]
	return ok
}

// CheckpointAnalysisResponse mirrors api/src/types.ts StoredCheckpointAnalysis.
// Used only for unmarshaling server responses.
type CheckpointAnalysisResponse struct {
	PipelineVersion       string               `json:"pipeline_version"`
	TotalSteps            int                  `json:"totalSteps"`
	TotalFilesChanged     int                  `json:"totalFilesChanged"`
	TotalTranscriptTokens int                  `json:"total_transcript_tokens"`
	Extraction            CheckpointExtraction `json:"extraction"`
	ToolProfile           *ToolProfile         `json:"toolProfile,omitempty"`
	SkillsUsed            []string             `json:"skillsUsed,omitempty"`
	MCPServersUsed        []MCPServerUsage     `json:"mcpServersUsed,omitempty"`
	ModelsUsed            []string             `json:"modelsUsed,omitempty"`
	AgentsUsed            []string             `json:"agentsUsed,omitempty"`
}

// CheckpointExtraction mirrors the api's CheckpointExtraction.
// Blocks are omitted from the recap package — we only need labels.
type CheckpointExtraction struct {
	Labels []string `json:"labels"`
}

// MCPServerUsage mirrors { name, count } entries in mcpServersUsed.
type MCPServerUsage struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}
