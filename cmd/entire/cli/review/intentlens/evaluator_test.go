package intentlens

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeGeminiTransport struct {
	response []byte
	err      error
	called   bool
	prompt   string
}

func (t *fakeGeminiTransport) Generate(_ context.Context, _ string, prompt string, _ json.RawMessage) ([]byte, error) {
	t.called = true
	t.prompt = prompt
	return t.response, t.err
}

func TestGeminiEvaluatorValidatesFakeResponse(t *testing.T) {
	transport := &fakeGeminiTransport{response: DemoAuditJSON()}
	evaluator := NewGeminiEvaluator(transport)
	t.Setenv("GEMINI_API_KEY", "set")

	result, err := evaluator.Evaluate(context.Background(), EvidencePackage(`{"checkpoint_evidence":[]}`))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !transport.called {
		t.Fatal("fake transport was not called")
	}
	if !strings.Contains(transport.prompt, "split it into atomic") {
		t.Fatalf("prompt did not use checkpoint audit contract: %q", transport.prompt)
	}
	if _, err := ParseAuditJSON(result); err != nil {
		t.Fatalf("result is not validated audit JSON: %v", err)
	}
}

func TestGeminiEvaluatorRejectsInvalidResponseAndMissingKey(t *testing.T) {
	t.Run("invalid response", func(t *testing.T) {
		transport := &fakeGeminiTransport{response: []byte(`{"summary":"unsupported"}`)}
		t.Setenv("GEMINI_API_KEY", "set")
		_, err := NewGeminiEvaluator(transport).Evaluate(context.Background(), EvidencePackage(`{}`))
		if err == nil {
			t.Fatal("expected invalid audit response to fail")
		}
	})
	t.Run("missing key", func(t *testing.T) {
		transport := &fakeGeminiTransport{err: errors.New("must not be called")}
		t.Setenv("GEMINI_API_KEY", "")
		_, err := NewGeminiEvaluator(transport).Evaluate(context.Background(), EvidencePackage(`{}`))
		if err == nil || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
			t.Fatalf("missing-key error = %v", err)
		}
		if transport.called {
			t.Fatal("transport called without runtime key")
		}
	})
}
