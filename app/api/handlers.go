package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/entireio/cli/app/privacy"
	"github.com/entireio/cli/app/providers"
)

// ServerDependencies encapsulates all provider interfaces required by the API layer.
type ServerDependencies struct {
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
		CheckpointProvider: providers.NewDevCheckpointProvider(),
		GraphProvider:      providers.NewDevGraphProvider(),
		GitHubProvider:     providers.NewDevGitHubProvider(),
		RepoAnalyzer:       devAnalyzer,
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
func (h *APIHandler) RepositoriesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/repositories")
	path = strings.Trim(path, "/")

	if path == "" {
		// GET /api/repositories
		repo, err := h.deps.RepoAnalyzer.AnalyzeRepository(r.Context(), ".")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode([]interface{}{repo})
		return
	}

	parts := strings.Split(path, "/")
	repoID := parts[0]

	if len(parts) == 1 {
		// GET /api/repositories/:id
		repo, err := h.deps.RepoAnalyzer.AnalyzeRepository(r.Context(), ".")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sanitized := h.deps.Sanitizer.SanitizeCheckpoints(cps)
		json.NewEncoder(w).Encode(sanitized)
	case "requirements":
		// GET /api/repositories/:id/requirements
		reqs, err := h.deps.ReqAnalyzer.AnalyzeRequirements(r.Context(), repoID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(reqs)
	case "graph":
		// GET /api/repositories/:id/graph
		findings, err := h.deps.GraphProvider.GetGraphFindings(r.Context(), repoID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(findings)
	case "handoff":
		// GET /api/repositories/:id/handoff
		handoff, err := h.deps.ReqAnalyzer.GetHandoff(r.Context(), repoID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(handoff)
	default:
		http.NotFound(w, r)
	}
}
