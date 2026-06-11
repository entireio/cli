package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testOwner = "entirehq"
const testRepo = "entire.io"
const testCPID = "cp1"

// writeTestJSON writes raw JSON to a response writer, ignoring write errors (test helper).
func writeTestJSON(w http.ResponseWriter, jsonStr string) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(jsonStr)) //nolint:errcheck // test helper
}

// -- ParseGitHubRemote tests --

func TestParseGitHubRemote_SSH(t *testing.T) {
	t.Parallel()
	owner, repo, err := ParseGitHubRemote("git@github.com:entirehq/entire.io.git")
	if err != nil {
		t.Fatal(err)
	}
	if owner != testOwner || repo != testRepo {
		t.Errorf("got %s/%s, want %s/%s", owner, repo, testOwner, testRepo)
	}
}

func TestParseGitHubRemote_HTTPS(t *testing.T) {
	t.Parallel()
	owner, repo, err := ParseGitHubRemote("https://github.com/entirehq/entire.io.git")
	if err != nil {
		t.Fatal(err)
	}
	if owner != testOwner || repo != testRepo {
		t.Errorf("got %s/%s, want %s/%s", owner, repo, testOwner, testRepo)
	}
}

func TestParseGitHubRemote_HTTPSNoGit(t *testing.T) {
	t.Parallel()
	owner, repo, err := ParseGitHubRemote("https://github.com/entirehq/entire.io")
	if err != nil {
		t.Fatal(err)
	}
	if owner != testOwner || repo != testRepo {
		t.Errorf("got %s/%s, want %s/%s", owner, repo, testOwner, testRepo)
	}
}

func TestParseGitHubRemote_Invalid(t *testing.T) {
	t.Parallel()
	_, _, err := ParseGitHubRemote("")
	if err == nil {
		t.Error("expected error for empty URL")
	}

	_, _, err = ParseGitHubRemote("not-a-url")
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestParseGitHubRemote_SSHProtocol(t *testing.T) {
	t.Parallel()
	owner, repo, err := ParseGitHubRemote("ssh://git@github.com/entirehq/entire.io.git")
	if err != nil {
		t.Fatal(err)
	}
	if owner != testOwner || repo != testRepo {
		t.Errorf("got %s/%s, want %s/%s", owner, repo, testOwner, testRepo)
	}
}

func TestParseGitHubRemote_SSHProtocolNoGit(t *testing.T) {
	t.Parallel()
	owner, repo, err := ParseGitHubRemote("ssh://git@github.com/entirehq/entire.io")
	if err != nil {
		t.Fatal(err)
	}
	if owner != testOwner || repo != testRepo {
		t.Errorf("got %s/%s, want %s/%s", owner, repo, testOwner, testRepo)
	}
}

func TestParseGitHubRemote_NonGitHubSSH(t *testing.T) {
	t.Parallel()
	_, _, err := ParseGitHubRemote("git@gitlab.com:entirehq/entire.io.git")
	if err == nil {
		t.Error("expected error for non-GitHub SSH remote")
	}
}

func TestParseGitHubRemote_NonGitHubHTTPS(t *testing.T) {
	t.Parallel()
	_, _, err := ParseGitHubRemote("https://gitlab.com/entirehq/entire.io.git")
	if err == nil {
		t.Error("expected error for non-GitHub HTTPS remote")
	}
}

// -- Search() tests --

func TestSearch_URLConstruction(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "ghp_test123",
		Owner:       "myowner",
		Repo:        "myrepo",
		Query:       "find bugs",
		Limit:       10,
	})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Path != "/search/v1/search" {
		t.Errorf("path = %s, want /search/v1/search", capturedReq.URL.Path)
	}
	if capturedReq.URL.Query().Get("q") != "find bugs" {
		t.Errorf("q = %s, want 'find bugs'", capturedReq.URL.Query().Get("q"))
	}
	if capturedReq.URL.Query().Get("repo") != "myowner/myrepo" {
		t.Errorf("repo = %s, want 'myowner/myrepo'", capturedReq.URL.Query().Get("repo"))
	}
	// types param should NOT be set — the CLI now requests all types
	if capturedReq.URL.Query().Has("types") {
		t.Errorf("types param should not be set, got %q", capturedReq.URL.Query().Get("types"))
	}
	if capturedReq.URL.Query().Get("limit") != "10" {
		t.Errorf("limit = %s, want '10'", capturedReq.URL.Query().Get("limit"))
	}
	if capturedReq.Header.Get("Authorization") != "Bearer ghp_test123" {
		t.Errorf("auth header = %s, want 'Bearer ghp_test123'", capturedReq.Header.Get("Authorization"))
	}
	if capturedReq.Header.Get("User-Agent") != "entire-cli" {
		t.Errorf("user-agent = %s, want 'entire-cli'", capturedReq.Header.Get("User-Agent"))
	}
}

func TestSearch_ZeroLimitOmitsParam(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Query().Has("limit") {
		t.Error("limit param should be omitted when zero")
	}
}

func TestSearch_ErrorJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid token"}) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "bad",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if got := err.Error(); got != "search service error (401): Invalid token" {
		t.Errorf("error = %q, want 'search service error (401): Invalid token'", got)
	}
}

func TestSearch_ErrorRawBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("upstream timeout")) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err == nil {
		t.Fatal("expected error for 502")
	}
	if got := err.Error(); got != "search service returned 502: upstream timeout" {
		t.Errorf("error = %q", got)
	}
}

func TestSearch_HTMLResponseNon200(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>Bad Gateway</html>")) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err == nil {
		t.Fatal("expected error for HTML response")
	}
	want := "search service returned 502: <html>Bad Gateway</html>"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestSearch_HTMLResponseOn200(t *testing.T) {
	t.Parallel()

	htmlBody := "<!DOCTYPE html><html><body>Website</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(htmlBody)) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err == nil {
		t.Fatal("expected error for HTML response on 200")
	}
	if !strings.Contains(err.Error(), htmlBody) {
		t.Errorf("error should contain full body, got: %q", err.Error())
	}
}

func TestSearch_ErrorFieldOn200(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Response{Error: "user not found in Entire"}) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err == nil {
		t.Fatal("expected error when server returns 200 with error field")
	}
	if !strings.Contains(err.Error(), "user not found") {
		t.Errorf("error = %q, want message containing 'user not found'", err.Error())
	}
}

func TestSearch_SuccessWithResults(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, `{"results":[{"type":"checkpoint","data":{"id":"abc123def456","branch":"main","prompt":"add auth middleware","author":"alice","createdAt":"2026-01-13T12:00:00Z","org":"","repo":"","filesTouched":[]},"searchMeta":{"score":0.042,"matchType":"both"}}],"total":1,"page":1}`)
	}))
	defer srv.Close()

	resp, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(resp.Results))
	}
	r := resp.Results[0]
	if r.Type != TypeCheckpoint {
		t.Errorf("type = %s, want checkpoint", r.Type)
	}
	if r.Checkpoint == nil {
		t.Fatal("checkpoint data is nil")
	}
	if r.Checkpoint.ID != "abc123def456" {
		t.Errorf("checkpoint id = %s, want abc123def456", r.Checkpoint.ID)
	}
	if r.Meta.MatchType != "both" {
		t.Errorf("matchType = %s, want both", r.Meta.MatchType)
	}
}

func TestSearch_SuccessWithMultipleTypes(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, `{"results":[{"type":"checkpoint","data":{"id":"cp1","prompt":"fix bug","branch":"main","org":"o","repo":"r","author":"alice","createdAt":"2026-01-13T12:00:00Z","filesTouched":[]},"searchMeta":{"matchType":"keyword","score":0.5}},{"type":"commit","data":{"id":"cm1","commitSha":"abc1234567890","commitMessage":"fix: auth bug","commitSubject":"fix: auth bug","branch":"main","org":"o","repo":"r","author":"bob","createdAt":"2026-01-14T12:00:00Z","additions":10,"deletions":5,"filesChanged":3},"searchMeta":{"matchType":"semantic","score":0.3}},{"type":"session","data":{"sessionId":"ss1","displayName":"Debug auth","org":"o","repo":"r","createdAt":"2026-01-15T12:00:00Z","stepCount":5},"searchMeta":{"matchType":"both","score":0.4}}],"total":3,"page":1,"counts":{"repos":0,"checkpoints":1,"commits":1,"prs":0,"sessions":1}}`)
	}))
	defer srv.Close()

	resp, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "auth",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("got %d results, want 3", len(resp.Results))
	}

	// Checkpoint
	if resp.Results[0].Checkpoint == nil {
		t.Fatal("result[0] checkpoint is nil")
	}
	if resp.Results[0].Checkpoint.ID != testCPID {
		t.Errorf("checkpoint ID = %q", resp.Results[0].Checkpoint.ID)
	}

	// Commit
	if resp.Results[1].Commit == nil {
		t.Fatal("result[1] commit is nil")
	}
	if resp.Results[1].Commit.CommitSHA != "abc1234567890" {
		t.Errorf("commit SHA = %q", resp.Results[1].Commit.CommitSHA)
	}
	if resp.Results[1].Commit.Additions != 10 {
		t.Errorf("commit additions = %d", resp.Results[1].Commit.Additions)
	}

	// Session
	if resp.Results[2].Session == nil {
		t.Fatal("result[2] session is nil")
	}
	if resp.Results[2].Session.SessionID != "ss1" {
		t.Errorf("session ID = %q", resp.Results[2].Session.SessionID)
	}
	if resp.Results[2].Session.StepCount != 5 {
		t.Errorf("session steps = %d", resp.Results[2].Session.StepCount)
	}

	// Counts
	if resp.Counts == nil {
		t.Fatal("counts is nil")
	}
	if resp.Counts.Checkpoints != 1 || resp.Counts.Commits != 1 || resp.Counts.Sessions != 1 {
		t.Errorf("counts = %+v", resp.Counts)
	}
}

func TestSearch_ResultAccessors(t *testing.T) {
	t.Parallel()

	cp := Result{
		Type:       TypeCheckpoint,
		Checkpoint: &CheckpointResult{ID: testCPID, Org: "o", Repo: "r", Branch: "main", Author: "alice", CreatedAt: "2026-01-01T00:00:00Z", Prompt: "fix bug"},
	}
	if cp.ResultOrg() != "o" {
		t.Errorf("ResultOrg = %q", cp.ResultOrg())
	}
	if cp.ResultTitle() != "fix bug" {
		t.Errorf("ResultTitle = %q", cp.ResultTitle())
	}
	if cp.ResultID() != testCPID {
		t.Errorf("ResultID = %q", cp.ResultID())
	}

	cm := Result{
		Type:   TypeCommit,
		Commit: &CommitResult{CommitSHA: "abc123", CommitSubject: "fix: bug", Org: "o", Repo: "r", Branch: "dev", Author: "bob"},
	}
	if cm.ResultTitle() != "fix: bug" {
		t.Errorf("commit ResultTitle = %q", cm.ResultTitle())
	}
	if cm.ResultID() != "abc123" {
		t.Errorf("commit ResultID = %q", cm.ResultID())
	}

	ss := Result{
		Type:    TypeSession,
		Session: &SessionResult{SessionID: "ss1", DisplayName: "Debug session", Org: "o", Repo: "r"},
	}
	if ss.ResultTitle() != "Debug session" {
		t.Errorf("session ResultTitle = %q", ss.ResultTitle())
	}
	if ss.ResultID() != "ss1" {
		t.Errorf("session ResultID = %q", ss.ResultID())
	}
}

func TestSearch_ResultJSONRoundTrip(t *testing.T) {
	t.Parallel()

	original := Result{
		Type: TypeCheckpoint,
		Checkpoint: &CheckpointResult{
			ID:        testCPID,
			Prompt:    "fix bug",
			Branch:    "main",
			Org:       "o",
			Repo:      "r",
			Author:    "alice",
			CreatedAt: "2026-01-01T00:00:00Z",
		},
		Meta: Meta{MatchType: "keyword", Score: 0.5},
	}

	data, err := json.Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}

	// Verify wire format has "type", "data", "searchMeta" keys
	if !strings.Contains(string(data), `"type":"checkpoint"`) {
		t.Errorf("JSON missing type: %s", data)
	}
	if !strings.Contains(string(data), `"data":{`) {
		t.Errorf("JSON missing data: %s", data)
	}
	if !strings.Contains(string(data), `"searchMeta":{`) {
		t.Errorf("JSON missing searchMeta: %s", data)
	}

	// Round-trip
	var decoded Result
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != TypeCheckpoint {
		t.Errorf("decoded type = %q", decoded.Type)
	}
	if decoded.Checkpoint == nil || decoded.Checkpoint.ID != testCPID {
		t.Errorf("decoded checkpoint = %+v", decoded.Checkpoint)
	}
	if decoded.Meta.Score != 0.5 {
		t.Errorf("decoded score = %f", decoded.Meta.Score)
	}
}

func TestSearch_FilterParams(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
		Author:      testAuthor,
		Date:        testDateWeek,
	})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Query().Get("author") != testAuthor {
		t.Errorf("author = %s, want %q", capturedReq.URL.Query().Get("author"), testAuthor)
	}
	if capturedReq.URL.Query().Get("date") != testDateWeek {
		t.Errorf("date = %s, want 'week'", capturedReq.URL.Query().Get("date"))
	}
}

func TestSearch_ExplicitRepoParam(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "default-owner",
		Repo:        "default-repo",
		Query:       "q",
		Repos:       []string{"owner-one/repo-a"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := capturedReq.URL.Query()["repo"]; len(got) != 1 || got[0] != "owner-one/repo-a" {
		t.Errorf("repo params = %v, want %v", got, []string{"owner-one/repo-a"})
	}
}

func TestSearch_DefaultRepoParam(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "default-owner",
		Repo:        "default-repo",
		Query:       "q",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := capturedReq.URL.Query()["repo"]; len(got) != 1 || got[0] != "default-owner/default-repo" {
		t.Errorf("repo params = %v, want %v", got, []string{"default-owner/default-repo"})
	}
}

func TestSearch_AllReposFilterOmitsRepoParam(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "default-owner",
		Repo:        "default-repo",
		Query:       "q",
		Repos:       []string{AllReposFilter},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := capturedReq.URL.Query()["repo"]; len(got) != 0 {
		t.Errorf("repo params = %v, want omitted for all-repos search", got)
	}
}

func TestSearch_AllReposFlagOmitsRepoParam(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "default-owner",
		Repo:        "default-repo",
		Query:       "q",
		AllRepos:    true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := capturedReq.URL.Query()["repo"]; len(got) != 0 {
		t.Errorf("repo params = %v, want omitted for AllRepos=true", got)
	}
}

func TestSearch_ExplicitRepoWinsOverAllRepos(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	// --all-repos alongside an explicit owner/name filter must scope to the
	// explicit repo (the more specific filter wins), not search all repos.
	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "default-owner",
		Repo:        "default-repo",
		Query:       "q",
		AllRepos:    true,
		Repos:       []string{"owner/explicit"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := capturedReq.URL.Query()["repo"]; len(got) != 1 || got[0] != "owner/explicit" {
		t.Errorf("repo params = %v, want [owner/explicit]", got)
	}
}

func TestSearch_MultipleExplicitReposRejected(t *testing.T) {
	t.Parallel()

	_, err := Search(context.Background(), Config{
		ServiceURL:  "http://example.com",
		GitHubToken: "tok",
		Owner:       "default-owner",
		Repo:        "default-repo",
		Query:       "q",
		Repos:       []string{"owner-one/repo-a", "owner-two/repo-b"},
	})
	if err == nil {
		t.Fatal("expected error for multiple explicit repo filters")
	}
	if got := err.Error(); got != "only one explicit repo filter is currently supported" {
		t.Errorf("error = %q", got)
	}
}

func TestSearch_PageParam(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 2}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
		Page:        2,
	})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Query().Get("page") != "2" {
		t.Errorf("page = %s, want '2'", capturedReq.URL.Query().Get("page"))
	}
}

func TestSearch_ZeroPageOmitsParam(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Query().Has("page") {
		t.Error("page param should be omitted when zero")
	}
}

func TestSearch_EmptyFiltersOmitParams(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Query().Has("author") {
		t.Error("author param should be omitted when empty")
	}
	if capturedReq.URL.Query().Has("date") {
		t.Error("date param should be omitted when empty")
	}
}

// -- HasFilters tests --

func TestConfig_HasFilters(t *testing.T) {
	t.Parallel()

	if (Config{}).HasFilters() {
		t.Error("empty config should not have filters")
	}
	if !(Config{Author: "alice"}).HasFilters() {
		t.Error("config with Author should have filters")
	}
	if !(Config{Date: testDateWeek}).HasFilters() {
		t.Error("config with Date should have filters")
	}
	if !(Config{Repos: []string{"entirehq/entire.io"}}).HasFilters() {
		t.Error("config with Repos should have filters")
	}
	if !(Config{AllRepos: true}).HasFilters() {
		t.Error("config with AllRepos should have filters")
	}
	if !(Config{Author: "alice", Date: testDateWeek}).HasFilters() {
		t.Error("config with both should have filters")
	}
}

// -- ParseSearchInput tests --

const testQuery = "auth"
const testAuthor = "alice"
const testDateWeek = "week"

func TestParseSearchInput_QueryOnly(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput("fix auth bug")
	if p.Query != "fix auth bug" {
		t.Errorf("query = %q, want 'fix auth bug'", p.Query)
	}
	if p.Author != "" || p.Date != "" {
		t.Error("expected no filters")
	}
}

func TestParseSearchInput_AuthorFilter(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput(testQuery + " author:" + testAuthor)
	if p.Query != testQuery {
		t.Errorf("query = %q, want %q", p.Query, testQuery)
	}
	if p.Author != testAuthor {
		t.Errorf("author = %q, want %q", p.Author, testAuthor)
	}
}

func TestParseSearchInput_DateFilter(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput(testQuery + " date:week")
	if p.Query != testQuery {
		t.Errorf("query = %q, want %q", p.Query, testQuery)
	}
	if p.Date != testDateWeek {
		t.Errorf("date = %q, want 'week'", p.Date)
	}
}

func TestParseSearchInput_BothFilters(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput(testQuery + " author:" + testAuthor + " date:month")
	if p.Query != testQuery {
		t.Errorf("query = %q, want %q", p.Query, testQuery)
	}
	if p.Author != testAuthor {
		t.Errorf("author = %q, want %q", p.Author, testAuthor)
	}
	if p.Date != "month" {
		t.Errorf("date = %q, want 'month'", p.Date)
	}
}

func TestParseSearchInput_RepoFilter(t *testing.T) {
	t.Parallel()

	p := ParseSearchInput("fix auth repo:entirehq/entire.io")
	if p.Query != "fix auth" {
		t.Errorf("query = %q, want %q", p.Query, "fix auth")
	}
	if got := p.Repos; len(got) != 1 || got[0] != "entirehq/entire.io" {
		t.Errorf("repos = %v, want %v", got, []string{"entirehq/entire.io"})
	}
}

func TestParseSearchInput_RepoOnly(t *testing.T) {
	t.Parallel()

	p := ParseSearchInput("repo:entirehq/entire.io")
	if p.Query != "" {
		t.Errorf("query = %q, want empty", p.Query)
	}
	if got := p.Repos; len(got) != 1 || got[0] != "entirehq/entire.io" {
		t.Errorf("repos = %v, want %v", got, []string{"entirehq/entire.io"})
	}
}

func TestParseSearchInput_AllReposFilter(t *testing.T) {
	t.Parallel()

	p := ParseSearchInput("repo:*")
	if p.Query != "" {
		t.Errorf("query = %q, want empty", p.Query)
	}
	if got := p.Repos; len(got) != 1 || got[0] != AllReposFilter {
		t.Errorf("repos = %v, want %v", got, []string{AllReposFilter})
	}
}

func TestValidateRepoFilters_RejectsMultipleRepos(t *testing.T) {
	t.Parallel()

	err := ValidateRepoFilters([]string{"entirehq/entire.io", "entireio/cli"})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if got := err.Error(); got != "only one explicit repo filter is currently supported" {
		t.Errorf("error = %q", got)
	}
}

func TestValidateRepoFilters_RejectsInvalidRepoValue(t *testing.T) {
	t.Parallel()

	err := ValidateRepoFilters([]string{"AGENTS.md"})
	if err == nil {
		t.Fatal("expected validation error")
	}
	want := "invalid repo filter \"AGENTS.md\": expected owner/name or *; if you meant all repos, quote the asterisk: --repo '*'"
	if got := err.Error(); got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

func TestParseSearchInput_QuotedAuthor(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput(`author:"` + testAuthor + ` smith" fix bug`)
	if p.Author != testAuthor+" smith" {
		t.Errorf("author = %q, want %q", p.Author, testAuthor+" smith")
	}
	if p.Query != "fix bug" {
		t.Errorf("query = %q, want 'fix bug'", p.Query)
	}
}

func TestParseSearchInput_QuotedDate(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput(`date:"week"`)
	if p.Date != testDateWeek {
		t.Errorf("date = %q, want 'week' (quotes should be stripped)", p.Date)
	}
}

func TestParseSearchInput_FiltersOnly(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput("author:bob")
	if p.Query != "" {
		t.Errorf("query = %q, want empty", p.Query)
	}
	if p.Author != "bob" {
		t.Errorf("author = %q, want 'bob'", p.Author)
	}
}

func TestParseSearchInput_Empty(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput("")
	if p.Query != "" || p.Author != "" || p.Date != "" || len(p.Repos) != 0 {
		t.Error("expected all empty for empty input")
	}
}
