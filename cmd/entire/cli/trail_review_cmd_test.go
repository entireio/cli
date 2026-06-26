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
	trailReviewTestStartPath        = "/api/v1/trails/trl_1/reviews"
	trailReviewTestCommentsPath     = "/api/v1/trails/trl_1/reviews/rvw_1/comments"
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
	if children["watch"] == nil {
		t.Fatal("trail command should register watch subcommand")
	}

	subcommands := map[string]bool{}
	for _, child := range findingCmd.Commands() {
		subcommands[child.Name()] = true
	}
	for _, required := range []string{"list", "add", "show", "update", "apply", "resolve", "dismiss", "reopen"} {
		if !subcommands[required] {
			t.Fatalf("trail finding missing %q subcommand", required)
		}
	}
	for _, removed := range []string{"start", "comments", "approve", "request-changes", "watch"} {
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
		Freshness:        "any",
		IncludeDismissed: true,
		Limit:            25,
		Offset:           50,
	})
	want := "/api/v1/trails/trail%20id%2Fwith%20slash/reviews/comments?include_dismissed=true&limit=25&offset=50&severity=high%2Cmedium&stale=any&status=open%2Cresolved"
	if got != want {
		t.Fatalf("trailReviewCommentsPath = %q, want %q", got, want)
	}
}

func TestNormalizeTrailReviewListOptionsIncludeDismissedBroadensDefaultStatus(t *testing.T) {
	t.Parallel()
	opts := defaultTrailReviewListOptions()
	opts.IncludeDismissed = true
	got, err := normalizeTrailReviewListOptions(opts)
	if err != nil {
		t.Fatalf("normalizeTrailReviewListOptions: %v", err)
	}
	if got.Status != trailReviewStatusAny {
		t.Fatalf("Status = %q, want %q", got.Status, trailReviewStatusAny)
	}

	opts = defaultTrailReviewListOptions()
	opts.IncludeDismissed = true
	opts.StatusChanged = true
	got, err = normalizeTrailReviewListOptions(opts)
	if err != nil {
		t.Fatalf("normalizeTrailReviewListOptions explicit status: %v", err)
	}
	if got.Status != trailReviewStatusOpen {
		t.Fatalf("explicit Status = %q, want open", got.Status)
	}
}

func TestNormalizeTrailReviewListOptionsRejectsInvalidFilters(t *testing.T) {
	t.Parallel()
	cases := []trailReviewListOptions{
		{Status: "open,nope", Freshness: trailReviewFreshnessAny, Limit: 1},
		{Status: trailReviewStatusAny, Severity: "urgent", Freshness: trailReviewFreshnessAny, Limit: 1},
		{Status: trailReviewStatusAny, Freshness: "old", Limit: 1},
		{Status: trailReviewStatusAny, Freshness: trailReviewFreshnessAny, Limit: 0},
		{Status: trailReviewStatusAny, Freshness: trailReviewFreshnessAny, Limit: 1, Offset: -1},
	}
	for _, opts := range cases {
		if _, err := normalizeTrailReviewListOptions(opts); err == nil {
			t.Fatalf("normalizeTrailReviewListOptions(%+v) succeeded, want error", opts)
		}
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

func TestBuildTrailReviewCommentPatchRequest(t *testing.T) {
	t.Parallel()

	req, err := buildTrailReviewCommentPatchRequest(trailReviewUpdateOptions{
		Body:              "Allow a five minute skew.",
		BodyChanged:       true,
		Severity:          "HIGH",
		SeverityChanged:   true,
		Confidence:        0.94,
		ConfidenceChanged: true,
	})
	if err != nil {
		t.Fatalf("buildTrailReviewCommentPatchRequest: %v", err)
	}
	if req.Title != nil {
		t.Fatalf("Title = %#v, want nil", req.Title)
	}
	if req.Body == nil || *req.Body != "Allow a five minute skew." {
		t.Fatalf("Body = %#v", req.Body)
	}
	if req.Severity == nil || *req.Severity != trailReviewSeverityHigh {
		t.Fatalf("Severity = %#v", req.Severity)
	}
	if req.Confidence == nil || *req.Confidence != 0.94 {
		t.Fatalf("Confidence = %#v", req.Confidence)
	}

	if _, err := buildTrailReviewCommentPatchRequest(trailReviewUpdateOptions{}); err == nil {
		t.Fatal("expected an error when no update fields are provided")
	}
	if _, err := buildTrailReviewCommentPatchRequest(trailReviewUpdateOptions{Severity: "urgent", SeverityChanged: true}); err == nil {
		t.Fatal("expected an error for invalid severity")
	}
	if _, err := buildTrailReviewCommentPatchRequest(trailReviewUpdateOptions{Body: " ", BodyChanged: true}); err == nil {
		t.Fatal("expected an error for empty body")
	}
	if _, err := buildTrailReviewCommentPatchRequest(trailReviewUpdateOptions{Severity: " ", SeverityChanged: true}); err == nil {
		t.Fatal("expected an error for empty severity")
	}
}

func TestBuildTrailReviewCommentInput(t *testing.T) {
	t.Parallel()
	input, err := buildTrailReviewCommentInput(trailReviewCommentAddOptions{
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
		t.Fatalf("buildTrailReviewCommentInput: %v", err)
	}
	if input.Body == nil || *input.Body != "Token refresh should allow clock skew." {
		t.Fatalf("Body = %#v", input.Body)
	}
	if input.Severity == nil || *input.Severity != trailReviewSeverityHigh {
		t.Fatalf("Severity = %#v", input.Severity)
	}
	if input.Confidence == nil || *input.Confidence != 0.94 {
		t.Fatalf("Confidence = %#v", input.Confidence)
	}
	if input.ClientID != "agent-run-1:finding-7" {
		t.Fatalf("ClientID = %q", input.ClientID)
	}
	if input.Location.Granularity != "range" || input.Location.FilePath == nil || *input.Location.FilePath != "src/auth/session.ts" {
		t.Fatalf("Location = %#v", input.Location)
	}
	if input.Location.StartLine == nil || *input.Location.StartLine != 88 || input.Location.EndLine == nil || *input.Location.EndLine != 91 {
		t.Fatalf("Location lines = %#v", input.Location)
	}
	if input.SuggestedChange == nil || input.SuggestedChange.ChangeType != "manual_instruction" {
		t.Fatalf("SuggestedChange = %#v", input.SuggestedChange)
	}
}

func TestBuildTrailReviewCommentInputGeneratesClientID(t *testing.T) {
	t.Parallel()
	input, err := buildTrailReviewCommentInput(trailReviewCommentAddOptions{Body: "finding body"})
	if err != nil {
		t.Fatalf("buildTrailReviewCommentInput: %v", err)
	}
	if input.ClientID == "" {
		t.Fatal("expected a generated client_id when --client-id is omitted")
	}
}

func TestCreateTrailReviewFindingStartsReviewThenPostsBatch(t *testing.T) {
	var (
		gotBatch    api.TrailReviewCommentBatchRequest
		startCalled bool
		batchCalled bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == trailReviewTestStartPath:
			startCalled = true
			encodeTrailReviewTestJSON(t, w, api.TrailReviewStartResponse{ReviewID: "rvw_1", TrailID: "trl_1"})
		case r.Method == http.MethodPost && r.URL.Path == trailReviewTestCommentsPath:
			batchCalled = true
			if err := json.NewDecoder(r.Body).Decode(&gotBatch); err != nil {
				t.Fatalf("decode batch body: %v", err)
			}
			encodeTrailReviewTestJSON(t, w, api.TrailReviewCommentBatchResponse{Results: []api.TrailReviewCommentBatchResult{{
				ClientID: "agent-run-1:finding-1",
				Status:   "created",
				Comment:  &api.TrailReviewComment{ID: trailReviewTestCommentID, TrailID: "trl_1", ReviewID: "rvw_1", Status: trailReviewStatusOpen},
			}}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	created, err := createTrailReviewFinding(context.Background(), client, "trl_1", api.TrailReviewCommentInput{
		ClientID: "agent-run-1:finding-1",
		Body:     trailReviewStrPtr("body"),
		Location: api.TrailReviewLocationCreateRequest{Granularity: "whole_change"},
	})
	if err != nil {
		t.Fatalf("createTrailReviewFinding: %v", err)
	}
	if !startCalled || !batchCalled {
		t.Fatalf("startCalled=%v batchCalled=%v (expected both)", startCalled, batchCalled)
	}
	if created.ID != trailReviewTestCommentID {
		t.Fatalf("created.ID = %q", created.ID)
	}
	if len(gotBatch.Comments) != 1 {
		t.Fatalf("batch comments = %#v, want 1", gotBatch.Comments)
	}
	if gotBatch.Comments[0].ClientID != "agent-run-1:finding-1" {
		t.Fatalf("batch client_id = %q", gotBatch.Comments[0].ClientID)
	}
	if gotBatch.Comments[0].Body == nil || *gotBatch.Comments[0].Body != "body" {
		t.Fatalf("batch body = %#v", gotBatch.Comments[0].Body)
	}
}

func TestCreateTrailReviewFindingsPostsOneBatch(t *testing.T) {
	var (
		gotBatch   api.TrailReviewCommentBatchRequest
		startCalls int
		batchCalls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == trailReviewTestStartPath:
			startCalls++
			encodeTrailReviewTestJSON(t, w, api.TrailReviewStartResponse{ReviewID: "rvw_1", TrailID: "trl_1", Limits: api.TrailReviewLimits{MaxCommentsPerBatch: 10}})
		case r.Method == http.MethodPost && r.URL.Path == trailReviewTestCommentsPath:
			batchCalls++
			if err := json.NewDecoder(r.Body).Decode(&gotBatch); err != nil {
				t.Fatalf("decode batch body: %v", err)
			}
			encodeTrailReviewTestJSON(t, w, api.TrailReviewCommentBatchResponse{Results: []api.TrailReviewCommentBatchResult{
				{ClientID: "c1", Status: "created", Comment: &api.TrailReviewComment{ID: "cm_1", TrailID: "trl_1", ReviewID: "rvw_1", Status: trailReviewStatusOpen}},
				{ClientID: "c2", Status: "created", Comment: &api.TrailReviewComment{ID: "cm_2", TrailID: "trl_1", ReviewID: "rvw_1", Status: trailReviewStatusOpen}},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	created, err := createTrailReviewFindings(context.Background(), client, "trl_1", []api.TrailReviewCommentInput{
		{ClientID: "c1", Body: trailReviewStrPtr("first"), Location: api.TrailReviewLocationCreateRequest{Granularity: "whole_change"}},
		{ClientID: "c2", Body: trailReviewStrPtr("second"), Location: api.TrailReviewLocationCreateRequest{Granularity: "whole_change"}},
	})
	if err != nil {
		t.Fatalf("createTrailReviewFindings: %v", err)
	}
	if startCalls != 1 || batchCalls != 1 {
		t.Fatalf("startCalls=%d batchCalls=%d, want 1/1", startCalls, batchCalls)
	}
	if len(created) != 2 {
		t.Fatalf("created = %d, want 2", len(created))
	}
	if len(gotBatch.Comments) != 2 {
		t.Fatalf("batch comments = %#v, want 2", gotBatch.Comments)
	}
}

func TestCreateTrailReviewFindingsHydratesLineSelectedText(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	testutil.WriteFile(t, tmp, "src/app.go", "package main\nfunc main() {}\n")

	var gotBatch api.TrailReviewCommentBatchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == trailReviewTestStartPath:
			encodeTrailReviewTestJSON(t, w, api.TrailReviewStartResponse{ReviewID: "rvw_1", TrailID: "trl_1", Limits: api.TrailReviewLimits{MaxCommentsPerBatch: 10}})
		case r.Method == http.MethodPost && r.URL.Path == trailReviewTestCommentsPath:
			if err := json.NewDecoder(r.Body).Decode(&gotBatch); err != nil {
				t.Fatalf("decode batch body: %v", err)
			}
			encodeTrailReviewTestJSON(t, w, api.TrailReviewCommentBatchResponse{Results: []api.TrailReviewCommentBatchResult{
				{ClientID: "c1", Status: "created", Comment: &api.TrailReviewComment{ID: "cm_1", TrailID: "trl_1", ReviewID: "rvw_1", Status: trailReviewStatusOpen}},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	filePath := "src/app.go"
	line := 2
	_, err := createTrailReviewFindings(context.Background(), client, "trl_1", []api.TrailReviewCommentInput{{
		ClientID: "c1",
		Body:     trailReviewStrPtr("body"),
		Location: api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityLine, FilePath: &filePath, StartLine: &line},
	}})
	if err != nil {
		t.Fatalf("createTrailReviewFindings: %v", err)
	}
	if len(gotBatch.Comments) != 1 {
		t.Fatalf("posted comments = %d, want 1", len(gotBatch.Comments))
	}
	loc := gotBatch.Comments[0].Location
	if loc.Granularity != reviewTrailGranularityLine || loc.SelectedText == nil || *loc.SelectedText != "func main() {}" {
		t.Fatalf("posted location = %+v, want line selected_text", loc)
	}
}

func TestPrepareTrailReviewCommentInputsForCreateDowngradesUnselectableLine(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	testutil.WriteFile(t, tmp, "src/app.go", "package main\n\n")

	filePath := "src/app.go"
	line := 2
	got := prepareTrailReviewCommentInputsForCreate(tmp, []api.TrailReviewCommentInput{{
		ClientID: "c1",
		Body:     trailReviewStrPtr("body"),
		Location: api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityLine, FilePath: &filePath, StartLine: &line},
	}})
	if len(got) != 1 {
		t.Fatalf("inputs = %d, want 1", len(got))
	}
	loc := got[0].Location
	if loc.Granularity != reviewTrailGranularityFile || loc.FilePath == nil || *loc.FilePath != filePath || loc.SelectedText != nil {
		t.Fatalf("location = %+v, want file fallback without selected_text", loc)
	}

	missing := "src/missing.go"
	got = prepareTrailReviewCommentInputsForCreate(tmp, []api.TrailReviewCommentInput{{
		ClientID: "c2",
		Body:     trailReviewStrPtr("body"),
		Location: api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityLine, FilePath: &missing, StartLine: &line},
	}})
	if loc := got[0].Location; loc.Granularity != reviewTrailGranularityWholeChange {
		t.Fatalf("missing-file location = %+v, want whole_change fallback", loc)
	}
}

func TestCreateTrailReviewFindingSurfacesBatchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case trailReviewTestStartPath:
			encodeTrailReviewTestJSON(t, w, api.TrailReviewStartResponse{ReviewID: "rvw_1", TrailID: "trl_1"})
		case trailReviewTestCommentsPath:
			encodeTrailReviewTestJSON(t, w, api.TrailReviewCommentBatchResponse{Results: []api.TrailReviewCommentBatchResult{{
				ClientID: "c1",
				Status:   "error",
				Error:    &api.TrailReviewCommentBatchError{Code: "invalid_location", Message: "bad location"},
			}}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	_, err := createTrailReviewFinding(context.Background(), client, "trl_1", api.TrailReviewCommentInput{
		ClientID: "c1",
		Body:     trailReviewStrPtr("body"),
		Location: api.TrailReviewLocationCreateRequest{Granularity: "whole_change"},
	})
	if err == nil {
		t.Fatal("expected an error when the batch result reports status=error")
	}
	if !strings.Contains(err.Error(), "invalid_location") || !strings.Contains(err.Error(), "bad location") {
		t.Fatalf("error = %v, want code+message surfaced", err)
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
			Body:     trailReviewStrPtr("Missing expiry skew handling"),
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
			Body:     trailReviewStrPtr("Retry loop can spin forever"),
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
		Status: "open",
		Branch: "feat/token-refresh",
		Base:   "main",
	}}, comments, false, defaultTrailReviewListOptions(), countTrailReviewComments(comments))
	text := out.String()
	for _, want := range []string{
		"Trail #42  Add token refresh",
		"Open findings: 1  high 1  medium 0  low 0",
		"Resolved: 1",
		"FRESHNESS",
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
		Status: "open",
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
