package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/entireio/cli/app/models"
)

// GitHubProvider defines the interface for interacting with GitHub repositories.
type GitHubProvider interface {
	GetRepositoryInfo(ctx context.Context, owner, repo string) (*models.Repository, error)
	GetMilestones(ctx context.Context, owner, repo string) ([]models.Milestone, error)
	GetMilestoneRequirements(ctx context.Context, owner, repo string, milestoneNumber int) ([]models.Requirement, error)
}

// DevGitHubProvider provides development test data implementing GitHubProvider.
// NOTE: This provider returns development fixtures for testing core application foundation.
type DevGitHubProvider struct{}

func NewDevGitHubProvider() GitHubProvider {
	return &DevGitHubProvider{}
}

func (p *DevGitHubProvider) GetRepositoryInfo(ctx context.Context, owner, repo string) (*models.Repository, error) {
	return &models.Repository{
		ID:            "repo-btw-cli",
		Name:          repo,
		Owner:         owner,
		URL:           "https://github.com/" + owner + "/" + repo,
		LocalPath:     "d:\\scaler buildathon\\cli_BTW",
		DefaultBranch: "main",
		Description:   "Bengaluru Tech Week Buildathon 2026 — Entire Checkpoint Intelligence Application",
	}, nil
}

func (p *DevGitHubProvider) GetMilestones(ctx context.Context, owner, repo string) ([]models.Milestone, error) {
	return []models.Milestone{
		{ID: "m1", Number: 1, Title: "Phase 1: Foundation", Description: "Initial setup & API foundation", State: "open", OpenIssues: 3, ClosedIssues: 5, URL: "https://github.com/" + owner + "/" + repo + "/milestone/1"},
		{ID: "m2", Number: 2, Title: "Phase 2: Integration", Description: "GitHub connection & requirement workflow", State: "open", OpenIssues: 2, ClosedIssues: 1, URL: "https://github.com/" + owner + "/" + repo + "/milestone/2"},
	}, nil
}

func (p *DevGitHubProvider) GetMilestoneRequirements(ctx context.Context, owner, repo string, milestoneNumber int) ([]models.Requirement, error) {
	return []models.Requirement{
		{
			ID:              "13",
			Title:           "COMPLETE THE REPOSITORY -> GITHUB -> REQUIREMENT WORKFLOW",
			Description:     "Make the application capable of taking a repository and turning its GitHub development goals into requirements that the Checkpoint Intelligence engine can consume.",
			Status:          models.StatusNeedsVerification,
			Source:          "github",
			Milestone:       "Phase 2: Integration",
			MilestoneNumber: milestoneNumber,
			GitHubIssueRef:  "13",
			RepositoryID:    owner + "/" + repo,
			State:           "open",
		},
		{
			ID:              "14",
			Title:           "Repository Readiness Verification Matrix",
			Description:     "Detect and report actual status for Git, GitHub, Entire, and Entire Graph integrations.",
			Status:          models.StatusCompleted,
			Source:          "github",
			Milestone:       "Phase 2: Integration",
			MilestoneNumber: milestoneNumber,
			GitHubIssueRef:  "14",
			RepositoryID:    owner + "/" + repo,
			State:           "closed",
		},
	}, nil
}

// LiveGitHubProvider implements GitHubProvider using the actual GitHub API.
type LiveGitHubProvider struct {
	client  *http.Client
	baseURL string
}

func NewLiveGitHubProvider() *LiveGitHubProvider {
	return &LiveGitHubProvider{
		client:  &http.Client{Timeout: 10 * time.Second},
		baseURL: "https://api.github.com",
	}
}

func NewLiveGitHubProviderWithBaseURL(baseURL string) *LiveGitHubProvider {
	return &LiveGitHubProvider{
		client:  &http.Client{Timeout: 10 * time.Second},
		baseURL: baseURL,
	}
}

func (p *LiveGitHubProvider) doRequest(ctx context.Context, url string, result interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "Entire-Checkpoint-Intelligence")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("repository or resource not found (404)")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("GitHub token invalid or unauthorized (401)")
	}
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("GitHub API rate limit exceeded or access forbidden (403)")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API returned status: %d", resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("failed to decode GitHub response: %w", err)
	}

	return nil
}

func (p *LiveGitHubProvider) GetRepositoryInfo(ctx context.Context, owner, repo string) (*models.Repository, error) {
	url := fmt.Sprintf("%s/repos/%s/%s", p.baseURL, owner, repo)
	var data struct {
		Name          string `json:"name"`
		Owner         struct{ Login string } `json:"owner"`
		HTMLURL       string `json:"html_url"`
		DefaultBranch string `json:"default_branch"`
		Description   string `json:"description"`
	}

	if err := p.doRequest(ctx, url, &data); err != nil {
		return nil, err
	}

	return &models.Repository{
		ID:            fmt.Sprintf("%s/%s", data.Owner.Login, data.Name),
		Name:          data.Name,
		Owner:         data.Owner.Login,
		URL:           data.HTMLURL,
		DefaultBranch: data.DefaultBranch,
		Description:   data.Description,
	}, nil
}

func (p *LiveGitHubProvider) GetMilestones(ctx context.Context, owner, repo string) ([]models.Milestone, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/milestones?state=all", p.baseURL, owner, repo)
	var data []struct {
		NodeID       string `json:"node_id"`
		Number       int    `json:"number"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		State        string `json:"state"`
		OpenIssues   int    `json:"open_issues"`
		ClosedIssues int    `json:"closed_issues"`
		HTMLURL      string `json:"html_url"`
	}

	if err := p.doRequest(ctx, url, &data); err != nil {
		return nil, err
	}

	milestones := make([]models.Milestone, len(data))
	for i, m := range data {
		milestones[i] = models.Milestone{
			ID:           m.NodeID,
			Number:       m.Number,
			Title:        m.Title,
			Description:  m.Description,
			State:        m.State,
			OpenIssues:   m.OpenIssues,
			ClosedIssues: m.ClosedIssues,
			URL:          m.HTMLURL,
		}
	}

	return milestones, nil
}

func (p *LiveGitHubProvider) GetMilestoneRequirements(ctx context.Context, owner, repo string, milestoneNumber int) ([]models.Requirement, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues?milestone=%d&state=all", p.baseURL, owner, repo, milestoneNumber)
	var data []struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		State     string `json:"state"`
		HTMLURL   string `json:"html_url"`
		Milestone struct {
			Title  string `json:"title"`
			Number int    `json:"number"`
		} `json:"milestone"`
	}

	if err := p.doRequest(ctx, url, &data); err != nil {
		return nil, err
	}

	reqs := make([]models.Requirement, len(data))
	for i, issue := range data {
		status := models.StatusNeedsVerification
		if issue.State == "closed" {
			status = models.StatusCompleted
		}

		reqs[i] = models.Requirement{
			ID:              strconv.Itoa(issue.Number),
			Title:           issue.Title,
			Description:     issue.Body,
			Status:          status,
			Source:          "github",
			Milestone:       issue.Milestone.Title,
			MilestoneNumber: milestoneNumber,
			GitHubIssueRef:  strconv.Itoa(issue.Number),
			RepositoryID:    fmt.Sprintf("%s/%s", owner, repo),
			State:           issue.State,
		}
	}

	return reqs, nil
}
