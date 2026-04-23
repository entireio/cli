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

// Analysis status values returned by the /checkpoints/:id/analysis
// endpoint. The response is a discriminated union (see
// frontend/src/domains/platform/checkpoints/api.ts: CheckpointAnalysisResponse).
// Only "complete" carries the extraction/tool-profile/token fields — every
// other status means "no data yet."
const (
	AnalysisStatusComplete     = "complete"
	AnalysisStatusPending      = "pending"
	AnalysisStatusGenerating   = "generating"
	AnalysisStatusFailed       = "failed"
	AnalysisStatusNotAvailable = "not_available"
)

// CheckpointAnalysisResponse mirrors api/src/types.ts StoredCheckpointAnalysis
// plus the status discriminator. Used only for unmarshaling server responses.
// Callers must check Status == AnalysisStatusComplete before trusting any
// of the data fields — pending/generating/failed responses carry zero
// values, not absent fields.
type CheckpointAnalysisResponse struct {
	Status                string               `json:"status"`
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

// IsComplete reports whether the response carries a finished analysis. A
// nil receiver, missing status, or any non-"complete" status returns false.
func (r *CheckpointAnalysisResponse) IsComplete() bool {
	return r != nil && r.Status == AnalysisStatusComplete
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
