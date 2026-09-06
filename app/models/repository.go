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
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Owner         string            `json:"owner"`
	URL           string            `json:"url"`
	LocalPath     string            `json:"local_path"`
	DefaultBranch string            `json:"default_branch"`
	Description   string            `json:"description"`
	IsActive      bool              `json:"is_active"`
	CreatedAt     time.Time         `json:"created_at"`
	Status        IntegrationStatus `json:"status"`
}
