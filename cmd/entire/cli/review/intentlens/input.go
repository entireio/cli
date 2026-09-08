package intentlens

import (
	"errors"
	"fmt"
)

// RequirementSignals contains only locally established facts for one behavior.
// A file name, a graph node, or an unexecuted test does not establish these facts.
// No text, paths, identifiers, logs, or arbitrary JSON can be stored here.
type RequirementSignals struct {
	ImplementationVerified  bool
	ConnectionVerified      bool
	BehaviorPassed          bool
	MissingBehaviorVerified bool
	Conflicting             bool
}

// EvaluatorInput is a sealed snapshot. Its zero value is invalid. Only bounded
// booleans and generated ordinal IDs cross this boundary; requirement prose and
// all collector text stay local. JSON unmarshalling cannot populate this type.
type EvaluatorInput struct {
	signals  []RequirementSignals
	complete bool
}

func NewEvaluatorInput(signals []RequirementSignals, complete bool) (EvaluatorInput, error) {
	if len(signals) == 0 || len(signals) > maxRequirements {
		return EvaluatorInput{}, errors.New("audit requires between 1 and 50 requirement signals")
	}
	return EvaluatorInput{signals: append([]RequirementSignals(nil), signals...), complete: complete}, nil
}

func (input EvaluatorInput) evidence() (EvidencePackage, error) {
	if len(input.signals) == 0 || len(input.signals) > maxRequirements {
		return EvidencePackage{}, errors.New("invalid sealed evaluator input")
	}
	status := ContextComplete
	if !input.complete {
		status = ContextIncomplete
	}
	e := EvidencePackage{Context: ContextEvidence{Status: status}}
	for i, s := range input.signals {
		id := fmt.Sprintf("R%d", i+1)
		e.Requirements = append(e.Requirements, AtomicRequirement{ID: id, Requirement: SanitizedText("Verify behavior for " + id)})
		if s.Conflicting {
			e.Context.Status = ContextIncomplete
		}
		if s.ImplementationVerified && s.ConnectionVerified {
			e.StructuralEvidence = append(e.StructuralEvidence, StructuralEvidence{RequirementID: id, Kind: "verified_connection", Observation: "Local verification established implementation and connection."})
		}
		if s.BehaviorPassed {
			e.TestEvidence = append(e.TestEvidence, TestEvidence{RequirementID: id, Name: SanitizedText("verification-" + id), Result: "passed", Provenance: "local behavior verification"})
		}
		if s.MissingBehaviorVerified {
			e.StructuralEvidence = append(e.StructuralEvidence, StructuralEvidence{RequirementID: id, Kind: "verified_missing_behavior", Observation: "Local verification established missing behavior."})
		}
	}
	return e, nil
}

func (input EvaluatorInput) status(i int) Status {
	for _, signal := range input.signals {
		if signal.Conflicting {
			return StatusUncertain
		}
	}
	s := input.signals[i]
	if !input.complete || s.Conflicting {
		return StatusUncertain
	}
	if s.MissingBehaviorVerified {
		if s.BehaviorPassed {
			return StatusUncertain
		}
		return StatusIncomplete
	}
	if s.ImplementationVerified && s.ConnectionVerified && s.BehaviorPassed {
		return StatusImplemented
	}
	return StatusUncertain
}

// ValidateResult prevents providers (including injected implementations) from
// inventing requirements, weakening them, or claiming unsupported completion.
func (input EvaluatorInput) ValidateResult(audit Audit) error {
	if len(audit.Requirements) != len(input.signals) {
		return errors.New("audit changed the requirement count")
	}
	for i, r := range audit.Requirements {
		if r.ID != fmt.Sprintf("R%d", i+1) || r.Status != input.status(i) {
			return errors.New("audit conclusion does not match supplied requirement evidence")
		}
		if r.Status == StatusUncertain && r.Confidence > 0.25 {
			return errors.New("uncertain audit confidence exceeds 0.25")
		}
	}
	return nil
}
