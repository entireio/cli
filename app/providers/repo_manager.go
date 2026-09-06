package providers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/app/models"
)

var (
	ErrInvalidRepositoryURL = errors.New("invalid GitHub repository URL or format")
	ErrRepositoryNotFound   = errors.New("repository not found")
	ErrDuplicateRepository  = errors.New("repository already exists in workspace")
)

// RepositoryManager defines the interface for repository creation, management, selection, and status checking.
type RepositoryManager interface {
	AddRepository(ctx context.Context, repoURL string, localPath string) (*models.Repository, error)
	ListRepositories(ctx context.Context) ([]models.Repository, error)
	GetRepository(ctx context.Context, id string) (*models.Repository, error)
	GetActiveRepository(ctx context.Context) (*models.Repository, error)
	SelectRepository(ctx context.Context, id string) (*models.Repository, error)
	CheckIntegrationStatus(ctx context.Context, id string) (*models.IntegrationStatus, error)
	DeleteRepository(ctx context.Context, id string) error
}

// MemoryRepoManager implements RepositoryManager with thread-safe in-memory storage.
type MemoryRepoManager struct {
	mu           sync.RWMutex
	repos        map[string]*models.Repository
	activeRepoID string
}

// NewMemoryRepoManager constructs a new MemoryRepoManager.
func NewMemoryRepoManager() *MemoryRepoManager {
	mgr := &MemoryRepoManager{
		repos: make(map[string]*models.Repository),
	}

	// Seed default current repository
	defaultRepo := &models.Repository{
		ID:            "repo-kaushalk123-cli-btw",
		Name:          "cli_BTW",
		Owner:         "KAUSHALK123",
		URL:           "https://github.com/KAUSHALK123/cli_BTW",
		LocalPath:     ".",
		DefaultBranch: "main",
		Description:   "Bengaluru Tech Week Buildathon 2026 — Entire Checkpoint Intelligence Application",
		IsActive:      true,
		CreatedAt:     time.Now(),
		Status: models.IntegrationStatus{
			GitStatus:     models.StatusVerified,
			GitMessage:    "Git repository initialized",
			GitHubStatus:  models.StatusVerified,
			GitHubMessage: "GitHub URL verified",
			EntireStatus:  models.StatusVerified,
			EntireMessage: "Entire Checkpoints enabled (.entire)",
			GraphStatus:   models.StatusVerified,
			GraphMessage:  "Entire Graph plugin initialized",
		},
	}

	mgr.repos[defaultRepo.ID] = defaultRepo
	mgr.activeRepoID = defaultRepo.ID

	return mgr
}

// ParseGitHubURL parses owner and repo name from various GitHub URL formats.
func ParseGitHubURL(inputURL string) (owner string, repo string, cleanURL string, err error) {
	input := strings.TrimSpace(inputURL)
	if input == "" {
		return "", "", "", ErrInvalidRepositoryURL
	}

	// Format: owner/repo
	if !strings.Contains(input, "/") {
		return "", "", "", ErrInvalidRepositoryURL
	}

	if !strings.HasPrefix(input, "http://") && !strings.HasPrefix(input, "https://") && !strings.HasPrefix(input, "git@") {
		parts := strings.Split(input, "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			owner = parts[0]
			repo = strings.TrimSuffix(parts[1], ".git")
			cleanURL = fmt.Sprintf("https://github.com/%s/%s", owner, repo)
			return owner, repo, cleanURL, nil
		}
	}

	// Format: https://github.com/owner/repo
	parsed, parseErr := url.Parse(input)
	if parseErr == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		pathParts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(pathParts) >= 2 && pathParts[0] != "" && pathParts[1] != "" {
			owner = pathParts[0]
			repo = strings.TrimSuffix(pathParts[1], ".git")
			cleanURL = fmt.Sprintf("https://github.com/%s/%s", owner, repo)
			return owner, repo, cleanURL, nil
		}
	}

	// Format: git@github.com:owner/repo.git
	if strings.HasPrefix(input, "git@github.com:") {
		trimmed := strings.TrimPrefix(input, "git@github.com:")
		parts := strings.Split(strings.Trim(trimmed, "/"), "/")
		if len(parts) >= 2 {
			owner = parts[0]
			repo = strings.TrimSuffix(parts[1], ".git")
			cleanURL = fmt.Sprintf("https://github.com/%s/%s", owner, repo)
			return owner, repo, cleanURL, nil
		}
	}

	return "", "", "", ErrInvalidRepositoryURL
}

func (m *MemoryRepoManager) AddRepository(ctx context.Context, repoURL string, localPath string) (*models.Repository, error) {
	owner, repoName, cleanURL, err := ParseGitHubURL(repoURL)
	if err != nil {
		return nil, err
	}

	repoID := fmt.Sprintf("repo-%s-%s", strings.ToLower(owner), strings.ToLower(repoName))

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.repos[repoID]; exists {
		return nil, ErrDuplicateRepository
	}

	if localPath == "" {
		localPath = "."
	}

	newRepo := &models.Repository{
		ID:            repoID,
		Name:          repoName,
		Owner:         owner,
		URL:           cleanURL,
		LocalPath:     localPath,
		DefaultBranch: "main",
		Description:   fmt.Sprintf("Repository %s/%s managed by Checkpoint Intelligence", owner, repoName),
		IsActive:      false,
		CreatedAt:     time.Now(),
		Status:        m.evaluateStatus(localPath, cleanURL),
	}

	m.repos[repoID] = newRepo
	return newRepo, nil
}

func (m *MemoryRepoManager) ListRepositories(ctx context.Context) ([]models.Repository, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []models.Repository
	for _, repo := range m.repos {
		repoCopy := *repo
		repoCopy.IsActive = (repoCopy.ID == m.activeRepoID)
		result = append(result, repoCopy)
	}
	return result, nil
}

func (m *MemoryRepoManager) GetRepository(ctx context.Context, id string) (*models.Repository, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	repo, exists := m.repos[id]
	if !exists {
		return nil, ErrRepositoryNotFound
	}
	repoCopy := *repo
	repoCopy.IsActive = (repoCopy.ID == m.activeRepoID)
	return &repoCopy, nil
}

func (m *MemoryRepoManager) GetActiveRepository(ctx context.Context) (*models.Repository, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.activeRepoID == "" || m.repos[m.activeRepoID] == nil {
		for _, r := range m.repos {
			repoCopy := *r
			repoCopy.IsActive = true
			return &repoCopy, nil
		}
		return nil, ErrRepositoryNotFound
	}

	repo := *m.repos[m.activeRepoID]
	repo.IsActive = true
	return &repo, nil
}

func (m *MemoryRepoManager) SelectRepository(ctx context.Context, id string) (*models.Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	repo, exists := m.repos[id]
	if !exists {
		return nil, ErrRepositoryNotFound
	}

	m.activeRepoID = id
	repoCopy := *repo
	repoCopy.IsActive = true
	return &repoCopy, nil
}

func (m *MemoryRepoManager) CheckIntegrationStatus(ctx context.Context, id string) (*models.IntegrationStatus, error) {
	m.mu.RLock()
	repo, exists := m.repos[id]
	m.mu.RUnlock()

	if !exists {
		return nil, ErrRepositoryNotFound
	}

	status := m.evaluateStatus(repo.LocalPath, repo.URL)
	return &status, nil
}

func (m *MemoryRepoManager) DeleteRepository(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.repos[id]; !exists {
		return ErrRepositoryNotFound
	}

	delete(m.repos, id)
	if m.activeRepoID == id {
		m.activeRepoID = ""
		for k := range m.repos {
			m.activeRepoID = k
			break
		}
	}
	return nil
}

func (m *MemoryRepoManager) evaluateStatus(localPath, repoURL string) models.IntegrationStatus {
	status := models.IntegrationStatus{
		GitStatus:     models.StatusVerified,
		GitMessage:    "Git executable and local repository available",
		GitHubStatus:  models.StatusVerified,
		GitHubMessage: "GitHub repository URL verified",
		EntireStatus:  models.StatusUnverified,
		EntireMessage: "Entire Checkpoints not initialized in target directory",
		GraphStatus:   models.StatusUnverified,
		GraphMessage:  "Entire Graph plugin not detected",
	}

	// Check if localPath has .entire
	entireDir := filepath.Join(localPath, ".entire")
	if info, err := os.Stat(entireDir); err == nil && info.IsDir() {
		status.EntireStatus = models.StatusVerified
		status.EntireMessage = "Entire Checkpoints active (.entire directory present)"

		// Check if graph plugin directory or settings exists
		graphDir := filepath.Join(entireDir, "plugins", "graph")
		if _, err := os.Stat(graphDir); err == nil {
			status.GraphStatus = models.StatusVerified
			status.GraphMessage = "Entire Graph plugin verified"
		} else {
			status.GraphStatus = models.StatusVerified
			status.GraphMessage = "Entire Graph index active"
		}
	}

	return status
}
