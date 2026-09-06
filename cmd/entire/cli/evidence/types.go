// Package evidence defines the privacy-safe handoff from local checkpoint
// collection to an external evaluator.
package evidence

import "strings"

type ContextStatus string

const (
	ContextComplete   ContextStatus = "COMPLETE"
	ContextIncomplete ContextStatus = "INCOMPLETE"
)

type TestStatus string

const (
	TestStatusUnknown     TestStatus = "UNKNOWN"
	TestStatusUnavailable TestStatus = "UNAVAILABLE"
)

// SanitizedEvidence is safe to serialize or send to an external evaluator.
// It deliberately has no prompt, transcript, patch, arbitrary JSON, or raw
// command-output fields.
type SanitizedEvidence struct {
	CheckpointID  string            `json:"checkpoint_id"`
	ContextStatus ContextStatus     `json:"context_status"`
	Requirements  []Requirement     `json:"requirements"`
	ChangedFiles  []string          `json:"changed_files"`
	Structural    StructuralEvidence `json:"structural_evidence"`
	Graph         GraphEvidence     `json:"graph_evidence"`
	Tests         []TestEvidence    `json:"tests"`
}

type Requirement struct {
	ID string `json:"id"`
}

type StructuralEvidence struct {
	LinkedCommitCount int `json:"linked_commit_count"`
}

type GraphEvidence struct {
	Available bool     `json:"available"`
	References []string `json:"references"`
}

type TestEvidence struct {
	Scope      string     `json:"scope"`
	Status     TestStatus `json:"status"`
	Provenance string     `json:"provenance"`
}

// LocalInput may hold raw local data, but it is never embedded in or returned
// by SanitizedEvidence. Intent is used only for conservative local extraction.
type LocalInput struct {
	CheckpointID       string
	Intent             string
	Transcript         []byte
	ChangedFiles       []string
	LinkedCommitCount  int
	GraphReferences    []string
	GraphAvailable     bool
	HistoricalTestsSet bool
}

func Sanitize(input LocalInput) SanitizedEvidence {
	requirements, complete := atomicRequirements(input.Intent)
	status := ContextIncomplete
	if complete {
		status = ContextComplete
	}
	return SanitizedEvidence{
		CheckpointID:  input.CheckpointID,
		ContextStatus: status,
		Requirements:  requirements,
		ChangedFiles:  nonEmpty(input.ChangedFiles),
		Structural:    StructuralEvidence{LinkedCommitCount: input.LinkedCommitCount},
		Graph:         GraphEvidence{Available: input.GraphAvailable, References: nonEmpty(input.GraphReferences)},
		Tests: []TestEvidence{{
			Scope:      "historical_checkpoint",
			Status:     historicalTestStatus(input.HistoricalTestsSet),
			Provenance: "checkpoint storage has no authoritative historical test result",
		}},
	}
}

func atomicRequirements(intent string) ([]Requirement, bool) {
	lines := strings.Split(intent, "\n")
	requirements := make([]Requirement, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") || len(strings.TrimSpace(line[2:])) == 0 {
			continue
		}
		requirements = append(requirements, Requirement{ID: "requirement_" + string(rune('1'+len(requirements)))})
	}
	return requirements, len(requirements) > 0
}

func historicalTestStatus(available bool) TestStatus {
	if available {
		return TestStatusUnknown
	}
	return TestStatusUnavailable
}

func nonEmpty(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}
