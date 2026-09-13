package models

import (
	"time"
)

// RepositoryStatusValue represents the integration verification status.
type RepositoryStatusValue string

const (
	StatusVerified   RepositoryStatusValue = "verified"
	StatusUnverified RepositoryStatusValue = "unverified"
	StatusFailed     RepositoryStatusValue = "failed"
)

// IntegrationStatus holds readiness information for Git, GitHub, Entire, and Entire Graph.
type IntegrationStatus struct {
	GitStatus     RepositoryStatusValue `json:"git_status"`
	GitMessage    string                `json:"git_message"`
	GitHubStatus  RepositoryStatusValue `json:"github_status"`
	GitHubMessage string                `json:"github_message"`
	EntireStatus  RepositoryStatusValue `json:"entire_status"`
	EntireMessage string                `json:"entire_message"`
	GraphStatus   RepositoryStatusValue `json:"graph_status"`
	GraphMessage  string                `json:"graph_message"`
}

// Repository represents a git code repository managed in the workspace.
type Repository struct {
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Owner         string                  `json:"owner"`
	URL           string                  `json:"url"`
	LocalPath     string                  `json:"local_path"`
	DefaultBranch string                  `json:"default_branch"`
	Description   string                  `json:"description"`
	IsActive      bool                    `json:"is_active"`
	CreatedAt     time.Time               `json:"created_at"`
	Status        IntegrationStatus       `json:"status"`
	Architecture  *RepositoryArchitecture `json:"architecture,omitempty"`
}

// RepositoryArchitecture holds the detected architectural summary of a repository.
type RepositoryArchitecture struct {
	Summary        string    `json:"summary"`
	Directories    []string  `json:"directories"`
	ImportantFiles []string  `json:"important_files"`
	EntryPoints    []string  `json:"entry_points"`
	Components     []string  `json:"components"`
	APIRoutes      []string  `json:"api_routes"`
	TechStack      []string  `json:"tech_stack"`
	ConfigFiles    []string  `json:"config_files"`
	TestStructure  []string  `json:"test_structure"`
	InferredInfo   []string  `json:"inferred_info"` // Clearly labeled inferred information
	UnknownInfo    []string  `json:"unknown_info"`  // Clearly labeled unknown information
	LastAnalyzedAt time.Time `json:"last_analyzed_at"`
}
