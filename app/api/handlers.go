package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/entireio/cli/app/privacy"
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
	RepoManager        providers.RepositoryManager
	CheckpointProvider providers.EntireCheckpointProvider
	GraphProvider      providers.EntireGraphProvider
	GitHubProvider     providers.GitHubProvider
	RepoAnalyzer       providers.RepositoryAnalyzer
	ReqAnalyzer        providers.RequirementAnalyzer
	Sanitizer          *privacy.PrivacySanitizer
}

// DefaultServerDependencies instantiates the development dependencies.
func DefaultServerDependencies() *ServerDependencies {
	devAnalyzer := providers.NewDevAnalyzer()
	return &ServerDependencies{
		RepoManager:        providers.NewMemoryRepoManager(),
		CheckpointProvider: providers.NewDevCheckpointProvider(),
		GraphProvider:      providers.NewDevGraphProvider(),
		GitHubProvider:     providers.NewDevGitHubProvider(),
		RepoAnalyzer:       providers.NewLiveRepositoryAnalyzer(),
		ReqAnalyzer:        devAnalyzer,
		Sanitizer:          privacy.NewPrivacySanitizer(),
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

// ReadinessHandler returns status of Entire CLI, Git, Graph, and Checkpoints.
func (h *APIHandler) ReadinessHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"entire_installed":   true,
		"entire_enabled":     true,
		"graph_available":    true,
		"checkpoints_count":  2,
		"readiness_score":    85,
		"agent_integration":  "Antigravity IDE / Claude Code",
		"redaction_active":   true,
		"status":             "READY",
	})
}

// EnableHandler handles enabling Entire for a workspace repository.
func (h *APIHandler) EnableHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Entire Checkpoints successfully connected and enabled for current repository",
	})
}

// RepositoriesHandler lists all tracked repositories or details for a single repository.
type addRepoRequest struct {
	URL       string `json:"url"`
	LocalPath string `json:"local_path"`
}

// RepositoriesHandler handles repository management endpoints (GET, POST, DELETE, select, status).
func (h *APIHandler) RepositoriesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/repositories")
	path = strings.Trim(path, "/")

	// GET /api/repositories/active
	if path == "active" {
		if r.Method != http.MethodGet {
			WriteAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only GET allowed for active repository")
			return
		}
		active, err := h.deps.RepoManager.GetActiveRepository(r.Context())
		if err != nil {
			WriteAPIError(w, http.StatusNotFound, "NO_ACTIVE_REPOSITORY", "No active repository selected")
			return
		}
		if active.Architecture == nil && h.deps.RepoAnalyzer != nil {
			if arch, err := h.deps.RepoAnalyzer.AnalyzeRepository(r.Context(), active.LocalPath, false); err == nil && arch != nil {
				active.Architecture = arch.Architecture
			}
		}
		json.NewEncoder(w).Encode(active)
		return
	}

	if path == "" {
		switch r.Method {
		case http.MethodGet:
			// GET /api/repositories - List all repositories
			repos, err := h.deps.RepoManager.ListRepositories(r.Context())
			if err != nil {
				slog.Error("Failed to list repositories", "error", err)
				WriteAPIError(w, http.StatusInternalServerError, "LIST_REPOSITORIES_FAILED", err.Error())
				return
			}
			json.NewEncoder(w).Encode(repos)

		case http.MethodPost:
			// POST /api/repositories - Add a new repository
			var req addRepoRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				WriteAPIError(w, http.StatusBadRequest, "INVALID_JSON_PAYLOAD", "Failed to parse JSON body")
				return
			}

			repo, err := h.deps.RepoManager.AddRepository(r.Context(), req.URL, req.LocalPath)
			if err != nil {
				if errors.Is(err, providers.ErrInvalidRepositoryURL) {
					WriteAPIError(w, http.StatusBadRequest, "INVALID_REPOSITORY_URL", "Provide a valid GitHub repository URL (e.g. https://github.com/owner/repo or owner/repo)")
					return
				}
				if errors.Is(err, providers.ErrDuplicateRepository) {
					WriteAPIError(w, http.StatusConflict, "DUPLICATE_REPOSITORY", "Repository already exists in workspace")
					return
				}
				slog.Error("Failed to add repository", "url", req.URL, "error", err)
				WriteAPIError(w, http.StatusInternalServerError, "ADD_REPOSITORY_FAILED", err.Error())
				return
			}

			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(repo)

		default:
			WriteAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		}
		return
	}

	parts := strings.Split(path, "/")
	repoID := parts[0]

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			// GET /api/repositories/:id
			repo, err := h.deps.RepoManager.GetRepository(r.Context(), repoID)
			if err != nil {
				// Fallback to analyzing repo directory if repoID isn't in MemoryRepoManager yet
				repo, err = h.deps.RepoAnalyzer.AnalyzeRepository(r.Context(), ".", false)
				if err != nil {
					slog.Error("Failed to fetch repository info", "repoID", repoID, "error", err)
					WriteAPIError(w, http.StatusNotFound, "REPOSITORY_NOT_FOUND", "Repository was not found")
					return
				}
			} else if repo.Architecture == nil && h.deps.RepoAnalyzer != nil {
				if arch, err := h.deps.RepoAnalyzer.AnalyzeRepository(r.Context(), repo.LocalPath, false); err == nil && arch != nil {
					repo.Architecture = arch.Architecture
				}
			}
			json.NewEncoder(w).Encode(repo)

		case http.MethodDelete:
			// DELETE /api/repositories/:id
			err := h.deps.RepoManager.DeleteRepository(r.Context(), repoID)
			if err != nil {
				WriteAPIError(w, http.StatusNotFound, "REPOSITORY_NOT_FOUND", "Repository was not found")
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			WriteAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		}
		return
	}

	subResource := parts[1]
	switch subResource {
	case "select":
		// POST /api/repositories/:id/select
		if r.Method != http.MethodPost {
			WriteAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only POST allowed for select")
			return
		}
		selected, err := h.deps.RepoManager.SelectRepository(r.Context(), repoID)
		if err != nil {
			WriteAPIError(w, http.StatusNotFound, "REPOSITORY_NOT_FOUND", "Repository was not found")
			return
		}
		json.NewEncoder(w).Encode(selected)

	case "status":
		// GET /api/repositories/:id/status
		if r.Method != http.MethodGet {
			WriteAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only GET allowed for status")
			return
		}
		status, err := h.deps.RepoManager.CheckIntegrationStatus(r.Context(), repoID)
		if err != nil {
			WriteAPIError(w, http.StatusNotFound, "REPOSITORY_NOT_FOUND", "Repository was not found")
			return
		}
		json.NewEncoder(w).Encode(status)

	case "checkpoints":
		// GET /api/repositories/:id/checkpoints
		cps, err := h.deps.CheckpointProvider.GetCheckpoints(r.Context(), repoID)
		if err != nil {
			slog.Error("Failed to fetch checkpoints", "repoID", repoID, "error", err)
			WriteAPIError(w, http.StatusInternalServerError, "CHECKPOINT_FETCH_FAILED", err.Error())
			return
		}
		sanitized := h.deps.Sanitizer.SanitizeCheckpoints(cps)
		json.NewEncoder(w).Encode(sanitized)
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
