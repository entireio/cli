package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/entireio/cli/app/providers"
)

// APIErrorBody holds the structured error details.
type APIErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// APIErrorResponse defines the standardized API error payload format.
type APIErrorResponse struct {
	Error APIErrorBody `json:"error"`
}

// WriteAPIError responds with a standardized JSON API error.
func WriteAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIErrorResponse{
		Error: APIErrorBody{
			Code:    code,
			Message: message,
		},
	})
}

// ServerDependencies encapsulates all provider interfaces required by the API layer.
type ServerDependencies struct {
	CheckpointProvider providers.EntireCheckpointProvider
	GraphProvider      providers.EntireGraphProvider
	GitHubProvider     providers.GitHubProvider
	RepoAnalyzer       providers.RepositoryAnalyzer
	ReqAnalyzer        providers.RequirementAnalyzer
}

// DefaultServerDependencies instantiates the development dependencies.
func DefaultServerDependencies() *ServerDependencies {
	devAnalyzer := providers.NewDevAnalyzer()
	return &ServerDependencies{
		CheckpointProvider: providers.NewDevCheckpointProvider(),
		GraphProvider:      providers.NewDevGraphProvider(),
		GitHubProvider:     providers.NewDevGitHubProvider(),
		RepoAnalyzer:       providers.NewLiveRepositoryAnalyzer(),
		ReqAnalyzer:        devAnalyzer,
	}
}

type APIHandler struct {
	deps *ServerDependencies
}

func NewAPIHandler(deps *ServerDependencies) *APIHandler {
	if deps == nil {
		deps = DefaultServerDependencies()
	}
	return &APIHandler{deps: deps}
}

// HealthHandler returns API server health status.
func (h *APIHandler) HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "ok",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"version":   "1.0.0",
		"service":   "Entire Checkpoint Intelligence Foundation API",
	})
}

// RepositoriesHandler lists all tracked repositories or details for a single repository.
func (h *APIHandler) RepositoriesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/repositories")
	path = strings.Trim(path, "/")

	if path == "" {
		// GET /api/repositories
		repo, err := h.deps.RepoAnalyzer.AnalyzeRepository(r.Context(), ".", false)
		if err != nil {
			slog.Error("Failed to analyze repository", "error", err)
			WriteAPIError(w, http.StatusInternalServerError, "REPOSITORY_ANALYSIS_FAILED", err.Error())
			return
		}
		json.NewEncoder(w).Encode([]interface{}{repo})
		return
	}

	parts := strings.Split(path, "/")
	repoID := parts[0]

	if len(parts) == 1 {
		// GET /api/repositories/:id
		repo, err := h.deps.RepoAnalyzer.AnalyzeRepository(r.Context(), ".", false)
		if err != nil {
			slog.Error("Failed to fetch repository info", "repoID", repoID, "error", err)
			WriteAPIError(w, http.StatusNotFound, "REPOSITORY_NOT_FOUND", "Repository was not found")
			return
		}
		repo.ID = repoID
		json.NewEncoder(w).Encode(repo)
		return
	}

	subResource := parts[1]
	switch subResource {
	case "checkpoints":
		// GET /api/repositories/:id/checkpoints
		cps, err := h.deps.CheckpointProvider.GetCheckpoints(r.Context(), repoID)
		if err != nil {
			slog.Error("Failed to fetch checkpoints", "repoID", repoID, "error", err)
			WriteAPIError(w, http.StatusInternalServerError, "CHECKPOINT_FETCH_FAILED", err.Error())
			return
		}
		json.NewEncoder(w).Encode(cps)
	case "analyze":
		if r.Method == http.MethodPost {
			// POST /api/repositories/:id/analyze
			repo, err := h.deps.RepoAnalyzer.AnalyzeRepository(r.Context(), ".", true)
			if err != nil {
				slog.Error("Failed to force analyze repository", "repoID", repoID, "error", err)
				WriteAPIError(w, http.StatusInternalServerError, "REPOSITORY_ANALYSIS_FAILED", err.Error())
				return
			}
			json.NewEncoder(w).Encode(repo)
		} else {
			WriteAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only POST is allowed")
		}
	case "requirements":
		// GET /api/repositories/:id/requirements
		reqs, err := h.deps.ReqAnalyzer.AnalyzeRequirements(r.Context(), repoID)
		if err != nil {
			slog.Error("Failed to analyze requirements", "repoID", repoID, "error", err)
			WriteAPIError(w, http.StatusInternalServerError, "REQUIREMENTS_ANALYSIS_FAILED", err.Error())
			return
		}
		json.NewEncoder(w).Encode(reqs)
	case "graph":
		// GET /api/repositories/:id/graph
		findings, err := h.deps.GraphProvider.GetGraphFindings(r.Context(), repoID)
		if err != nil {
			slog.Error("Failed to fetch graph findings", "repoID", repoID, "error", err)
			WriteAPIError(w, http.StatusInternalServerError, "GRAPH_QUERY_FAILED", err.Error())
			return
		}
		json.NewEncoder(w).Encode(findings)
	case "handoff":
		// GET /api/repositories/:id/handoff
		handoff, err := h.deps.ReqAnalyzer.GetHandoff(r.Context(), repoID)
		if err != nil {
			slog.Error("Failed to generate handoff", "repoID", repoID, "error", err)
			WriteAPIError(w, http.StatusInternalServerError, "HANDOFF_GENERATION_FAILED", err.Error())
			return
		}
		json.NewEncoder(w).Encode(handoff)
	default:
		WriteAPIError(w, http.StatusNotFound, "ENDPOINT_NOT_FOUND", "The requested API endpoint was not found")
	}
}
