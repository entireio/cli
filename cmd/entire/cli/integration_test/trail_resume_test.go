//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/discovery"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

func TestTrailResume_UsesCheckpointSessionsWhenLocalStateIsMissing(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	addTrailResumeIntegrationOrigin(t, env, "https://github.com/entireio/cli.git")

	firstSession := env.NewSession()
	firstPrompt := "Create hello method"
	if err := env.SimulateUserPromptSubmitWithPrompt(firstSession.ID, firstPrompt); err != nil {
		t.Fatalf("SimulateUserPromptSubmit first session: %v", err)
	}
	firstContent := "def hello; :hello; end\n"
	env.WriteFile("hello.rb", firstContent)
	firstSession.CreateTranscript(firstPrompt, []FileChange{{Path: "hello.rb", Content: firstContent}})
	if err := env.SimulateStop(firstSession.ID, firstSession.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop first session: %v", err)
	}

	secondSession := env.NewSession()
	secondPrompt := "Create goodbye method"
	if err := env.SimulateUserPromptSubmitWithPrompt(secondSession.ID, secondPrompt); err != nil {
		t.Fatalf("SimulateUserPromptSubmit second session: %v", err)
	}
	secondContent := "def goodbye; :goodbye; end\n"
	env.WriteFile("goodbye.rb", secondContent)
	secondSession.CreateTranscript(secondPrompt, []FileChange{{Path: "goodbye.rb", Content: secondContent}})
	if err := env.SimulateStop(secondSession.ID, secondSession.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop second session: %v", err)
	}

	env.GitCommitWithShadowHooks("Add hello and goodbye methods", "hello.rb", "goodbye.rb")
	checkpointID := env.GetLatestCheckpointIDFromHistory()

	if err := env.ClearSessionState(firstSession.ID); err != nil {
		t.Fatalf("clear first session state: %v", err)
	}
	if err := env.ClearSessionState(secondSession.ID); err != nil {
		t.Fatalf("clear second session state: %v", err)
	}

	trail := api.TrailResource{
		ID:        "trail-integration-321",
		Number:    321,
		URL:       "https://entire.io/gh/entireio/cli/trails/321",
		Branch:    env.GetCurrentBranch(),
		Base:      masterBranch,
		Title:     "Resume checkpoint sessions from trail",
		Status:    "open",
		Phase:     "building",
		CreatedAt: time.Now().Add(-time.Hour).UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	server := newTrailResumeIntegrationAPIServer(t, trail)
	defer server.Close()
	configureTrailResumeIntegrationAuth(t, env, server.URL)

	contextOutput := env.RunCLI("trail", "--insecure-http-auth", "resume", "321", "--no-resume")
	for _, want := range []string{
		"Trail #321",
		"Checkpoint sessions:",
		firstSession.ID,
		secondSession.ID,
		checkpointID,
		"Create hello method",
		"Create goodbye method",
		"entire trail resume 321 --repo entireio/cli --branch feature/test-branch --session " + firstSession.ID,
		"entire trail resume 321 --repo entireio/cli --branch feature/test-branch --session " + secondSession.ID,
	} {
		if !strings.Contains(contextOutput, want) {
			t.Fatalf("trail resume --no-resume output missing %q:\n%s", want, contextOutput)
		}
	}
	if strings.Contains(contextOutput, "none found") {
		t.Fatalf("trail resume should not report missing local sessions after reading checkpoint metadata:\n%s", contextOutput)
	}

	resumeOutput := env.RunCLI("trail", "--insecure-http-auth", "resume", "321", "--session", secondSession.ID)
	for _, want := range []string{
		"Restored checkpoint " + checkpointID + " (1 session)",
		"claude -r " + secondSession.ID,
		"Create goodbye method",
	} {
		if !strings.Contains(resumeOutput, want) {
			t.Fatalf("trail resume --session output missing %q:\n%s", want, resumeOutput)
		}
	}

	restoredTranscript := filepath.Join(env.ClaudeProjectDir, secondSession.ID+".jsonl")
	data, err := os.ReadFile(restoredTranscript)
	if err != nil {
		t.Fatalf("read restored transcript %s: %v", restoredTranscript, err)
	}
	if !strings.Contains(string(data), "Create goodbye method") {
		t.Fatalf("restored transcript does not contain selected session prompt:\n%s", data)
	}
}

func newTrailResumeIntegrationAPIServer(t *testing.T, trail api.TrailResource) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/token":
			writeTrailResumeIntegrationJSON(t, w, http.StatusOK, map[string]any{
				"access_token": "trail-resume-data-token",
				"token_type":   "Bearer",
				"expires_in":   3600,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trails/gh/entireio/cli":
			writeTrailResumeIntegrationJSON(t, w, http.StatusOK, api.TrailListResponse{
				Trails:       []api.TrailResource{trail},
				Total:        1,
				Limit:        200,
				RepoFullName: "entireio/cli",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trails/"+url.PathEscape(trail.ID)+"/reviews/comments":
			writeTrailResumeIntegrationJSON(t, w, http.StatusOK, map[string]any{
				"comments": []any{},
				"has_more": false,
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func writeTrailResumeIntegrationJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode JSON response: %v", err)
	}
}

func configureTrailResumeIntegrationAuth(t *testing.T, env *TestEnv, coreURL string) {
	t.Helper()

	configDir := filepath.Join(env.RepoDir, ".entire-test-config")
	xdgCacheHome := filepath.Join(env.RepoDir, ".entire-test-cache")
	tokenStorePath := filepath.Join(env.RepoDir, ".entire-test-tokens.json")
	service := tokenstore.CoreKeyringService(coreURL)
	handle := "tester"

	if err := contexts.Save(configDir, &contexts.File{
		CurrentContext: "tester@trail-resume",
		Contexts: []*contexts.Context{
			{
				Name:            "tester@trail-resume",
				CoreURL:         coreURL,
				Handle:          handle,
				KeychainService: service,
			},
		},
	}); err != nil {
		t.Fatalf("save auth context: %v", err)
	}

	host := mustTrailResumeIntegrationHost(t, coreURL)
	cacheDir := filepath.Join(xdgCacheHome, "entire")
	if err := discovery.ModifyAPICores(cacheDir, func(c discovery.ClusterCoresCache) error {
		c.Set(host, []string{coreURL})
		return nil
	}); err != nil {
		t.Fatalf("seed API discovery cache: %v", err)
	}

	tokenStore := map[string]map[string]string{
		service: {
			handle: tokenstore.EncodeTokenWithExpiration(fakeLoginJWT(coreURL), 7200),
		},
	}
	tokenData, err := json.Marshal(tokenStore)
	if err != nil {
		t.Fatalf("marshal token store: %v", err)
	}
	if err := os.WriteFile(tokenStorePath, tokenData, 0o600); err != nil {
		t.Fatalf("write token store: %v", err)
	}

	env.ExtraEnv = append(env.ExtraEnv,
		"ENTIRE_API_BASE_URL="+coreURL,
		"ENTIRE_CONFIG_DIR="+configDir,
		"XDG_CACHE_HOME="+xdgCacheHome,
		"ENTIRE_TOKEN_STORE=file",
		"ENTIRE_TOKEN_STORE_PATH="+tokenStorePath,
	)
}

func mustTrailResumeIntegrationHost(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse URL %q: %v", rawURL, err)
	}
	if parsed.Host == "" {
		t.Fatalf("URL %q has no host", rawURL)
	}
	return parsed.Host
}

func addTrailResumeIntegrationOrigin(t *testing.T, env *TestEnv, remoteURL string) {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "git", "remote", "add", "origin", remoteURL)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add origin: %v\n%s", err, output)
	}

	configData, err := os.ReadFile(filepath.Join(env.RepoDir, ".git", "config"))
	if err != nil {
		t.Fatalf("read git config after remote add: %v", err)
	}
	if !strings.Contains(string(configData), remoteURL) {
		t.Fatalf("git config does not contain origin URL %q:\n%s", remoteURL, configData)
	}
	env.AcceptGitConfigChanges(string(configData))
}
