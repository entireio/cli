package intentlens

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

type Status string

const (
	StatusImplemented Status = "IMPLEMENTED"
	StatusIncomplete  Status = "INCOMPLETE"
	StatusUncertain   Status = "UNCERTAIN"
)

type EvidenceType string

const (
	EvidenceCheckpoint EvidenceType = "checkpoint"
	EvidenceCode       EvidenceType = "code"
	EvidenceGitDiff    EvidenceType = "git_diff"
	EvidenceGraph      EvidenceType = "graph"
	EvidenceTest       EvidenceType = "test"
)

type Audit struct {
	Summary      string        `json:"summary"`
	Requirements []Requirement `json:"requirements"`
}

type Requirement struct {
	ID             string     `json:"id"`
	Requirement    string     `json:"requirement"`
	Status         Status     `json:"status"`
	Confidence     float64    `json:"confidence"`
	Evidence       []Evidence `json:"evidence"`
	Recommendation string     `json:"recommendation"`
}

type Evidence struct {
	Type        EvidenceType `json:"type"`
	Explanation string       `json:"explanation"`
	File        string       `json:"file,omitempty"`
	Symbol      string       `json:"symbol,omitempty"`
	Reference   string       `json:"reference,omitempty"`
	TestName    string       `json:"test_name,omitempty"`
	Result      string       `json:"result,omitempty"`
}

var (
	//go:embed audit.schema.json
	auditSchema []byte

	//go:embed testdata/expected-audit.json
	demoAudit []byte

	ErrEmptyAudit = errors.New("audit result is empty")
	requirementID = regexp.MustCompile("^R[1-9][0-9]*$")
)

func Schema() []byte { return bytes.Clone(auditSchema) }

func DemoAuditJSON() []byte { return bytes.Clone(demoAudit) }

func ParseAuditJSON(data []byte) (Audit, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Audit{}, ErrEmptyAudit
	}
	var audit Audit
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&audit); err != nil {
		return Audit{}, fmt.Errorf("malformed audit JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Audit{}, err
	}
	if err := validateSchemaShape(data, audit); err != nil {
		return Audit{}, fmt.Errorf("audit schema validation: %w", err)
	}
	if err := ValidateSemantics(audit); err != nil {
		return Audit{}, fmt.Errorf("audit semantic validation: %w", err)
	}
	return audit, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("malformed audit JSON: %w", err)
	}
	return errors.New("malformed audit JSON: multiple JSON values")
}

func validateSchemaShape(data []byte, audit Audit) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if err := requireKeys("audit", raw, "summary", "requirements"); err != nil {
		return err
	}
	if strings.TrimSpace(audit.Summary) == "" {
		return errors.New("summary must not be empty")
	}
	if len(audit.Requirements) == 0 {
		return errors.New("requirements must be a non-empty array")
	}
	var rawRequirements []map[string]json.RawMessage
	if err := json.Unmarshal(raw["requirements"], &rawRequirements); err != nil {
		return errors.New("requirements must be an array")
	}
	for i, requirement := range audit.Requirements {
		label := fmt.Sprintf("requirements[%d]", i)
		if err := requireKeys(label, rawRequirements[i], "id", "requirement", "status", "confidence", "evidence", "recommendation"); err != nil {
			return err
		}
		if !requirementID.MatchString(requirement.ID) {
			return fmt.Errorf("%s.id must match R1, R2, ...", label)
		}
		if strings.TrimSpace(requirement.Requirement) == "" {
			return fmt.Errorf("%s.requirement must not be empty", label)
		}
		if requirement.Confidence < 0 || requirement.Confidence > 1 {
			return fmt.Errorf("%s.confidence must be between 0 and 1", label)
		}
		if len(requirement.Evidence) == 0 {
			return fmt.Errorf("%s.evidence must be a non-empty array", label)
		}
		var rawEvidence []map[string]json.RawMessage
		if err := json.Unmarshal(rawRequirements[i]["evidence"], &rawEvidence); err != nil {
			return fmt.Errorf("%s.evidence must be an array", label)
		}
		for j, evidence := range requirement.Evidence {
			evidenceLabel := fmt.Sprintf("%s (%s).evidence[%d]", label, requirement.ID, j)
			if err := requireKeys(evidenceLabel, rawEvidence[j], "type", "explanation"); err != nil {
				return err
			}
			if err := validateOptionalNonEmptyStrings(evidenceLabel, rawEvidence[j], "file", "symbol", "reference", "test_name", "result"); err != nil {
				return err
			}
			if !validEvidenceType(evidence.Type) {
				return fmt.Errorf("%s.type %q is not allowed", evidenceLabel, evidence.Type)
			}
			if strings.TrimSpace(evidence.Explanation) == "" {
				return fmt.Errorf("%s.explanation must not be empty", evidenceLabel)
			}
		}
	}
	return nil
}

func validateOptionalNonEmptyStrings(label string, object map[string]json.RawMessage, keys ...string) error {
	for _, key := range keys {
		value, ok := object[key]
		if !ok {
			continue
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil || strings.TrimSpace(text) == "" {
			return fmt.Errorf("%s.%s must be a non-empty string when present", label, key)
		}
	}
	return nil
}

func requireKeys(label string, object map[string]json.RawMessage, keys ...string) error {
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			return fmt.Errorf("%s is missing required property %q", label, key)
		}
	}
	return nil
}

func validEvidenceType(value EvidenceType) bool {
	switch value {
	case EvidenceCheckpoint, EvidenceCode, EvidenceGitDiff, EvidenceGraph, EvidenceTest:
		return true
	default:
		return false
	}
}

func ValidateSemantics(audit Audit) error {
	seen := make(map[string]struct{}, len(audit.Requirements))
	for _, requirement := range audit.Requirements {
		if _, ok := seen[requirement.ID]; ok {
			return fmt.Errorf("requirement ID %q is duplicated", requirement.ID)
		}
		seen[requirement.ID] = struct{}{}
		for i, evidence := range requirement.Evidence {
			if !validEvidenceType(evidence.Type) {
				return fmt.Errorf("%s evidence[%d]: type %q is not allowed", requirement.ID, i, evidence.Type)
			}
		}

		switch requirement.Status {
		case StatusImplemented:
			if strings.TrimSpace(requirement.Recommendation) != "" {
				return fmt.Errorf("%s: IMPLEMENTED recommendation must be empty", requirement.ID)
			}
			if !hasImplementationEvidence(requirement.Evidence) || !hasPassingTest(requirement.Evidence) {
				return fmt.Errorf("%s: IMPLEMENTED requires implementation evidence and a passing relevant test", requirement.ID)
			}
		case StatusIncomplete, StatusUncertain:
			if strings.TrimSpace(requirement.Recommendation) == "" {
				return fmt.Errorf("%s: %s requires an actionable recommendation", requirement.ID, requirement.Status)
			}
		default:
			return fmt.Errorf("%s: status %q is not allowed", requirement.ID, requirement.Status)
		}
	}
	return nil
}

func hasImplementationEvidence(evidence []Evidence) bool {
	for _, item := range evidence {
		if item.Type == EvidenceCode || item.Type == EvidenceGitDiff || item.Type == EvidenceGraph {
			return true
		}
	}
	return false
}

func hasPassingTest(evidence []Evidence) bool {
	for _, item := range evidence {
		if item.Type != EvidenceTest {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(item.Result)) {
		case "pass", "passed", "success", "succeeded":
			return true
		}
	}
	return false
}
