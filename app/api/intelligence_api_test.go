package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIntelligenceAPI_GetCommitIntelligence(t *testing.T) {
	handler := NewAPIHandler(nil)

	// GET /api/repositories/repo-cli-btw/commits/3dbdf8b83c39/intelligence
	req := httptest.NewRequest("GET", "/api/repositories/repo-cli-btw/commits/3dbdf8b83c39/intelligence", nil)
	rec := httptest.NewRecorder()

	handler.RepositoriesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK, got %d. Body: %s", rec.Code, rec.Body.String())
	}

	var res map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if res["context_completeness"] != "COMPLETE" {
		t.Errorf("expected context_completeness COMPLETE, got %v", res["context_completeness"])
	}

	if res["verification_status"] != "COMPLETED" {
		t.Errorf("expected verification_status COMPLETED, got %v", res["verification_status"])
	}

	if res["intent"] == "" {
		t.Errorf("expected non-empty intent")
	}

	evidence, ok := res["evidence"].(map[string]interface{})
	if !ok || evidence["checkpoint"] == nil {
		t.Errorf("expected evidence matrix with checkpoint data")
	}
}

func TestIntelligenceAPI_GetGitOnlyIntelligence(t *testing.T) {
	handler := NewAPIHandler(nil)

	// Commit 'a1b2c3d4e5f6' has no Checkpoint
	req := httptest.NewRequest("GET", "/api/repositories/repo-cli-btw/commits/a1b2c3d4e5f6/intelligence", nil)
	rec := httptest.NewRecorder()

	handler.RepositoriesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK, got %d", rec.Code)
	}

	var res map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if res["context_completeness"] != "UNAVAILABLE" {
		t.Errorf("expected context_completeness UNAVAILABLE for Git-only commit, got %v", res["context_completeness"])
	}

	if res["verification_status"] != "NEEDS_VERIFICATION" {
		t.Errorf("expected verification_status NEEDS_VERIFICATION, got %v", res["verification_status"])
	}
}
