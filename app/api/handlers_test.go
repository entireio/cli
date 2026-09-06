package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/entireio/cli/app/api"
	"github.com/entireio/cli/app/models"
)

func TestHealthHandler(t *testing.T) {
	handler := api.NewAPIHandler(nil)
	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()

	handler.HealthHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", w.Code)
	}
}

func TestCommitsHandler_List(t *testing.T) {
	handler := api.NewAPIHandler(nil)
	req := httptest.NewRequest("GET", "/api/repositories/repo-cli-btw/commits", nil)
	w := httptest.NewRecorder()

	handler.RepositoriesHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", w.Code)
	}

	var commits []models.Commit
	if err := json.Unmarshal(w.Body.Bytes(), &commits); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if len(commits) == 0 {
		t.Fatalf("Expected non-empty list of commits")
	}
}

func TestCommitsHandler_Context_Available(t *testing.T) {
	handler := api.NewAPIHandler(nil)
	req := httptest.NewRequest("GET", "/api/repositories/repo-cli-btw/commits/3dbdf8b83c39/context", nil)
	w := httptest.NewRecorder()

	handler.RepositoriesHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", w.Code)
	}

	var devCtx models.CommitDevelopmentContext
	if err := json.Unmarshal(w.Body.Bytes(), &devCtx); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if devCtx.CheckpointStatus != models.CheckpointAvailable {
		t.Errorf("Expected CheckpointAvailable, got %s", devCtx.CheckpointStatus)
	}

	if !devCtx.HasCheckpoint {
		t.Errorf("Expected HasCheckpoint to be true")
	}
}

func TestCommitsHandler_Context_Unavailable(t *testing.T) {
	handler := api.NewAPIHandler(nil)
	req := httptest.NewRequest("GET", "/api/repositories/repo-cli-btw/commits/a1b2c3d4e5f6/context", nil)
	w := httptest.NewRecorder()

	handler.RepositoriesHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", w.Code)
	}

	var devCtx models.CommitDevelopmentContext
	if err := json.Unmarshal(w.Body.Bytes(), &devCtx); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if devCtx.CheckpointStatus != models.CheckpointUnavailable {
		t.Errorf("Expected CheckpointUnavailable, got %s", devCtx.CheckpointStatus)
	}

	if devCtx.HasCheckpoint {
		t.Errorf("Expected HasCheckpoint to be false")
	}

	if devCtx.MissingContextReason == "" {
		t.Errorf("Expected explicit missing context reason")
	}
}
