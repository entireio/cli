package summarize

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// openAITestGenerator returns an OpenAIGenerator wired to the given test server.
func openAITestGenerator(t *testing.T, srv *httptest.Server, apiKey string) *OpenAIGenerator {
	t.Helper()
	return &OpenAIGenerator{
		APIKey:   apiKey,
		endpoint: srv.URL,
	}
}

// openAISuccessHandler returns an HTTP handler that responds with a valid summary JSON.
func openAISuccessHandler(t *testing.T, intent, outcome string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		body, err := json.Marshal(map[string]any{
			"intent":     intent,
			"outcome":    outcome,
			"learnings":  map[string]any{"repo": []any{}, "code": []any{}, "workflow": []any{}},
			"friction":   []any{},
			"open_items": []any{},
		})
		if err != nil {
			t.Errorf("failed to marshal summary: %v", err)
		}
		resp := openAIResponse{Choices: []openAIChoice{{Message: openAIMessage{Content: string(body)}}}}
		w.Header().Set("Content-Type", "application/json")
		if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
			t.Errorf("failed to encode response: %v", encErr)
		}
	}
}

func TestOpenAIGenerator_MissingAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	gen := &OpenAIGenerator{}
	input := Input{Transcript: []Entry{{Type: EntryTypeUser, Content: "Hello"}}}
	_, err := gen.Generate(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Errorf("expected API key error, got: %v", err)
	}
}

func TestOpenAIGenerator_EnvAPIKey(t *testing.T) {
	srv := httptest.NewServer(openAISuccessHandler(t, "from env key", "ok"))
	defer srv.Close()

	t.Setenv("OPENAI_API_KEY", "sk-env-test")
	gen := &OpenAIGenerator{endpoint: srv.URL}

	input := Input{Transcript: []Entry{{Type: EntryTypeUser, Content: "Hello"}}}
	summary, err := gen.Generate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.Intent != "from env key" {
		t.Errorf("unexpected intent: %s", summary.Intent)
	}
}

func TestOpenAIGenerator_SuccessResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		openAISuccessHandler(t, "test intent", "test outcome")(w, r)
	}))
	defer srv.Close()

	gen := openAITestGenerator(t, srv, "sk-test")
	input := Input{Transcript: []Entry{{Type: EntryTypeUser, Content: "Help me"}}}
	summary, err := gen.Generate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.Intent != "test intent" {
		t.Errorf("unexpected intent: %s", summary.Intent)
	}
	if summary.Outcome != "test outcome" {
		t.Errorf("unexpected outcome: %s", summary.Outcome)
	}
}

func TestOpenAIGenerator_APIError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		resp := openAIResponse{Error: &openAIError{Message: "invalid API key", Type: "invalid_request_error"}}
		if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
			t.Errorf("failed to encode response: %v", encErr)
		}
	}))
	defer srv.Close()

	gen := openAITestGenerator(t, srv, "sk-bad")
	input := Input{Transcript: []Entry{{Type: EntryTypeUser, Content: "Hello"}}}
	_, err := gen.Generate(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for API error response")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("expected API error message, got: %v", err)
	}
}

func TestOpenAIGenerator_NoChoices(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := openAIResponse{Choices: []openAIChoice{}}
		w.Header().Set("Content-Type", "application/json")
		if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
			t.Errorf("failed to encode response: %v", encErr)
		}
	}))
	defer srv.Close()

	gen := openAITestGenerator(t, srv, "sk-test")
	input := Input{Transcript: []Entry{{Type: EntryTypeUser, Content: "Hello"}}}
	_, err := gen.Generate(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Errorf("expected 'no choices' error, got: %v", err)
	}
}

func TestOpenAIGenerator_InvalidSummaryJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := openAIResponse{Choices: []openAIChoice{{Message: openAIMessage{Content: "not valid json"}}}}
		w.Header().Set("Content-Type", "application/json")
		if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
			t.Errorf("failed to encode response: %v", encErr)
		}
	}))
	defer srv.Close()

	gen := openAITestGenerator(t, srv, "sk-test")
	input := Input{Transcript: []Entry{{Type: EntryTypeUser, Content: "Hello"}}}
	_, err := gen.Generate(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for invalid summary JSON")
	}
	if !strings.Contains(err.Error(), "parse summary JSON") {
		t.Errorf("expected parse error, got: %v", err)
	}
}

func TestOpenAIGenerator_DefaultModel(t *testing.T) {
	t.Parallel()
	var capturedModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openAIRequest
		if decErr := json.NewDecoder(r.Body).Decode(&req); decErr == nil {
			capturedModel = req.Model
		}
		openAISuccessHandler(t, "ok", "ok")(w, r)
	}))
	defer srv.Close()

	gen := &OpenAIGenerator{APIKey: "sk-test", endpoint: srv.URL}
	input := Input{Transcript: []Entry{{Type: EntryTypeUser, Content: "Hello"}}}
	_, err := gen.Generate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedModel != openAIDefaultModel {
		t.Errorf("expected default model %s, got %s", openAIDefaultModel, capturedModel)
	}
}

func TestOpenAIGenerator_MarkdownCodeBlock(t *testing.T) {
	t.Parallel()
	summaryContent := "```json\n{\"intent\":\"md intent\",\"outcome\":\"md outcome\",\"learnings\":{\"repo\":[],\"code\":[],\"workflow\":[]},\"friction\":[],\"open_items\":[]}\n```"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := openAIResponse{Choices: []openAIChoice{{Message: openAIMessage{Content: summaryContent}}}}
		w.Header().Set("Content-Type", "application/json")
		if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
			t.Errorf("failed to encode response: %v", encErr)
		}
	}))
	defer srv.Close()

	gen := openAITestGenerator(t, srv, "sk-test")
	input := Input{Transcript: []Entry{{Type: EntryTypeUser, Content: "Hello"}}}
	summary, err := gen.Generate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.Intent != "md intent" {
		t.Errorf("unexpected intent: %s", summary.Intent)
	}
}
