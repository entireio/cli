package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAddRepositoryAPI(t *testing.T) {
	handler := NewAPIHandler(nil)

	// Valid repository addition
	payload := map[string]string{
		"url":        "https://github.com/entireio/cli",
		"local_path": ".",
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest("POST", "/api/repositories", bytes.NewBuffer(body))
	rec := httptest.NewRecorder()

	handler.RepositoriesHandler(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected HTTP 201 Created, got %d. Body: %s", rec.Code, rec.Body.String())
	}

	// Invalid URL error handling
	invalidPayload := map[string]string{
		"url": "not-a-valid-url",
	}
	invalidBody, _ := json.Marshal(invalidPayload)

	reqErr := httptest.NewRequest("POST", "/api/repositories", bytes.NewBuffer(invalidBody))
	recErr := httptest.NewRecorder()

	handler.RepositoriesHandler(recErr, reqErr)

	if recErr.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400 Bad Request, got %d", recErr.Code)
	}

	var errResp APIErrorResponse
	json.Unmarshal(recErr.Body.Bytes(), &errResp)
	if errResp.Error.Code != "INVALID_REPOSITORY_URL" {
		t.Errorf("expected INVALID_REPOSITORY_URL, got %s", errResp.Error.Code)
	}
}

func TestSelectAndActiveRepositoryAPI(t *testing.T) {
	handler := NewAPIHandler(nil)

	// Get Active repository
	reqActive := httptest.NewRequest("GET", "/api/repositories/active", nil)
	recActive := httptest.NewRecorder()

	handler.RepositoriesHandler(recActive, reqActive)

	if recActive.Code != http.StatusOK {
		t.Errorf("expected 200 OK for active repo, got %d", recActive.Code)
	}

	// Add second repository and select it
	addPayload := map[string]string{
		"url": "https://github.com/entireio/tap",
	}
	addBody, _ := json.Marshal(addPayload)

	reqAdd := httptest.NewRequest("POST", "/api/repositories", bytes.NewBuffer(addBody))
	recAdd := httptest.NewRecorder()
	handler.RepositoriesHandler(recAdd, reqAdd)

	var newRepo map[string]interface{}
	json.Unmarshal(recAdd.Body.Bytes(), &newRepo)
	newRepoID := newRepo["id"].(string)

	// Select repository
	reqSelect := httptest.NewRequest("POST", "/api/repositories/"+newRepoID+"/select", nil)
	recSelect := httptest.NewRecorder()
	handler.RepositoriesHandler(recSelect, reqSelect)

	if recSelect.Code != http.StatusOK {
		t.Errorf("expected 200 OK selecting repo, got %d", recSelect.Code)
	}

	// Verify status endpoint
	reqStatus := httptest.NewRequest("GET", "/api/repositories/"+newRepoID+"/status", nil)
	recStatus := httptest.NewRecorder()
	handler.RepositoriesHandler(recStatus, reqStatus)

	if recStatus.Code != http.StatusOK {
		t.Errorf("expected 200 OK for repo status, got %d", recStatus.Code)
	}
}
