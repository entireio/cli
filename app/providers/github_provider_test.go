package providers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/entireio/cli/app/models"
	"github.com/entireio/cli/app/providers"
)

func TestDevGitHubProvider_MilestonesAndRequirements(t *testing.T) {
	provider := providers.NewDevGitHubProvider()
	ctx := context.Background()

	milestones, err := provider.GetMilestones(ctx, "owner", "repo")
	if err != nil {
		t.Fatalf("unexpected error fetching milestones: %v", err)
	}

	if len(milestones) == 0 {
		t.Fatalf("expected non-empty milestones")
	}

	if milestones[0].Title == "" || milestones[0].Number == 0 {
		t.Errorf("expected valid milestone title and number, got %+v", milestones[0])
	}

	reqs, err := provider.GetMilestoneRequirements(ctx, "owner", "repo", 1)
	if err != nil {
		t.Fatalf("unexpected error fetching milestone requirements: %v", err)
	}

	if len(reqs) == 0 {
		t.Fatalf("expected non-empty requirements for milestone 1")
	}

	req := reqs[0]
	if req.GitHubIssueRef == "" || req.Title == "" || req.Source != "github" {
		t.Errorf("expected requirement with GitHub metadata, got %+v", req)
	}
}

func TestLiveGitHubProvider_GetMilestones_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/testowner/testrepo/milestones" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[
			{
				"node_id": "m_100",
				"number": 1,
				"title": "Release 1.0",
				"description": "First major release",
				"state": "open",
				"open_issues": 5,
				"closed_issues": 10,
				"html_url": "https://github.com/testowner/testrepo/milestone/1"
			}
		]`))
	}))
	defer server.Close()

	provider := providers.NewLiveGitHubProviderWithBaseURL(server.URL)
	milestones, err := provider.GetMilestones(context.Background(), "testowner", "testrepo")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(milestones) != 1 {
		t.Fatalf("expected 1 milestone, got %d", len(milestones))
	}

	m := milestones[0]
	if m.Number != 1 || m.Title != "Release 1.0" || m.OpenIssues != 5 || m.ClosedIssues != 10 {
		t.Errorf("milestone data mismatch: %+v", m)
	}
}

func TestLiveGitHubProvider_GetMilestoneRequirements_Conversion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/testowner/testrepo/issues" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[
			{
				"number": 42,
				"title": "Implement OAuth2 login flow",
				"body": "User authentication requirement",
				"state": "open",
				"html_url": "https://github.com/testowner/testrepo/issues/42",
				"milestone": {
					"title": "Release 1.0",
					"number": 1
				}
			},
			{
				"number": 43,
				"title": "Setup database schema migration",
				"body": "DB requirement",
				"state": "closed",
				"html_url": "https://github.com/testowner/testrepo/issues/43",
				"milestone": {
					"title": "Release 1.0",
					"number": 1
				}
			}
		]`))
	}))
	defer server.Close()

	provider := providers.NewLiveGitHubProviderWithBaseURL(server.URL)
	reqs, err := provider.GetMilestoneRequirements(context.Background(), "testowner", "testrepo", 1)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(reqs) != 2 {
		t.Fatalf("expected 2 requirements, got %d", len(reqs))
	}

	// Verify open issue conversion
	r1 := reqs[0]
	if r1.ID != "42" || r1.GitHubIssueRef != "42" || r1.Status != models.StatusNeedsVerification || r1.Milestone != "Release 1.0" || r1.State != "open" {
		t.Errorf("open requirement conversion mismatch: %+v", r1)
	}

	// Verify closed issue conversion
	r2 := reqs[1]
	if r2.ID != "43" || r2.GitHubIssueRef != "43" || r2.Status != models.StatusCompleted || r2.State != "closed" {
		t.Errorf("closed requirement conversion mismatch: %+v", r2)
	}
}

func TestLiveGitHubProvider_ErrorScenarios(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		response   string
		wantErr    string
	}{
		{"404 Not Found", http.StatusNotFound, `{"message":"Not Found"}`, "not found"},
		{"401 Unauthorized", http.StatusUnauthorized, `{"message":"Bad credentials"}`, "unauthorized"},
		{"403 Rate Limited", http.StatusForbidden, `{"message":"API rate limit exceeded"}`, "forbidden"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.response))
			}))
			defer server.Close()

			provider := providers.NewLiveGitHubProviderWithBaseURL(server.URL)
			_, err := provider.GetMilestones(context.Background(), "owner", "repo")
			if err == nil {
				t.Fatalf("expected error for status %d, got nil", tt.statusCode)
			}
		})
	}
}
