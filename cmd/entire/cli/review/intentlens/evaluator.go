package intentlens

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

type ContextStatus string

const (
	ContextComplete   ContextStatus = "COMPLETE"
	ContextIncomplete ContextStatus = "INCOMPLETE"
)

const (
	maxRequirements       = 50
	maxChangedFiles       = 100
	maxStructuralEvidence = 100
	maxGraphEvidence      = 100
	maxTestEvidence       = 100
	maxPromptTextRunes    = 600
	maxPromptPathRunes    = 240
	redactedIntentText    = "[REDACTED INTENT]"
)

// SanitizedText is bounded, best-effort redacted local display text. It is not
// safe provider input; only EvaluatorInput establishes that boundary.
type SanitizedText string

// SanitizedPath is a changed file path, not file content or a patch.
type SanitizedPath string

type ContextEvidence struct {
	Status ContextStatus `json:"status"`
	Note   SanitizedText `json:"note,omitempty"`
}

type AtomicRequirement struct {
	ID             string        `json:"id"`
	Requirement    SanitizedText `json:"requirement"`
	IntentRedacted bool          `json:"intent_redacted,omitempty"`
}

type ChangedFileEvidence struct {
	Path SanitizedPath `json:"path"`
}

type StructuralEvidence struct {
	RequirementID string        `json:"requirement_id,omitempty"`
	Kind          SanitizedText `json:"kind"`
	Path          SanitizedPath `json:"path,omitempty"`
	Symbol        SanitizedText `json:"symbol,omitempty"`
	Observation   SanitizedText `json:"observation"`
}

type GraphEvidence struct {
	RequirementID string        `json:"requirement_id,omitempty"`
	Source        SanitizedText `json:"source"`
	Relation      SanitizedText `json:"relation"`
	Target        SanitizedText `json:"target"`
	Path          SanitizedPath `json:"path,omitempty"`
}

type TestEvidence struct {
	RequirementID string        `json:"requirement_id,omitempty"`
	Name          SanitizedText `json:"name"`
	Result        SanitizedText `json:"result"`
	Command       SanitizedText `json:"command,omitempty"`
	Summary       SanitizedText `json:"summary,omitempty"`
	Provenance    SanitizedText `json:"provenance"`
}

// EvidencePackage holds local display metadata. Its text wrappers are NOT a
// privacy boundary. Evaluator accepts only the structurally sealed EvaluatorInput.
type EvidencePackage struct {
	Context            ContextEvidence       `json:"context"`
	Requirements       []AtomicRequirement   `json:"requirements"`
	ChangedFiles       []ChangedFileEvidence `json:"changed_files,omitempty"`
	StructuralEvidence []StructuralEvidence  `json:"structural_evidence,omitempty"`
	GraphEvidence      []GraphEvidence       `json:"graph_evidence,omitempty"`
	TestEvidence       []TestEvidence        `json:"test_evidence,omitempty"`
}

var (
	secretAssignmentPattern = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:secret|token|api[_-]?key|password)[a-z0-9_.-]*)\s*[:=]\s*("[^"]*"|'[^']*'|[^,\s]+)`)
	secretValuePattern      = regexp.MustCompile(`(?i)\b(?:sk-[a-z0-9_-]{8,}|gh[pousr]_[a-z0-9_]{8,}|AIza[0-9a-z_-]{10,})\b`)
	whitespacePattern       = regexp.MustCompile(`\s+`)
)

func AtomicRequirementFromText(id string, requirement string) AtomicRequirement {
	text, redacted := sanitizeTextWithReport(requirement)
	if strings.TrimSpace(text) == "" {
		text = redactedIntentText
		redacted = true
	}
	return AtomicRequirement{
		ID:             id,
		Requirement:    SanitizedText(text),
		IntentRedacted: redacted,
	}
}

// Evaluator converts a collected evidence package into validated audit JSON.
type Evaluator interface {
	Evaluate(ctx context.Context, evidence EvaluatorInput) (json.RawMessage, error)
}

// GeminiTransport isolates Gemini I/O so tests never use the network.
type GeminiTransport interface {
	Generate(ctx context.Context, apiKey string, prompt string, schema json.RawMessage) ([]byte, error)
}

// GeminiEvaluator evaluates evidence with Gemini. It reads GEMINI_API_KEY only
// when Evaluate is called, never during command construction.
type GeminiEvaluator struct {
	transport GeminiTransport
}

func NewGeminiEvaluator(transport GeminiTransport) *GeminiEvaluator {
	if transport == nil {
		transport = httpGeminiTransport{client: &http.Client{
			Timeout: 2 * time.Minute,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}}
	}
	return &GeminiEvaluator{transport: transport}
}

func (e *GeminiEvaluator) Evaluate(ctx context.Context, input EvaluatorInput) (json.RawMessage, error) {
	evidence, err := input.evidence()
	if err != nil {
		return nil, err
	}
	if err := evidence.validate(); err != nil {
		return nil, err
	}
	allUncertain := true
	for i := range input.signals {
		if input.status(i) != StatusUncertain {
			allUncertain = false
		}
	}
	if allUncertain {
		return evidence.conservativeAuditJSON()
	}
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return nil, errors.New("GEMINI_API_KEY is required to audit a checkpoint")
	}
	response, err := e.transport.Generate(ctx, apiKey, CheckpointAuditPrompt(input), Schema())
	if err != nil {
		return nil, errors.New("generate IntentLens audit: provider request failed")
	}
	auditJSON, err := extractGeminiAuditJSON(response)
	if err != nil {
		return nil, err
	}
	audit, err := ParseAuditJSON(auditJSON)
	if err != nil {
		return nil, err
	}
	if err := input.ValidateResult(audit); err != nil {
		return nil, err
	}
	validated, err := json.Marshal(audit)
	if err != nil {
		return nil, fmt.Errorf("encode validated audit: %w", err)
	}
	return validated, nil
}

func (e EvidencePackage) validate() error {
	switch e.Context.Status {
	case ContextComplete, ContextIncomplete:
	default:
		return fmt.Errorf("sanitized evidence context status must be %s or %s", ContextComplete, ContextIncomplete)
	}
	if len(e.Requirements) == 0 {
		return errors.New("sanitized evidence package requires at least one atomic requirement")
	}
	if err := validateMax("requirements", len(e.Requirements), maxRequirements); err != nil {
		return err
	}
	if err := validateMax("changed files", len(e.ChangedFiles), maxChangedFiles); err != nil {
		return err
	}
	if err := validateMax("structural evidence", len(e.StructuralEvidence), maxStructuralEvidence); err != nil {
		return err
	}
	if err := validateMax("graph evidence", len(e.GraphEvidence), maxGraphEvidence); err != nil {
		return err
	}
	if err := validateMax("test evidence", len(e.TestEvidence), maxTestEvidence); err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(e.Requirements))
	for i, requirement := range e.Requirements {
		label := fmt.Sprintf("requirements[%d]", i)
		if !requirementID.MatchString(requirement.ID) {
			return fmt.Errorf("%s.id must match R1, R2, ...", label)
		}
		if _, ok := seen[requirement.ID]; ok {
			return fmt.Errorf("requirement ID %q is duplicated", requirement.ID)
		}
		seen[requirement.ID] = struct{}{}
		if !requirement.IntentRedacted && strings.TrimSpace(sanitizeText(string(requirement.Requirement))) == "" {
			return fmt.Errorf("%s.requirement must not be empty", label)
		}
	}
	for i, file := range e.ChangedFiles {
		if strings.TrimSpace(sanitizePath(string(file.Path))) == "" {
			return fmt.Errorf("changed_files[%d].path must not be empty", i)
		}
	}
	for i, item := range e.StructuralEvidence {
		if strings.TrimSpace(sanitizeText(string(item.Kind))) == "" {
			return fmt.Errorf("structural_evidence[%d].kind must not be empty", i)
		}
		if strings.TrimSpace(sanitizeText(string(item.Observation))) == "" {
			return fmt.Errorf("structural_evidence[%d].observation must not be empty", i)
		}
		if item.RequirementID != "" && !requirementID.MatchString(item.RequirementID) {
			return fmt.Errorf("structural_evidence[%d].requirement_id must match R1, R2, ...", i)
		}
	}
	for i, item := range e.GraphEvidence {
		if strings.TrimSpace(sanitizeText(string(item.Source))) == "" {
			return fmt.Errorf("graph_evidence[%d].source must not be empty", i)
		}
		if strings.TrimSpace(sanitizeText(string(item.Relation))) == "" {
			return fmt.Errorf("graph_evidence[%d].relation must not be empty", i)
		}
		if strings.TrimSpace(sanitizeText(string(item.Target))) == "" {
			return fmt.Errorf("graph_evidence[%d].target must not be empty", i)
		}
		if item.RequirementID != "" && !requirementID.MatchString(item.RequirementID) {
			return fmt.Errorf("graph_evidence[%d].requirement_id must match R1, R2, ...", i)
		}
	}
	for i, item := range e.TestEvidence {
		if strings.TrimSpace(sanitizeText(string(item.Name))) == "" {
			return fmt.Errorf("test_evidence[%d].name must not be empty", i)
		}
		if strings.TrimSpace(sanitizeText(string(item.Result))) == "" {
			return fmt.Errorf("test_evidence[%d].result must not be empty", i)
		}
		if strings.TrimSpace(sanitizeText(string(item.Provenance))) == "" {
			return fmt.Errorf("test_evidence[%d].provenance must not be empty", i)
		}
		if item.RequirementID != "" && !requirementID.MatchString(item.RequirementID) {
			return fmt.Errorf("test_evidence[%d].requirement_id must match R1, R2, ...", i)
		}
	}
	return nil
}

func validateMax(label string, got, max int) error {
	if got > max {
		return fmt.Errorf("%s exceeds evaluator bound: got %d, max %d", label, got, max)
	}
	return nil
}

func (e EvidencePackage) needsConservativeAudit() bool {
	if e.Context.Status == ContextIncomplete {
		return true
	}
	for _, requirement := range e.Requirements {
		if requirement.IntentRedacted {
			return true
		}
	}
	return false
}

func (e EvidencePackage) conservativeAuditJSON() (json.RawMessage, error) {
	reason := "Relevant implementation, connection, and behavior verification is unavailable or context is incomplete."
	reference := "context:" + string(e.Context.Status)
	if e.hasRedactedIntent() {
		reference += ";intent:redacted"
	}
	audit := Audit{
		Summary:      "Requirements need verified implementation and behavior evidence.",
		Requirements: make([]Requirement, 0, len(e.Requirements)),
	}
	for _, requirement := range e.Requirements {
		text := sanitizeText(string(requirement.Requirement))
		if requirement.IntentRedacted || strings.TrimSpace(text) == "" {
			text = redactedIntentText
		}
		audit.Requirements = append(audit.Requirements, Requirement{
			ID:          requirement.ID,
			Requirement: text,
			Status:      StatusUncertain,
			Confidence:  0.1,
			Evidence: []Evidence{{
				Type:        EvidenceCheckpoint,
				Reference:   reference,
				Explanation: reason,
			}},
			Recommendation: "Run the audit with complete sanitized context and non-redacted requirements before evaluating implementation.",
		})
	}
	data, err := json.Marshal(audit)
	if err != nil {
		return nil, fmt.Errorf("encode conservative audit: %w", err)
	}
	if _, err := ParseAuditJSON(data); err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func (e EvidencePackage) hasRedactedIntent() bool {
	for _, requirement := range e.Requirements {
		if requirement.IntentRedacted {
			return true
		}
	}
	return false
}

func (e EvidencePackage) sanitizedJSON() ([]byte, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	type promptContext struct {
		Status ContextStatus `json:"status"`
		Note   string        `json:"note,omitempty"`
	}
	type promptRequirement struct {
		ID             string `json:"id"`
		Requirement    string `json:"requirement"`
		IntentRedacted bool   `json:"intent_redacted,omitempty"`
	}
	type promptChangedFile struct {
		Path string `json:"path"`
	}
	type promptStructuralEvidence struct {
		RequirementID string `json:"requirement_id,omitempty"`
		Kind          string `json:"kind"`
		Path          string `json:"path,omitempty"`
		Symbol        string `json:"symbol,omitempty"`
		Observation   string `json:"observation"`
	}
	type promptGraphEvidence struct {
		RequirementID string `json:"requirement_id,omitempty"`
		Source        string `json:"source"`
		Relation      string `json:"relation"`
		Target        string `json:"target"`
		Path          string `json:"path,omitempty"`
	}
	type promptTestEvidence struct {
		RequirementID string `json:"requirement_id,omitempty"`
		Name          string `json:"name"`
		Result        string `json:"result"`
		Command       string `json:"command,omitempty"`
		Summary       string `json:"summary,omitempty"`
		Provenance    string `json:"provenance"`
	}
	payload := struct {
		Context            promptContext              `json:"context"`
		Requirements       []promptRequirement        `json:"requirements"`
		ChangedFiles       []promptChangedFile        `json:"changed_files,omitempty"`
		StructuralEvidence []promptStructuralEvidence `json:"structural_evidence,omitempty"`
		GraphEvidence      []promptGraphEvidence      `json:"graph_evidence,omitempty"`
		TestEvidence       []promptTestEvidence       `json:"test_evidence,omitempty"`
	}{
		Context: promptContext{
			Status: e.Context.Status,
			Note:   sanitizeText(string(e.Context.Note)),
		},
		Requirements: make([]promptRequirement, 0, len(e.Requirements)),
	}
	for _, requirement := range e.Requirements {
		text := sanitizeText(string(requirement.Requirement))
		if requirement.IntentRedacted {
			text = redactedIntentText
		}
		payload.Requirements = append(payload.Requirements, promptRequirement{
			ID:             requirement.ID,
			Requirement:    text,
			IntentRedacted: requirement.IntentRedacted,
		})
	}
	for _, file := range e.ChangedFiles {
		payload.ChangedFiles = append(payload.ChangedFiles, promptChangedFile{Path: sanitizePath(string(file.Path))})
	}
	for _, item := range e.StructuralEvidence {
		payload.StructuralEvidence = append(payload.StructuralEvidence, promptStructuralEvidence{
			RequirementID: item.RequirementID,
			Kind:          sanitizeText(string(item.Kind)),
			Path:          sanitizePath(string(item.Path)),
			Symbol:        sanitizeText(string(item.Symbol)),
			Observation:   sanitizeText(string(item.Observation)),
		})
	}
	for _, item := range e.GraphEvidence {
		payload.GraphEvidence = append(payload.GraphEvidence, promptGraphEvidence{
			RequirementID: item.RequirementID,
			Source:        sanitizeText(string(item.Source)),
			Relation:      sanitizeText(string(item.Relation)),
			Target:        sanitizeText(string(item.Target)),
			Path:          sanitizePath(string(item.Path)),
		})
	}
	for _, item := range e.TestEvidence {
		payload.TestEvidence = append(payload.TestEvidence, promptTestEvidence{
			RequirementID: item.RequirementID,
			Name:          sanitizeText(string(item.Name)),
			Result:        sanitizeText(string(item.Result)),
			Command:       sanitizeText(string(item.Command)),
			Summary:       sanitizeText(string(item.Summary)),
			Provenance:    sanitizeText(string(item.Provenance)),
		})
	}
	return json.Marshal(payload)
}

func sanitizedEvidenceForPrompt(evidence EvidencePackage) string {
	data, err := evidence.sanitizedJSON()
	if err == nil {
		return string(data)
	}
	fallback, marshalErr := json.Marshal(struct {
		ContractError string `json:"contract_error"`
	}{ContractError: sanitizeText(err.Error())})
	if marshalErr != nil {
		return `{"contract_error":"invalid sanitized evidence contract"}`
	}
	return string(fallback)
}

func sanitizeText(value string) string {
	sanitized, _ := sanitizeTextWithReport(value)
	return sanitized
}

func sanitizeTextWithReport(value string) (string, bool) {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.ReplaceAll(value, "\x00", "")
	redacted := false
	replaced := secretAssignmentPattern.ReplaceAllString(value, "${1}=[REDACTED]")
	if replaced != value {
		redacted = true
	}
	value = replaced
	replaced = secretValuePattern.ReplaceAllString(value, "[REDACTED_SECRET]")
	if replaced != value {
		redacted = true
	}
	value = replaced

	lines := strings.Split(value, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if unsafePromptLine(trimmed) {
			redacted = true
			continue
		}
		kept = append(kept, trimmed)
	}
	if redacted {
		kept = append(kept, "[REDACTED UNSAFE CONTENT]")
	}
	result := strings.TrimSpace(whitespacePattern.ReplaceAllString(strings.Join(kept, " "), " "))
	if result == "" && redacted {
		result = "[REDACTED UNSAFE CONTENT]"
	}
	return truncateRunes(result, maxPromptTextRunes), redacted
}

func sanitizePath(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.ReplaceAll(value, "\x00", "")
	value = strings.TrimSpace(value)
	if i := strings.IndexByte(value, '\n'); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	if value == "" {
		return ""
	}
	if unsafePromptLine(value) {
		return "[REDACTED_PATH]"
	}
	return truncateRunes(value, maxPromptPathRunes)
}

func unsafePromptLine(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(lower, "begin transcript"),
		strings.Contains(lower, "end transcript"),
		strings.Contains(lower, "session log"),
		strings.Contains(lower, "checkpoint prompt"),
		strings.Contains(lower, "raw checkpoint"):
		return true
	case strings.HasPrefix(lower, "user:"),
		strings.HasPrefix(lower, "assistant:"),
		strings.HasPrefix(lower, "system:"),
		strings.HasPrefix(lower, "tool:"):
		return true
	case strings.HasPrefix(value, "diff --git "),
		strings.HasPrefix(value, "@@ "),
		strings.HasPrefix(value, "index "),
		strings.HasPrefix(value, "+++ "),
		strings.HasPrefix(value, "--- "):
		return true
	case len(value) > 1 && (value[0] == '+' || value[0] == '-'):
		return true
	case strings.HasPrefix(value, "{") && strings.Contains(value, "\":"):
		return true
	case strings.HasPrefix(value, "[") && strings.Contains(value, "{"):
		return true
	default:
		return false
	}
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + " [truncated]"
}

func extractGeminiAuditJSON(response []byte) ([]byte, error) {
	if _, err := ParseAuditJSON(response); err == nil {
		return response, nil
	}
	var envelope struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return nil, fmt.Errorf("decode Gemini response: %w", err)
	}
	for _, candidate := range envelope.Candidates {
		for _, part := range candidate.Content.Parts {
			if json.Valid([]byte(part.Text)) {
				return []byte(part.Text), nil
			}
		}
	}
	return nil, errors.New("Gemini response did not contain audit JSON")
}

type httpGeminiTransport struct {
	client *http.Client
}

func (t httpGeminiTransport) Generate(ctx context.Context, apiKey string, prompt string, schema json.RawMessage) ([]byte, error) {
	body, err := json.Marshal(struct {
		Contents []struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
		GenerationConfig struct {
			ResponseMIMEType   string          `json:"responseMimeType"`
			ResponseJSONSchema json.RawMessage `json:"responseJsonSchema"`
		} `json:"generationConfig"`
	}{
		Contents: []struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}{{Parts: []struct {
			Text string `json:"text"`
		}{{Text: prompt}}}},
		GenerationConfig: struct {
			ResponseMIMEType   string          `json:"responseMimeType"`
			ResponseJSONSchema json.RawMessage `json:"responseJsonSchema"`
		}{ResponseMIMEType: "application/json", ResponseJSONSchema: schema},
	})
	if err != nil {
		return nil, fmt.Errorf("encode Gemini request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Gemini request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call Gemini: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("Gemini returned HTTP %d", resp.StatusCode)
	}
	const maxResponseBytes = 1 << 20
	result, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Gemini response: %w", err)
	}
	if len(result) > maxResponseBytes {
		return nil, errors.New("Gemini response exceeds size limit")
	}
	return result, nil
}
