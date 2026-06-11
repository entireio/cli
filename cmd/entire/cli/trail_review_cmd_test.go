package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/spf13/cobra"
)

const (
	trailReviewApplyOriginalContent = "hello\nold\n"
	trailReviewTestCommentID        = "cmt_1"
)

func TestTrailCommandSurfaceUsesFindings(t *testing.T) {
	t.Parallel()
	trailCmd := newTrailCmd()
	children := map[string]*cobra.Command{}
	for _, child := range trailCmd.Commands() {
		children[child.Name()] = child
	}
	findingCmd := children["finding"]
	if findingCmd == nil {
		t.Fatal("trail command did not register finding subcommand")
	}
	if children["review"] != nil {
		t.Fatal("trail command should not register review subcommand")
	}

	subcommands := map[string]bool{}
	for _, child := range findingCmd.Commands() {
		subcommands[child.Name()] = true
	}
	for _, required := range []string{"list", "add", "show", "apply", "resolve", "dismiss", "reopen", "watch"} {
		if !subcommands[required] {
			t.Fatalf("trail finding missing %q subcommand", required)
		}
	}
	for _, removed := range []string{"start", "comments", "approve", "request-changes"} {
		if subcommands[removed] {
			t.Fatalf("trail finding should not register removed %q subcommand", removed)
		}
	}
}

func TestTrailCommandRejectsRemovedReviewCommand(t *testing.T) {
	t.Parallel()
	cmd := newTrailCmd()
	cmd.SetArgs([]string{"review"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected removed trail review command to error")
	}
}

// Not parallel: uses t.Chdir() to point remote resolution at a fake repo.
func TestResolveTrailReviewTargetRejectsUnsupportedForge(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	cmd := exec.CommandContext(context.Background(), "git", "remote", "add", "origin", "git@gitlab.com:acme/my-app.git")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if err := cmd.Run(); err != nil {
		t.Fatalf("git remote add: %v", err)
	}
	t.Chdir(repoDir)

	_, err := resolveTrailReviewTarget(context.Background(), api.NewClient("tok"), "")
	if err == nil {
		t.Fatal("expected error for gitlab.com origin, got nil")
	}
	if !strings.Contains(err.Error(), "not on a forge supported by Entire trails") {
		t.Fatalf("error message does not mention unsupported forge: %v", err)
	}
}

func TestTrailReviewCommentsPath(t *testing.T) {
	t.Parallel()
	got := trailReviewCommentsPath("trail id/with slash", trailReviewListOptions{
		Status:           "open,resolved",
		Severity:         "high,medium",
		Stale:            "any",
		IncludeDismissed: true,
		Limit:            25,
		Offset:           50,
	})
	want := "/api/v1/trails/trail%20id%2Fwith%20slash/reviews/comments?include_dismissed=true&limit=25&offset=50&severity=high%2Cmedium&status=open%2Cresolved"
	if got != want {
		t.Fatalf("trailReviewCommentsPath = %q, want %q", got, want)
	}
}

func TestParseTrailSelectorAndCommentID(t *testing.T) {
	t.Parallel()
	selector, commentID, err := parseTrailSelectorAndCommentID([]string{trailReviewTestCommentID}, "425")
	if err != nil {
		t.Fatalf("parseTrailSelectorAndCommentID with --trail: %v", err)
	}
	if selector != "425" || commentID != trailReviewTestCommentID {
		t.Fatalf("selector=%q commentID=%q, want 425/cmt_1", selector, commentID)
	}

	selector, commentID, err = parseTrailSelectorAndCommentID([]string{"feat/review", "cmt_2"}, "")
	if err != nil {
		t.Fatalf("parseTrailSelectorAndCommentID positional: %v", err)
	}
	if selector != "feat/review" || commentID != "cmt_2" {
		t.Fatalf("selector=%q commentID=%q, want feat/review/cmt_2", selector, commentID)
	}

	if _, _, err := parseTrailSelectorAndCommentID([]string{"425", trailReviewTestCommentID}, "trl_1"); err == nil {
		t.Fatal("expected error when both positional trail and --trail are provided")
	}
}

func TestLoadTrailReviewCommentPatchFile(t *testing.T) {
	t.Parallel()
	opts, err := loadTrailReviewCommentPatchFile(trailReviewCommentAddOptions{PatchFile: "-"}, strings.NewReader("diff --git a/file.txt b/file.txt\n"))
	if err != nil {
		t.Fatalf("loadTrailReviewCommentPatchFile: %v", err)
	}
	if opts.Patch != "diff --git a/file.txt b/file.txt\n" {
		t.Fatalf("Patch = %q", opts.Patch)
	}

	if _, err := loadTrailReviewCommentPatchFile(trailReviewCommentAddOptions{Patch: "inline", PatchFile: "-"}, strings.NewReader("patch")); err == nil {
		t.Fatal("expected error when --patch and --patch-file are both provided")
	}
}

func TestBuildTrailReviewCommentCreateRequest(t *testing.T) {
	t.Parallel()
	req, err := buildTrailReviewCommentCreateRequest(trailReviewCommentAddOptions{
		Title:       "Missing expiry skew handling",
		Body:        "Token refresh should allow clock skew.",
		Severity:    "HIGH",
		Confidence:  0.94,
		FilePath:    "src/auth/session.ts",
		StartLine:   88,
		EndLine:     91,
		ClientID:    "agent-run-1:finding-7",
		Instruction: "Allow a five minute skew.",
	})
	if err != nil {
		t.Fatalf("buildTrailReviewCommentCreateRequest: %v", err)
	}
	if req.Body != "Token refresh should allow clock skew." {
		t.Fatalf("Body = %q", req.Body)
	}
	if req.Title == nil || *req.Title != "Missing expiry skew handling" {
		t.Fatalf("Title = %#v", req.Title)
	}
	if req.Severity == nil || *req.Severity != trailReviewSeverityHigh {
		t.Fatalf("Severity = %#v", req.Severity)
	}
	if req.Confidence == nil || *req.Confidence != 0.94 {
		t.Fatalf("Confidence = %#v", req.Confidence)
	}
	if req.ClientID == nil || *req.ClientID != "agent-run-1:finding-7" {
		t.Fatalf("ClientID = %#v", req.ClientID)
	}
	if req.Location.Granularity != "range" || req.Location.FilePath == nil || *req.Location.FilePath != "src/auth/session.ts" {
		t.Fatalf("Location = %#v", req.Location)
	}
	if req.Location.StartLine == nil || *req.Location.StartLine != 88 || req.Location.EndLine == nil || *req.Location.EndLine != 91 {
		t.Fatalf("Location lines = %#v", req.Location)
	}
	if len(req.SuggestedChanges) != 1 || req.SuggestedChanges[0].ChangeType != "manual_instruction" {
		t.Fatalf("SuggestedChanges = %#v", req.SuggestedChanges)
	}
}

func TestCreateTrailReviewCommentPostsTrailScopedPath(t *testing.T) {
	var gotBody api.TrailReviewCommentCreateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/trails/trl_1/reviews/comments" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		encodeTrailReviewTestJSON(t, w, api.TrailReviewComment{ID: trailReviewTestCommentID, TrailID: "trl_1", Status: trailReviewStatusOpen})
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	created, err := createTrailReviewComment(context.Background(), client, "trl_1", api.TrailReviewCommentCreateRequest{
		Body:     "body",
		ClientID: trailReviewStrPtr("agent-run-1:finding-1"),
		Location: api.TrailReviewLocationCreateRequest{Granularity: "whole_change"},
	})
	if err != nil {
		t.Fatalf("createTrailReviewComment: %v", err)
	}
	if created.ID != trailReviewTestCommentID {
		t.Fatalf("created.ID = %q", created.ID)
	}
	if gotBody.Body != "body" || gotBody.ClientID == nil || *gotBody.ClientID != "agent-run-1:finding-1" {
		t.Fatalf("request body = %#v", gotBody)
	}
}

func TestPrintTrailReviewDashboard(t *testing.T) {
	t.Parallel()
	high := trailReviewSeverityHigh
	medium := trailReviewSeverityMedium
	path := "src/auth/session.ts"
	line := 88
	comments := []api.TrailReviewComment{
		{
			ID:       "comment-high-123",
			ReviewID: "review-1",
			Title:    trailReviewStrPtr("Missing expiry skew handling"),
			Severity: &high,
			Status:   trailReviewStatusOpen,
			Location: api.TrailReviewLocation{
				Granularity: "line",
				FilePath:    &path,
				StartLine:   &line,
			},
		},
		{
			ID:       "comment-medium-123",
			ReviewID: "review-1",
			Title:    trailReviewStrPtr("Retry loop can spin forever"),
			Severity: &medium,
			Status:   trailReviewStatusResolved,
			Location: api.TrailReviewLocation{Granularity: "whole_change"},
		},
	}
	var out strings.Builder
	printTrailReviewDashboard(&out, trailReviewTarget{Trail: api.TrailResource{
		ID:     "trl_1",
		Number: 42,
		Title:  "Add token refresh",
		Status: "in_review",
		Branch: "feat/token-refresh",
		Base:   "main",
	}}, comments, false, defaultTrailReviewListOptions(), countTrailReviewComments(comments))
	text := out.String()
	for _, want := range []string{
		"Trail #42  Add token refresh",
		"Open findings: 1  high 1  medium 0  low 0",
		"Resolved: 1",
		"High",
		"src/auth/session.ts:88",
		"Missing expiry skew handling",
		"Actions:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, text)
		}
	}
}

func TestPrintTrailReviewDashboard_UsesSeparateCountsWhenFilteredCommentsEmpty(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	counts := countTrailReviewComments([]api.TrailReviewComment{
		{ID: "resolved-1", Status: trailReviewStatusResolved},
		{ID: "dismissed-1", Status: trailReviewStatusDismissed, StaleOutcome: "stale"},
	})
	printTrailReviewDashboard(&out, trailReviewTarget{Trail: api.TrailResource{
		ID:     "trl_1",
		Number: 42,
		Title:  "Add token refresh",
		Status: "in_review",
		Branch: "feat/token-refresh",
		Base:   "main",
	}}, nil, false, defaultTrailReviewListOptions(), counts)
	text := out.String()
	for _, want := range []string{
		"Open findings: 0  high 0  medium 0  low 0",
		"Resolved: 1        Dismissed: 1     Stale: 1",
		"No findings match the current filters.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, text)
		}
	}
}

func TestFetchTrailReviewCommentsAndPatchStatus(t *testing.T) {
	var gotPatchBody api.TrailReviewCommentPatchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trails/trl_1/reviews/comments":
			if got := r.URL.Query().Get("status"); got != "open" {
				t.Fatalf("status query = %q, want open", got)
			}
			encodeTrailReviewTestJSON(t, w, api.TrailReviewCommentsResponse{Comments: []api.TrailReviewComment{
				{ID: trailReviewTestCommentID, TrailID: "trl_1", ReviewID: "rvw_1", Status: trailReviewStatusOpen, Location: api.TrailReviewLocation{Granularity: "whole_change"}},
			}})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/trails/trl_1/reviews/rvw_1/comments/cmt_1":
			if err := json.NewDecoder(r.Body).Decode(&gotPatchBody); err != nil {
				t.Fatalf("decode patch body: %v", err)
			}
			encodeTrailReviewTestJSON(t, w, api.TrailReviewComment{ID: trailReviewTestCommentID, TrailID: "trl_1", ReviewID: "rvw_1", Status: trailReviewStatusResolved})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	comments, hasMore, err := fetchTrailReviewComments(context.Background(), client, "trl_1", defaultTrailReviewListOptions())
	if err != nil {
		t.Fatalf("fetchTrailReviewComments: %v", err)
	}
	if hasMore || len(comments) != 1 || comments[0].ID != trailReviewTestCommentID {
		t.Fatalf("comments = %#v, hasMore=%v", comments, hasMore)
	}
	updated, err := patchTrailReviewCommentStatus(context.Background(), client, "trl_1", comments[0], trailReviewStatusResolved, "fixed")
	if err != nil {
		t.Fatalf("patchTrailReviewCommentStatus: %v", err)
	}
	if updated.Status != trailReviewStatusResolved {
		t.Fatalf("updated status = %q", updated.Status)
	}
	if gotPatchBody.Status != trailReviewStatusResolved || gotPatchBody.StatusReason == nil || *gotPatchBody.StatusReason != "fixed" {
		t.Fatalf("patch body = %#v", gotPatchBody)
	}
}

func TestFetchTrailReviewStateFollowsCursor(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/trails/trl_1/reviews/rvw_1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		requests++
		switch r.URL.Query().Get("cursor") {
		case "":
			next := "cursor-2"
			encodeTrailReviewTestJSON(t, w, api.TrailReviewStateResponse{
				Review:      api.TrailReview{ID: "rvw_1"},
				CodeVersion: api.TrailReviewCodeVersion{ID: "cv_1"},
				Comments:    []api.TrailReviewComment{{ID: trailReviewTestCommentID}},
				NextCursor:  &next,
			})
		case "cursor-2":
			encodeTrailReviewTestJSON(t, w, api.TrailReviewStateResponse{
				Review:      api.TrailReview{ID: "rvw_1"},
				CodeVersion: api.TrailReviewCodeVersion{ID: "cv_1"},
				Comments:    []api.TrailReviewComment{{ID: "cmt_2"}},
			})
		default:
			t.Fatalf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	state, err := fetchTrailReviewState(context.Background(), client, "trl_1", "rvw_1")
	if err != nil {
		t.Fatalf("fetchTrailReviewState: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if len(state.Comments) != 2 || state.Comments[0].ID != trailReviewTestCommentID || state.Comments[1].ID != "cmt_2" {
		t.Fatalf("comments = %#v", state.Comments)
	}
	if state.NextCursor != nil {
		t.Fatalf("NextCursor = %#v, want nil after final page", state.NextCursor)
	}
}

func TestApplyTrailReviewSuggestions_AppliesUnifiedDiff(t *testing.T) {
	repo := newTrailReviewApplyRepo(t)
	writeTrailReviewApplyFile(t, repo, "file.txt")
	comment := trailReviewApplyComment(trailReviewPatch("file.txt", "old"))

	applied, err := applyTrailReviewSuggestions(context.Background(), comment, false, io.Discard)
	if err != nil {
		t.Fatalf("applyTrailReviewSuggestions: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied = %d, want 1", applied)
	}
	if got := readTrailReviewApplyFile(t, repo, "file.txt"); got != "hello\nnew\n" {
		t.Fatalf("file content = %q", got)
	}
}

func TestApplyTrailReviewSuggestions_CheckDoesNotModifyWorktree(t *testing.T) {
	repo := newTrailReviewApplyRepo(t)
	writeTrailReviewApplyFile(t, repo, "file.txt")
	comment := trailReviewApplyComment(trailReviewPatch("file.txt", "old"))

	applied, err := applyTrailReviewSuggestions(context.Background(), comment, true, io.Discard)
	if err != nil {
		t.Fatalf("applyTrailReviewSuggestions --check: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied = %d, want 1", applied)
	}
	if got := readTrailReviewApplyFile(t, repo, "file.txt"); got != trailReviewApplyOriginalContent {
		t.Fatalf("file content = %q", got)
	}
}

func TestApplyTrailReviewSuggestions_FailureDoesNotPartiallyApply(t *testing.T) {
	repo := newTrailReviewApplyRepo(t)
	writeTrailReviewApplyFile(t, repo, "a.txt")
	writeTrailReviewApplyFile(t, repo, "b.txt")
	comment := trailReviewApplyComment(
		trailReviewPatch("a.txt", "old"),
		trailReviewPatch("b.txt", "missing"),
	)

	applied, err := applyTrailReviewSuggestions(context.Background(), comment, false, io.Discard)
	if err == nil {
		t.Fatal("applyTrailReviewSuggestions expected error")
	}
	if applied != 0 {
		t.Fatalf("applied = %d, want 0", applied)
	}
	if got := readTrailReviewApplyFile(t, repo, "a.txt"); got != trailReviewApplyOriginalContent {
		t.Fatalf("a.txt content = %q", got)
	}
	if got := readTrailReviewApplyFile(t, repo, "b.txt"); got != trailReviewApplyOriginalContent {
		t.Fatalf("b.txt content = %q", got)
	}
}

func TestApplyTrailReviewSuggestions_RejectsGitMetadataPaths(t *testing.T) {
	_ = newTrailReviewApplyRepo(t)
	comment := trailReviewApplyComment(`diff --git a/.git/config b/.git/config
--- a/.git/config
+++ b/.git/config
@@ -1,1 +1,1 @@
-old
+new
`)

	_, err := applyTrailReviewSuggestions(context.Background(), comment, false, io.Discard)
	if err == nil {
		t.Fatal("applyTrailReviewSuggestions expected unsafe path error")
	}
	if !strings.Contains(err.Error(), ".git") {
		t.Fatalf("error = %v, want .git mention", err)
	}
}

func newTrailReviewApplyRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Bare `git init` rather than testutil.InitRepo: these tests apply patches to
	// the working tree without committing, so no user/GPG config is needed, and we
	// must avoid testutil's core.autocrlf=true which rewrites patched LF to CRLF.
	runTrailReviewApplyGit(t, dir, "init")
	paths.ClearWorktreeRootCache()
	t.Chdir(dir)
	t.Cleanup(paths.ClearWorktreeRootCache)
	return dir
}

func writeTrailReviewApplyFile(t *testing.T, repo, rel string) {
	t.Helper()
	path := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(trailReviewApplyOriginalContent), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func readTrailReviewApplyFile(t *testing.T, repo, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func runTrailReviewApplyGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func trailReviewApplyComment(patches ...string) api.TrailReviewComment {
	changes := make([]api.TrailReviewSuggestedChange, len(patches))
	for i, patch := range patches {
		changes[i] = api.TrailReviewSuggestedChange{
			ID:         "change-" + string(rune('a'+i)),
			ChangeType: "unified_diff",
			Patch:      trailReviewStrPtr(patch),
		}
	}
	return api.TrailReviewComment{ID: trailReviewTestCommentID, SuggestedChanges: changes}
}

func trailReviewPatch(file, oldText string) string {
	return "diff --git a/" + file + " b/" + file + "\n" +
		"--- a/" + file + "\n" +
		"+++ b/" + file + "\n" +
		"@@ -1,2 +1,2 @@\n" +
		" hello\n" +
		"-" + oldText + "\n" +
		"+new\n"
}

func encodeTrailReviewTestJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func trailReviewStrPtr(s string) *string { return &s }
