package providers

import (
	"context"
	"testing"
)

func TestParseGitHubURL(t *testing.T) {
	validCases := []struct {
		input     string
		wantOwner string
		wantRepo  string
		wantURL   string
	}{
		{"https://github.com/KAUSHALK123/cli_BTW", "KAUSHALK123", "cli_BTW", "https://github.com/KAUSHALK123/cli_BTW"},
		{"https://github.com/owner/repo.git", "owner", "repo", "https://github.com/owner/repo"},
		{"owner/myrepo", "owner", "myrepo", "https://github.com/owner/myrepo"},
		{"git@github.com:entireio/cli.git", "entireio", "cli", "https://github.com/entireio/cli"},
	}

	for _, tc := range validCases {
		owner, repo, cleanURL, err := ParseGitHubURL(tc.input)
		if err != nil {
			t.Errorf("unexpected error for %s: %v", tc.input, err)
			continue
		}
		if owner != tc.wantOwner || repo != tc.wantRepo || cleanURL != tc.wantURL {
			t.Errorf("parse mismatch for %s: got (%s, %s, %s), want (%s, %s, %s)",
				tc.input, owner, repo, cleanURL, tc.wantOwner, tc.wantRepo, tc.wantURL)
		}
	}

	invalidCases := []string{
		"",
		"invalid-single-word",
		"http://",
		"https://notgithub.com",
	}

	for _, tc := range invalidCases {
		_, _, _, err := ParseGitHubURL(tc)
		if err == nil {
			t.Errorf("expected error for invalid URL %s, got nil", tc)
		}
	}
}

func TestMemoryRepoManagerCRUD(t *testing.T) {
	mgr := NewMemoryRepoManager()
	ctx := context.Background()

	// Initial repository list
	repos, err := mgr.ListRepositories(ctx)
	if err != nil {
		t.Fatalf("failed to list repos: %v", err)
	}
	if len(repos) == 0 {
		t.Fatalf("expected initial seeded repo")
	}

	// Add new repository
	newRepo, err := mgr.AddRepository(ctx, "https://github.com/entireio/cli", ".")
	if err != nil {
		t.Fatalf("failed to add repo: %v", err)
	}

	if newRepo.Owner != "entireio" || newRepo.Name != "cli" {
		t.Errorf("unexpected repo fields: %+v", newRepo)
	}

	// Duplicate check
	_, err = mgr.AddRepository(ctx, "https://github.com/entireio/cli", ".")
	if err != ErrDuplicateRepository {
		t.Errorf("expected ErrDuplicateRepository, got %v", err)
	}

	// Active repository selection
	selected, err := mgr.SelectRepository(ctx, newRepo.ID)
	if err != nil {
		t.Fatalf("failed to select repo: %v", err)
	}
	if !selected.IsActive {
		t.Errorf("selected repo should be active")
	}

	active, err := mgr.GetActiveRepository(ctx)
	if err != nil || active.ID != newRepo.ID {
		t.Errorf("expected active repo ID %s, got %+v", newRepo.ID, active)
	}

	// Status check
	status, err := mgr.CheckIntegrationStatus(ctx, newRepo.ID)
	if err != nil || status == nil {
		t.Errorf("failed to check integration status: %v", err)
	}

	// Delete repository
	err = mgr.DeleteRepository(ctx, newRepo.ID)
	if err != nil {
		t.Fatalf("failed to delete repo: %v", err)
	}

	_, err = mgr.GetRepository(ctx, newRepo.ID)
	if err != ErrRepositoryNotFound {
		t.Errorf("expected ErrRepositoryNotFound after delete, got %v", err)
	}
}
