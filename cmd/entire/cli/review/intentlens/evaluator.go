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
)

// EvidencePackage is the bounded, collector-produced JSON supplied to an
// evaluator. It must contain only evidence the evaluator may rely on.
type EvidencePackage json.RawMessage

// Evaluator converts a collected evidence package into validated audit JSON.
type Evaluator interface {
	Evaluate(ctx context.Context, evidence EvidencePackage) (json.RawMessage, error)
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
		transport = httpGeminiTransport{client: http.DefaultClient}
	}
	return &GeminiEvaluator{transport: transport}
}

func (e *GeminiEvaluator) Evaluate(ctx context.Context, evidence EvidencePackage) (json.RawMessage, error) {
	if !json.Valid(evidence) {
		return nil, errors.New("IntentLens evidence package must be valid JSON")
	}
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return nil, errors.New("GEMINI_API_KEY is required to audit a checkpoint")
	}
	response, err := e.transport.Generate(ctx, apiKey, CheckpointAuditPrompt(evidence), Schema())
	if err != nil {
		return nil, fmt.Errorf("generate IntentLens audit: %w", err)
	}
	auditJSON, err := extractGeminiAuditJSON(response)
	if err != nil {
		return nil, err
	}
	audit, err := ParseAuditJSON(auditJSON)
	if err != nil {
		return nil, err
	}
	validated, err := json.Marshal(audit)
	if err != nil {
		return nil, fmt.Errorf("encode validated audit: %w", err)
	}
	return validated, nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent", bytes.NewReader(body))
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
	result, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read Gemini response: %w", err)
	}
	return result, nil
}

