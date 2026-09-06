package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthHandler(t *testing.T) {
	handler := NewAPIHandler(nil)
	req := httptest.NewRequest("GET", "/api/health", nil)
	rec := httptest.NewRecorder()

	handler.HealthHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected HTTP 200 OK, got %d", rec.Code)
	}

	var res map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("failed to decode json response: %v", err)
	}

	if res["status"] != "ok" {
		t.Errorf("expected status 'ok', got %v", res["status"])
	}
}

func TestRepositoriesHandler(t *testing.T) {
	handler := NewAPIHandler(nil)

	tests := []struct {
		name     string
		path     string
		wantCode int
	}{
		{"List Repositories", "/api/repositories", http.StatusOK},
		{"Single Repository", "/api/repositories/repo-123", http.StatusOK},
		{"Checkpoints Endpoint", "/api/repositories/repo-123/checkpoints", http.StatusOK},
		{"Requirements Endpoint", "/api/repositories/repo-123/requirements", http.StatusOK},
		{"Graph Endpoint", "/api/repositories/repo-123/graph", http.StatusOK},
		{"Handoff Endpoint", "/api/repositories/repo-123/handoff", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()

			handler.RepositoriesHandler(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("expected code %d for %s, got %d", tt.wantCode, tt.path, rec.Code)
			}
		})
	}
}

func TestAPIErrorFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteAPIError(rec, http.StatusBadRequest, "INVALID_INPUT", "Invalid parameter supplied")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}

	var errResp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse API error JSON: %v", err)
	}

	if errResp.Error.Code != "INVALID_INPUT" {
		t.Errorf("expected error code INVALID_INPUT, got %s", errResp.Error.Code)
	}
	if errResp.Error.Message != "Invalid parameter supplied" {
		t.Errorf("expected message 'Invalid parameter supplied', got %s", errResp.Error.Message)
	}
}
