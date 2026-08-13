package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
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
	_, _, err := ParseGitHubRemote("   ")
	if err == nil || err.Error() != "empty remote URL" {
		t.Errorf("expected 'empty remote URL' for blank input, got %v", err)
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

func TestParseGitHubRemote_RejectsExtraPathSegments(t *testing.T) {
	t.Parallel()
	for _, remoteURL := range []string{
		"https://github.com/entirehq/entire.io/extra.git",
		"entire://aws-us-east-2.entire.io/gh/entirehq/entire.io/extra",
	} {
		if _, _, err := ParseGitHubRemote(remoteURL); err == nil {
			t.Errorf("expected error for malformed remote %q, got none", remoteURL)
		}
	}
}

func TestParseGitHubRemote_EntireMirror(t *testing.T) {
	t.Parallel()
	owner, repo, err := ParseGitHubRemote("entire://aws-us-east-2.entire.io/gh/entirehq/entire.io")
	if err != nil {
		t.Fatal(err)
	}
	if owner != testOwner || repo != testRepo {
		t.Errorf("got %s/%s, want %s/%s", owner, repo, testOwner, testRepo)
	}
}

// -- Search() tests --

func TestCellV4_ErrorJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid token"}) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "q"}, nil)
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if got := err.Error(); got != "search service error (401): Invalid token" {
		t.Errorf("error = %q, want 'search service error (401): Invalid token'", got)
	}
}

func TestCellV4_ErrorRawBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("upstream timeout")) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "q"}, nil)
	if err == nil {
		t.Fatal("expected error for 502")
	}
	if got := err.Error(); got != "search service returned 502: upstream timeout" {
		t.Errorf("error = %q", got)
	}
}

func TestCellV4_HTMLResponseNon200(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>Bad Gateway</html>")) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "q"}, nil)
	if err == nil {
		t.Fatal("expected error for HTML response")
	}
	want := "search service returned 502: <html>Bad Gateway</html>"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestCellV4_HTMLResponseOn200(t *testing.T) {
	t.Parallel()

	htmlBody := "<!DOCTYPE html><html><body>Website</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(htmlBody)) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "q"}, nil)
	if err == nil {
		t.Fatal("expected error for HTML response on 200")
	}
	if !strings.Contains(err.Error(), htmlBody) {
		t.Errorf("error should contain full body, got: %q", err.Error())
	}
}

func TestCellV4_ErrorFieldOn200(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Response{Error: "user not found in Entire"}) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "q"}, nil)
	if err == nil {
		t.Fatal("expected error when server returns 200 with error field")
	}
	if !strings.Contains(err.Error(), "user not found") {
		t.Errorf("error = %q, want message containing 'user not found'", err.Error())
	}
}

func TestCellV4_SuccessWithResults(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, `{"results":[{"type":"checkpoint","data":{"id":"abc123def456","branch":"main","prompt":"add auth middleware","author":"alice","createdAt":"2026-01-13T12:00:00Z","org":"","repo":"","filesTouched":[]},"searchMeta":{"score":0.042,"matchType":"both"}}],"total":1,"page":1}`)
	}))
	defer srv.Close()

	resp, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "test"}, nil)
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

func TestCellV4_SuccessWithMultipleTypes(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, `{"results":[{"type":"checkpoint","data":{"id":"cp1","prompt":"fix bug","branch":"main","org":"o","repo":"r","author":"alice","createdAt":"2026-01-13T12:00:00Z","filesTouched":[]},"searchMeta":{"matchType":"keyword","score":0.5}},{"type":"commit","data":{"id":"cm1","commitSha":"abc1234567890","commitMessage":"fix: auth bug","commitSubject":"fix: auth bug","branch":"main","org":"o","repo":"r","author":"bob","createdAt":"2026-01-14T12:00:00Z","additions":10,"deletions":5,"filesChanged":3},"searchMeta":{"matchType":"semantic","score":0.3}},{"type":"session","data":{"sessionId":"ss1","displayName":"Debug auth","org":"o","repo":"r","createdAt":"2026-01-15T12:00:00Z","stepCount":5},"searchMeta":{"matchType":"both","score":0.4}}],"total":3,"page":1,"counts":{"repos":0,"checkpoints":1,"commits":1,"prs":0,"sessions":1}}`)
	}))
	defer srv.Close()

	resp, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "auth"}, nil)
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
	if cp.ResultRepo() != "r" {
		t.Errorf("ResultRepo = %q", cp.ResultRepo())
	}
	if cp.ResultBranch() != "main" {
		t.Errorf("ResultBranch = %q", cp.ResultBranch())
	}
	if cp.ResultCreatedAt() != "2026-01-01T00:00:00Z" {
		t.Errorf("ResultCreatedAt = %q", cp.ResultCreatedAt())
	}
	if cp.ResultAuthor() != "alice" {
		t.Errorf("ResultAuthor = %q", cp.ResultAuthor())
	}
	if cp.ResultTitle() != "fix bug" {
		t.Errorf("ResultTitle = %q", cp.ResultTitle())
	}
	if cp.ResultID() != testCPID {
		t.Errorf("ResultID = %q", cp.ResultID())
	}

	// AuthorUsername, when set and non-empty, wins over Author; a commit
	// subject wins over the prompt.
	const usernameOverride = "alice-gh"
	username := usernameOverride
	subject := "fix: the bug"
	cp.Checkpoint.AuthorUsername = &username
	cp.Checkpoint.CommitSubject = &subject
	if cp.ResultAuthor() != usernameOverride {
		t.Errorf("ResultAuthor with username = %q", cp.ResultAuthor())
	}
	if cp.ResultTitle() != "fix: the bug" {
		t.Errorf("ResultTitle with subject = %q", cp.ResultTitle())
	}

	cm := Result{
		Type:   TypeCommit,
		Commit: &CommitResult{CommitSHA: "abc123", CommitSubject: "fix: bug", Org: "o", Repo: "r", Branch: "dev", Author: "bob", CreatedAt: "2026-02-02T00:00:00Z"},
	}
	if cm.ResultTitle() != "fix: bug" {
		t.Errorf("commit ResultTitle = %q", cm.ResultTitle())
	}
	if cm.ResultID() != "abc123" {
		t.Errorf("commit ResultID = %q", cm.ResultID())
	}
	if cm.ResultRepo() != "r" {
		t.Errorf("commit ResultRepo = %q", cm.ResultRepo())
	}
	if cm.ResultBranch() != "dev" {
		t.Errorf("commit ResultBranch = %q", cm.ResultBranch())
	}
	if cm.ResultCreatedAt() != "2026-02-02T00:00:00Z" {
		t.Errorf("commit ResultCreatedAt = %q", cm.ResultCreatedAt())
	}
	if cm.ResultAuthor() != "bob" {
		t.Errorf("commit ResultAuthor = %q", cm.ResultAuthor())
	}

	branch := "feature"
	ss := Result{
		Type:    TypeSession,
		Session: &SessionResult{SessionID: "ss1", DisplayName: "Debug session", Org: "o", Repo: "r", Branch: &branch, CreatedAt: "2026-03-03T00:00:00Z"},
	}
	if ss.ResultTitle() != "Debug session" {
		t.Errorf("session ResultTitle = %q", ss.ResultTitle())
	}
	if ss.ResultID() != "ss1" {
		t.Errorf("session ResultID = %q", ss.ResultID())
	}
	if ss.ResultBranch() != "feature" {
		t.Errorf("session ResultBranch = %q", ss.ResultBranch())
	}
	if ss.ResultCreatedAt() != "2026-03-03T00:00:00Z" {
		t.Errorf("session ResultCreatedAt = %q", ss.ResultCreatedAt())
	}
	// Session author comes only from AuthorUsername.
	if ss.ResultAuthor() != "" {
		t.Errorf("session ResultAuthor without username = %q", ss.ResultAuthor())
	}
	ss.Session.AuthorUsername = &username
	if ss.ResultAuthor() != usernameOverride {
		t.Errorf("session ResultAuthor = %q", ss.ResultAuthor())
	}

	// Nil payloads and unknown types resolve to "" on every accessor.
	for _, r := range []Result{{Type: TypeCheckpoint}, {Type: TypeCommit}, {Type: TypeSession}, {Type: "repo"}} {
		if got := r.ResultOrg() + r.ResultRepo() + r.ResultBranch() + r.ResultCreatedAt() + r.ResultAuthor() + r.ResultID() + r.ResultTitle(); got != "" {
			t.Errorf("accessors on %q with nil payload = %q, want all empty", r.Type, got)
		}
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

// TestSearch_MetaDecodesRerankScore guards that query-serve's rerankScore is
// actually parsed off the wire — the cross-cell merge orders tier 0/1 by it, so
// a dropped field silently reverts the CLI to pre-rerank ordering.
func TestSearch_MetaDecodesRerankScore(t *testing.T) {
	t.Parallel()

	raw := `{"type":"commit","data":{"commitSha":"abc123","org":"o","repo":"r"},"searchMeta":{"matchType":"both","score":6,"tier":1,"rerankScore":0.42,"bm25Score":6}}`
	var r Result
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	if r.Meta.RerankScore == nil {
		t.Fatal("rerankScore decoded as nil, want 0.42")
	}
	if *r.Meta.RerankScore != 0.42 {
		t.Errorf("decoded rerankScore = %f, want 0.42", *r.Meta.RerankScore)
	}

	// Absent rerankScore must stay nil (a cell whose rerank fell back), not 0,
	// so rerankOf can distinguish "unscored" from a genuine 0 score.
	var noRerank Result
	if err := json.Unmarshal([]byte(`{"type":"commit","data":{"commitSha":"d","org":"o","repo":"r"},"searchMeta":{"score":1,"tier":1}}`), &noRerank); err != nil {
		t.Fatal(err)
	}
	if noRerank.Meta.RerankScore != nil {
		t.Errorf("absent rerankScore = %v, want nil", *noRerank.Meta.RerankScore)
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

func TestValidateRepoFilters_AllowsMultipleRepos(t *testing.T) {
	t.Parallel()

	if err := ValidateRepoFilters([]string{"entirehq/entire.io", "entireio/cli"}); err != nil {
		t.Errorf("expected multiple valid repo filters to be accepted, got: %v", err)
	}
}

func TestValidateRepoFilters_RejectsInvalidAmongMultiple(t *testing.T) {
	t.Parallel()

	err := ValidateRepoFilters([]string{"entireio/cli", "AGENTS.md"})
	if err == nil {
		t.Fatal("expected validation error for an invalid repo among valid ones")
	}
	if got := err.Error(); !strings.Contains(got, `invalid repo filter "AGENTS.md"`) {
		t.Errorf("error = %q, want it to name the invalid repo", got)
	}
}

func TestValidateRepoFilters_RejectsInvalidRepoValue(t *testing.T) {
	t.Parallel()

	err := ValidateRepoFilters([]string{"AGENTS.md"})
	if err == nil {
		t.Fatal("expected validation error")
	}
	want := "invalid repo filter \"AGENTS.md\": expected owner/name, gh/owner/repo, a repo ULID, or *; if you meant all repos, quote the asterisk: --repo '*'"
	if got := err.Error(); got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

// The CLI --repo help advertises gh/owner/repo, et/proj/repo, and raw ULIDs,
// and the semantic v4 lookup + code-search resolver both handle them. Validation
// must accept the same set so it never rejects a filter that would resolve
// downstream (ENT-1047 review finding).
func TestValidateRepoFilters_AcceptsAdvertisedFormats(t *testing.T) {
	t.Parallel()

	valid := []string{
		"entireio/cli",               // bare owner/name slug
		"gh/entireio/cli",            // GitHub prefixed path
		"et/proj/repo",               // Entire project prefixed path
		"git/owner/repo",             // generic git prefixed path
		"01ARZ3NDEKTSV4RRFFQ69G5FAV", // raw repo ULID (canonical)
		"*",                          // all-repos wildcard
	}
	for _, repo := range valid {
		if err := ValidateRepoFilters([]string{repo}); err != nil {
			t.Errorf("ValidateRepoFilters(%q) = %v, want nil", repo, err)
		}
	}
}

func TestValidateRepoFilters_RejectsMalformed(t *testing.T) {
	t.Parallel()

	invalid := []string{
		"AGENTS.md",  // bare filename, not a ULID or slug
		"owner/",     // empty name segment
		"/repo",      // empty owner segment
		"a/b/c/d",    // too many path segments
		"owner name", // contains a space
		"gh//repo",   // empty middle segment in a prefixed path
	}
	for _, repo := range invalid {
		if err := ValidateRepoFilters([]string{repo}); err == nil {
			t.Errorf("ValidateRepoFilters(%q) = nil, want validation error", repo)
		}
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
