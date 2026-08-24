package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/spf13/cobra"
)

const (
	changeReviewApplyOriginalContent = "hello\nold\n"
	changeReviewTestCommentID        = "cmt_1"
	changeReviewTestFilePath         = "src/auth/session.ts"
	changeReviewTestStartPath        = "/api/v1/changes/trl_1/reviews"
	changeReviewTestCommentsPath     = "/api/v1/changes/trl_1/reviews/rvw_1/comments"
)

func TestChangeCommandSurfaceUsesFindings(t *testing.T) {
	t.Parallel()
	changeCmd := newChangeCmd()
	children := map[string]*cobra.Command{}
	for _, child := range changeCmd.Commands() {
		children[child.Name()] = child
	}
	findingCmd := children["finding"]
	if findingCmd == nil {
		t.Fatal("change command did not register finding subcommand")
	}
	if children["review"] != nil {
		t.Fatal("change command should not register review subcommand")
	}
	if children["watch"] == nil {
		t.Fatal("change command should register watch subcommand")
	}

	subcommands := map[string]bool{}
	for _, child := range findingCmd.Commands() {
		subcommands[child.Name()] = true
	}
	for _, required := range []string{"list", "add", "show", "update", "apply", "resolve", "dismiss", "reopen"} {
		if !subcommands[required] {
			t.Fatalf("change finding missing %q subcommand", required)
		}
	}
	for _, removed := range []string{"start", "comments", "approve", "request-changes", "watch"} {
		if subcommands[removed] {
			t.Fatalf("change finding should not register removed %q subcommand", removed)
		}
	}
}

func TestChangeCommandRejectsRemovedReviewCommand(t *testing.T) {
	t.Parallel()
	cmd := newChangeCmd()
	cmd.SetArgs([]string{"review"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected removed change review command to error")
	}
}

// Not parallel: uses t.Chdir() to point remote resolution at a fake repo.
func TestResolveChangeReviewTargetRejectsUnsupportedForge(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	cmd := exec.CommandContext(context.Background(), "git", "remote", "add", "origin", "git@gitlab.com:acme/my-app.git")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if err := cmd.Run(); err != nil {
		t.Fatalf("git remote add: %v", err)
	}
	t.Chdir(repoDir)

	_, err := resolveChangeReviewTarget(context.Background(), api.NewClient("tok"), "", "", "")
	if err == nil {
		t.Fatal("expected error for gitlab.com origin, got nil")
	}
	if !strings.Contains(err.Error(), "not on a forge supported by Entire changes") {
		t.Fatalf("error message does not mention unsupported forge: %v", err)
	}
}

func TestChangeReviewCommentsPathUsesReviewQueryContract(t *testing.T) {
	t.Parallel()
	got := changeReviewCommentsPath("change id/with slash", changeReviewListOptions{
		Status:           "open,resolved",
		Severity:         "high,medium",
		Freshness:        "any",
		IncludeDismissed: true,
		Limit:            25,
		Offset:           50,
	})
	want := "/api/v1/changes/change%20id%2Fwith%20slash/reviews/comments?include_dismissed=true&limit=25&offset=50&severity=high%2Cmedium&stale=any&status=open%2Cresolved"
	if got != want {
		t.Fatalf("changeReviewCommentsPath = %q, want %q", got, want)
	}
}

func TestNormalizeChangeReviewListOptionsIncludeDismissedBroadensDefaultStatus(t *testing.T) {
	t.Parallel()
	opts := defaultChangeReviewListOptions()
	opts.IncludeDismissed = true
	got, err := normalizeChangeReviewListOptions(opts)
	if err != nil {
		t.Fatalf("normalizeChangeReviewListOptions: %v", err)
	}
	if got.Status != changeReviewStatusAny {
		t.Fatalf("Status = %q, want %q", got.Status, changeReviewStatusAny)
	}

	opts = defaultChangeReviewListOptions()
	opts.IncludeDismissed = true
	opts.StatusChanged = true
	got, err = normalizeChangeReviewListOptions(opts)
	if err != nil {
		t.Fatalf("normalizeChangeReviewListOptions explicit status: %v", err)
	}
	if got.Status != changeReviewStatusOpen {
		t.Fatalf("explicit Status = %q, want open", got.Status)
	}
}

func TestNormalizeChangeReviewListOptionsRejectsInvalidFilters(t *testing.T) {
	t.Parallel()
	cases := []changeReviewListOptions{
		{Status: "open,nope", Freshness: changeReviewFreshnessAny, Limit: 1},
		{Status: changeReviewStatusAny, Severity: "urgent", Freshness: changeReviewFreshnessAny, Limit: 1},
		{Status: changeReviewStatusAny, Freshness: "old", Limit: 1},
		{Status: changeReviewStatusAny, Freshness: changeReviewFreshnessAny, Limit: 0},
		{Status: changeReviewStatusAny, Freshness: changeReviewFreshnessAny, Limit: 1, Offset: -1},
	}
	for _, opts := range cases {
		if _, err := normalizeChangeReviewListOptions(opts); err == nil {
			t.Fatalf("normalizeChangeReviewListOptions(%+v) succeeded, want error", opts)
		}
	}
}

func TestParseChangeSelectorAndCommentID(t *testing.T) {
	t.Parallel()
	selector, commentID, err := parseChangeSelectorAndCommentID([]string{changeReviewTestCommentID}, "425")
	if err != nil {
		t.Fatalf("parseChangeSelectorAndCommentID with --change: %v", err)
	}
	if selector != "425" || commentID != changeReviewTestCommentID {
		t.Fatalf("selector=%q commentID=%q, want 425/cmt_1", selector, commentID)
	}

	selector, commentID, err = parseChangeSelectorAndCommentID([]string{"feat/review", "cmt_2"}, "")
	if err != nil {
		t.Fatalf("parseChangeSelectorAndCommentID positional: %v", err)
	}
	if selector != "feat/review" || commentID != "cmt_2" {
		t.Fatalf("selector=%q commentID=%q, want feat/review/cmt_2", selector, commentID)
	}

	if _, _, err := parseChangeSelectorAndCommentID([]string{"425", changeReviewTestCommentID}, "trl_1"); err == nil {
		t.Fatal("expected error when both positional change and --change are provided")
	}
}

func TestLoadChangeReviewCommentPatchFile(t *testing.T) {
	t.Parallel()
	opts, err := loadChangeReviewCommentPatchFile(changeReviewCommentAddOptions{PatchFile: "-"}, strings.NewReader("diff --git a/file.txt b/file.txt\n"))
	if err != nil {
		t.Fatalf("loadChangeReviewCommentPatchFile: %v", err)
	}
	if opts.Patch != "diff --git a/file.txt b/file.txt\n" {
		t.Fatalf("Patch = %q", opts.Patch)
	}

	if _, err := loadChangeReviewCommentPatchFile(changeReviewCommentAddOptions{Patch: "inline", PatchFile: "-"}, strings.NewReader("patch")); err == nil {
		t.Fatal("expected error when --patch and --patch-file are both provided")
	}
}

func TestBuildChangeReviewCommentPatchRequest(t *testing.T) {
	t.Parallel()

	req, err := buildChangeReviewCommentPatchRequest(changeReviewUpdateOptions{
		Body:              "Allow a five minute skew.",
		BodyChanged:       true,
		Severity:          "HIGH",
		SeverityChanged:   true,
		Confidence:        0.94,
		ConfidenceChanged: true,
	})
	if err != nil {
		t.Fatalf("buildChangeReviewCommentPatchRequest: %v", err)
	}
	if req.Title != nil {
		t.Fatalf("Title = %#v, want nil", req.Title)
	}
	if req.Body == nil || *req.Body != "Allow a five minute skew." {
		t.Fatalf("Body = %#v", req.Body)
	}
	if req.Severity == nil || *req.Severity != changeReviewSeverityHigh {
		t.Fatalf("Severity = %#v", req.Severity)
	}
	if req.Confidence == nil || *req.Confidence != 0.94 {
		t.Fatalf("Confidence = %#v", req.Confidence)
	}

	if _, err := buildChangeReviewCommentPatchRequest(changeReviewUpdateOptions{}); err == nil {
		t.Fatal("expected an error when no update fields are provided")
	}
	if _, err := buildChangeReviewCommentPatchRequest(changeReviewUpdateOptions{Severity: "urgent", SeverityChanged: true}); err == nil {
		t.Fatal("expected an error for invalid severity")
	}
	if _, err := buildChangeReviewCommentPatchRequest(changeReviewUpdateOptions{Body: " ", BodyChanged: true}); err == nil {
		t.Fatal("expected an error for empty body")
	}
	if _, err := buildChangeReviewCommentPatchRequest(changeReviewUpdateOptions{Severity: " ", SeverityChanged: true}); err == nil {
		t.Fatal("expected an error for empty severity")
	}
}

func TestBuildChangeReviewCommentInput(t *testing.T) {
	t.Parallel()
	input, err := buildChangeReviewCommentInput(changeReviewCommentAddOptions{
		Body:        "Token refresh should allow clock skew.",
		Severity:    "HIGH",
		Confidence:  0.94,
		FilePath:    changeReviewTestFilePath,
		StartLine:   88,
		EndLine:     91,
		ClientID:    "agent-run-1:finding-7",
		Instruction: "Allow a five minute skew.",
	}, nil)
	if err != nil {
		t.Fatalf("buildChangeReviewCommentInput: %v", err)
	}
	if input.Body == nil || *input.Body != "Token refresh should allow clock skew." {
		t.Fatalf("Body = %#v", input.Body)
	}
	if input.Severity == nil || *input.Severity != changeReviewSeverityHigh {
		t.Fatalf("Severity = %#v", input.Severity)
	}
	if input.Confidence == nil || *input.Confidence != 0.94 {
		t.Fatalf("Confidence = %#v", input.Confidence)
	}
	if input.ClientID != "agent-run-1:finding-7" {
		t.Fatalf("ClientID = %q", input.ClientID)
	}
	if input.Location.Granularity != "range" || input.Location.FilePath == nil || *input.Location.FilePath != changeReviewTestFilePath {
		t.Fatalf("Location = %#v", input.Location)
	}
	if input.Location.StartLine == nil || *input.Location.StartLine != 88 || input.Location.EndLine == nil || *input.Location.EndLine != 91 {
		t.Fatalf("Location lines = %#v", input.Location)
	}
	if input.SuggestedChange == nil || input.SuggestedChange.ChangeType != "manual_instruction" {
		t.Fatalf("SuggestedChange = %#v", input.SuggestedChange)
	}
}

func TestBuildChangeReviewCommentInputGeneratesClientID(t *testing.T) {
	t.Parallel()
	input, err := buildChangeReviewCommentInput(changeReviewCommentAddOptions{Body: "finding body"}, nil)
	if err != nil {
		t.Fatalf("buildChangeReviewCommentInput: %v", err)
	}
	if input.ClientID == "" {
		t.Fatal("expected a generated client_id when --client-id is omitted")
	}
}

// TestBuildChangeReviewCommentInputSendsFullPatchAnchor guards the contract the
// API enforces: a unified_diff is rejected unless it carries expected_file_path,
// expected_file_hash, expected_start_line, expected_end_line and expected_lines.
// Sending only change_type and patch is what made --patch fail with a 400.
func TestBuildChangeReviewCommentInputSendsFullPatchAnchor(t *testing.T) {
	t.Parallel()
	anchor := &changeReviewPatchAnchor{
		FilePath:  changeReviewTestFilePath,
		FileHash:  "0cfbf08886fca9a91cb753ec8734c84fcbe52c9f",
		StartLine: 88,
		EndLine:   91,
		Lines:     "old line\n",
	}
	patch := changeReviewPatch(changeReviewTestFilePath, "old")
	input, err := buildChangeReviewCommentInput(changeReviewCommentAddOptions{
		Body:  "Token refresh should allow clock skew.",
		Patch: patch,
	}, anchor)
	if err != nil {
		t.Fatalf("buildChangeReviewCommentInput: %v", err)
	}
	// Compare the whole request: that pins Instruction to nil too, and fails
	// loudly if the API request struct later grows a field nothing populates.
	trimmedPatch := strings.TrimSpace(patch)
	want := api.ChangeReviewSuggestedChangeCreateRequest{
		ChangeType:        "unified_diff",
		Patch:             &trimmedPatch,
		ExpectedFilePath:  &anchor.FilePath,
		ExpectedFileHash:  &anchor.FileHash,
		ExpectedStartLine: &anchor.StartLine,
		ExpectedEndLine:   &anchor.EndLine,
		ExpectedLines:     &anchor.Lines,
	}
	if input.SuggestedChange == nil || !reflect.DeepEqual(*input.SuggestedChange, want) {
		t.Fatalf("SuggestedChange = %#v, want %#v", input.SuggestedChange, want)
	}
}

// TestBuildChangeReviewCommentInputLocatesPatchOnlyFinding covers a --patch with
// no --file: the patch already names the file and the lines it rewrites, so the
// finding is placed there instead of landing on the change as a whole.
func TestBuildChangeReviewCommentInputLocatesPatchOnlyFinding(t *testing.T) {
	t.Parallel()
	anchor := &changeReviewPatchAnchor{FilePath: changeReviewTestFilePath, StartLine: 40, EndLine: 98}
	input, err := buildChangeReviewCommentInput(changeReviewCommentAddOptions{
		Body:  "finding body",
		Patch: changeReviewPatch(changeReviewTestFilePath, "old"),
	}, anchor)
	if err != nil {
		t.Fatalf("buildChangeReviewCommentInput: %v", err)
	}
	loc := input.Location
	if loc.Granularity != reviewTrailGranularityRange {
		t.Fatalf("Granularity = %q, want range", loc.Granularity)
	}
	if loc.FilePath == nil || *loc.FilePath != changeReviewTestFilePath {
		t.Fatalf("FilePath = %#v", loc.FilePath)
	}
	if loc.StartLine == nil || *loc.StartLine != 40 || loc.EndLine == nil || *loc.EndLine != 98 {
		t.Fatalf("lines = %#v/%#v, want 40/98", loc.StartLine, loc.EndLine)
	}
}

// TestBuildChangeReviewCommentInputExplicitFileWinsOverAnchor pins the precedence:
// the anchor only fills a gap, it never overrides what the caller typed.
func TestBuildChangeReviewCommentInputExplicitFileWinsOverAnchor(t *testing.T) {
	t.Parallel()
	// Same file spelled with a leading ./ — must not read as a mismatch. The
	// caller's line stays put even though the patch spans a different range.
	anchor := &changeReviewPatchAnchor{FilePath: changeReviewTestFilePath, StartLine: 40, EndLine: 98}
	input, err := buildChangeReviewCommentInput(changeReviewCommentAddOptions{
		Body:     "finding body",
		FilePath: "./" + changeReviewTestFilePath,
		Line:     45,
		Patch:    changeReviewPatch(changeReviewTestFilePath, "old"),
	}, anchor)
	if err != nil {
		t.Fatalf("buildChangeReviewCommentInput: %v", err)
	}
	if input.Location.Granularity != reviewTrailGranularityLine {
		t.Fatalf("Granularity = %q, want line", input.Location.Granularity)
	}
	if input.Location.StartLine == nil || *input.Location.StartLine != 45 {
		t.Fatalf("StartLine = %#v, want the explicit 45", input.Location.StartLine)
	}
}

// TestBuildChangeReviewCommentInputRejectsFileAnchorMismatch refuses a finding
// whose location and fix point at different files.
func TestBuildChangeReviewCommentInputRejectsFileAnchorMismatch(t *testing.T) {
	t.Parallel()
	anchor := &changeReviewPatchAnchor{FilePath: "cmd/b.go", StartLine: 1, EndLine: 2}
	_, err := buildChangeReviewCommentInput(changeReviewCommentAddOptions{
		Body:     "finding body",
		FilePath: "cmd/a.go",
		Patch:    changeReviewPatch("cmd/b.go", "old"),
	}, anchor)
	if err == nil {
		t.Fatal("expected an error when --file and the patch name different files")
	}
	if !strings.Contains(err.Error(), "cmd/a.go") || !strings.Contains(err.Error(), "cmd/b.go") {
		t.Fatalf("error = %v, want it to name both paths", err)
	}
}

// TestBuildChangeReviewCommentInputRejectsUnanchoredPatch keeps an unanchored
// patch off the wire rather than letting the API reject it.
func TestBuildChangeReviewCommentInputRejectsUnanchoredPatch(t *testing.T) {
	t.Parallel()
	_, err := buildChangeReviewCommentInput(changeReviewCommentAddOptions{
		Body:  "finding body",
		Patch: changeReviewPatch("file.txt", "old"),
	}, nil)
	if err == nil {
		t.Fatal("expected an error when a patch has no resolved anchor")
	}
}

func TestResolveChangeReviewPatchAnchor(t *testing.T) {
	repo := newChangeReviewApplyRepo(t)
	writeChangeReviewApplyFile(t, repo, "dir/file.txt")

	anchor, err := resolveChangeReviewPatchAnchor(context.Background(), changeReviewPatch("dir/file.txt", "old"))
	if err != nil {
		t.Fatalf("resolveChangeReviewPatchAnchor: %v", err)
	}
	if anchor.FilePath != "dir/file.txt" {
		t.Fatalf("FilePath = %q", anchor.FilePath)
	}
	if anchor.StartLine != 1 || anchor.EndLine != 2 {
		t.Fatalf("range = %d-%d, want 1-2", anchor.StartLine, anchor.EndLine)
	}
	// expected_lines is the exact pre-image slice, line endings included.
	if anchor.Lines != changeReviewApplyOriginalContent {
		t.Fatalf("Lines = %q, want %q", anchor.Lines, changeReviewApplyOriginalContent)
	}
	// The hash must be the blob OID git itself would print for the file.
	want := changeReviewGitOutput(t, repo, "hash-object", "dir/file.txt")
	if anchor.FileHash != want {
		t.Fatalf("FileHash = %q, want %q", anchor.FileHash, want)
	}
}

func TestResolveChangeReviewPatchAnchorErrors(t *testing.T) {
	repo := newChangeReviewApplyRepo(t)
	writeChangeReviewApplyFile(t, repo, "file.txt")

	tests := []struct {
		name  string
		patch string
		want  string
	}{
		{
			name:  "missing file",
			patch: changeReviewPatch("absent.txt", "old"),
			want:  "anchor the suggested change",
		},
		{
			name:  "hunk past end of file",
			patch: "--- a/file.txt\n+++ b/file.txt\n@@ -40,2 +40,2 @@\n old\n",
			want:  "the file has 2",
		},
		{
			name:  "multiple files",
			patch: changeReviewPatch("file.txt", "old") + changeReviewPatch("other.txt", "old"),
			want:  "multiple files",
		},
		{
			name:  "file creation",
			patch: "--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1,1 @@\n+added\n",
			want:  "must modify an existing file",
		},
		{
			name:  "no hunk header",
			patch: "--- a/file.txt\n+++ b/file.txt\n",
			want:  "no '@@' hunk header",
		},
		{
			name:  "escapes the repository",
			patch: "--- a/../outside.txt\n+++ b/../outside.txt\n@@ -1,1 +1,1 @@\n-a\n+b\n",
			want:  "escapes the repository",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveChangeReviewPatchAnchor(context.Background(), tc.patch)
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestParseChangeReviewPatchTargetSkipsHunkBodies covers diff *content* that
// looks like a diff *header*. Deleting a line that itself starts with "-- "
// (SQL, Lua and Haskell comments, CLI flag docs) serializes as "--- ...", and
// an added line starting with "++ " serializes as "+++ ...". Neither is a file
// header, and treating them as one rejected valid single-file patches.
func TestParseChangeReviewPatchTargetSkipsHunkBodies(t *testing.T) {
	t.Parallel()
	patch := "--- a/schema.sql\n+++ b/schema.sql\n" +
		"@@ -10,4 +10,4 @@\n" +
		" CREATE TABLE t (\n" +
		"--- legacy column, drop after migration\n" +
		"+++ replacement column\n" +
		" );\n"
	target, err := parseChangeReviewPatchTarget(patch)
	if err != nil {
		t.Fatalf("parseChangeReviewPatchTarget: %v", err)
	}
	if target.Path != "schema.sql" {
		t.Fatalf("Path = %q, want schema.sql", target.Path)
	}
	if target.StartLine != 10 || target.EndLine != 13 {
		t.Fatalf("range = %d-%d, want 10-13", target.StartLine, target.EndLine)
	}
}

// TestParseChangeReviewPatchTargetKeepsRealPathPrefix guards a file genuinely
// living under b/ (or a/): the diff header "--- a/b/pkg.go" must resolve to
// b/pkg.go, not pkg.go. Stripping both prefixes in sequence read the wrong file.
func TestParseChangeReviewPatchTargetKeepsRealPathPrefix(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"a", "b"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			want := dir + "/pkg.go"
			patch := "--- a/" + want + "\n+++ b/" + want + "\n@@ -1,2 +1,2 @@\n-old\n+new\n"
			target, err := parseChangeReviewPatchTarget(patch)
			if err != nil {
				t.Fatalf("parseChangeReviewPatchTarget: %v", err)
			}
			if target.Path != want {
				t.Fatalf("Path = %q, want %q", target.Path, want)
			}
		})
	}
}

// TestParseChangeReviewPatchTargetPrependToExistingFile covers "@@ -0,0 +1,N @@",
// which `diff -u0` emits both for a brand-new file and for an insertion above
// line 1 of an existing one. Only a /dev/null old path means creation; the
// prepend anchors on the line it is inserted before.
func TestParseChangeReviewPatchTargetPrependToExistingFile(t *testing.T) {
	t.Parallel()
	patch := "--- a/file.txt\n+++ b/file.txt\n@@ -0,0 +1,1 @@\n+added\n"
	target, err := parseChangeReviewPatchTarget(patch)
	if err != nil {
		t.Fatalf("parseChangeReviewPatchTarget: %v", err)
	}
	if target.StartLine != 1 || target.EndLine != 1 {
		t.Fatalf("range = %d-%d, want 1-1", target.StartLine, target.EndLine)
	}
}

// TestParseChangeReviewPatchTargetAllowsDeletion keeps "delete this" a valid
// suggestion: a /dev/null *new* side still has a real old file to anchor to,
// unlike a /dev/null old side.
func TestParseChangeReviewPatchTargetAllowsDeletion(t *testing.T) {
	t.Parallel()
	patch := "--- a/file.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-hello\n-old\n"
	target, err := parseChangeReviewPatchTarget(patch)
	if err != nil {
		t.Fatalf("parseChangeReviewPatchTarget: %v", err)
	}
	if target.Path != "file.txt" || target.StartLine != 1 || target.EndLine != 2 {
		t.Fatalf("target = %+v, want file.txt 1-2", target)
	}
}

// TestParseChangeReviewPatchTargetRejectsRename gives renames an actionable
// message: the old path is gone from the worktree, so anchoring would otherwise
// fail with a confusing "read <old path>" error.
func TestParseChangeReviewPatchTargetRejectsRename(t *testing.T) {
	t.Parallel()
	patch := "diff --git a/old.go b/new.go\nrename from old.go\nrename to new.go\n" +
		"--- a/old.go\n+++ b/new.go\n@@ -1,2 +1,2 @@\n-old\n+new\n"
	_, err := parseChangeReviewPatchTarget(patch)
	if err == nil {
		t.Fatal("expected a rename patch to be rejected")
	}
	if !strings.Contains(err.Error(), "renames") {
		t.Fatalf("error = %v, want it to mention the rename", err)
	}
}

// TestParseChangeReviewPatchTargetSpansHunks checks that a multi-hunk patch
// anchors to the full span it touches, not just its first hunk.
func TestParseChangeReviewPatchTargetSpansHunks(t *testing.T) {
	t.Parallel()
	// Hunk bodies must match the counts their headers declare, exactly as git
	// emits them — the parser consumes bodies by those counts.
	patch := "--- a/file.txt\n+++ b/file.txt\n" +
		"@@ -20,3 +20,3 @@\n a\n-b\n+c\n g\n" +
		"@@ -5,2 +5,2 @@\n d\n-e\n+f\n"
	target, err := parseChangeReviewPatchTarget(patch)
	if err != nil {
		t.Fatalf("parseChangeReviewPatchTarget: %v", err)
	}
	if target.StartLine != 5 || target.EndLine != 22 {
		t.Fatalf("range = %d-%d, want 5-22", target.StartLine, target.EndLine)
	}
}

func TestCreateChangeReviewFindingStartsReviewThenPostsBatch(t *testing.T) {
	var (
		gotBatch    api.ChangeReviewCommentBatchRequest
		startCalled bool
		batchCalled bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == changeReviewTestStartPath:
			startCalled = true
			encodeChangeReviewTestJSON(t, w, api.ChangeReviewStartResponse{ReviewID: "rvw_1", ChangeID: "trl_1"})
		case r.Method == http.MethodPost && r.URL.Path == changeReviewTestCommentsPath:
			batchCalled = true
			if err := json.NewDecoder(r.Body).Decode(&gotBatch); err != nil {
				t.Fatalf("decode batch body: %v", err)
			}
			encodeChangeReviewTestJSON(t, w, api.ChangeReviewCommentBatchResponse{Results: []api.ChangeReviewCommentBatchResult{{
				ClientID: "agent-run-1:finding-1",
				Status:   "created",
				Comment:  &api.ChangeReviewComment{ID: changeReviewTestCommentID, ChangeID: "trl_1", ReviewID: "rvw_1", Status: changeReviewStatusOpen},
			}}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	created, err := createChangeReviewFinding(context.Background(), client, "trl_1", api.ChangeReviewCommentInput{
		ClientID: "agent-run-1:finding-1",
		Body:     changeReviewStrPtr("body"),
		Location: api.ChangeReviewLocationCreateRequest{Granularity: "whole_change"},
	})
	if err != nil {
		t.Fatalf("createChangeReviewFinding: %v", err)
	}
	if !startCalled || !batchCalled {
		t.Fatalf("startCalled=%v batchCalled=%v (expected both)", startCalled, batchCalled)
	}
	if created.ID != changeReviewTestCommentID {
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

func TestCreateChangeReviewFindingsPostsOneBatch(t *testing.T) {
	var (
		gotBatch   api.ChangeReviewCommentBatchRequest
		startCalls int
		batchCalls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == changeReviewTestStartPath:
			startCalls++
			encodeChangeReviewTestJSON(t, w, api.ChangeReviewStartResponse{ReviewID: "rvw_1", ChangeID: "trl_1", Limits: api.ChangeReviewLimits{MaxCommentsPerBatch: 10}})
		case r.Method == http.MethodPost && r.URL.Path == changeReviewTestCommentsPath:
			batchCalls++
			if err := json.NewDecoder(r.Body).Decode(&gotBatch); err != nil {
				t.Fatalf("decode batch body: %v", err)
			}
			encodeChangeReviewTestJSON(t, w, api.ChangeReviewCommentBatchResponse{Results: []api.ChangeReviewCommentBatchResult{
				{ClientID: "c1", Status: "created", Comment: &api.ChangeReviewComment{ID: "cm_1", ChangeID: "trl_1", ReviewID: "rvw_1", Status: changeReviewStatusOpen}},
				{ClientID: "c2", Status: "created", Comment: &api.ChangeReviewComment{ID: "cm_2", ChangeID: "trl_1", ReviewID: "rvw_1", Status: changeReviewStatusOpen}},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	created, err := createChangeReviewFindings(context.Background(), client, "trl_1", []api.ChangeReviewCommentInput{
		{ClientID: "c1", Body: changeReviewStrPtr("first"), Location: api.ChangeReviewLocationCreateRequest{Granularity: "whole_change"}},
		{ClientID: "c2", Body: changeReviewStrPtr("second"), Location: api.ChangeReviewLocationCreateRequest{Granularity: "whole_change"}},
	})
	if err != nil {
		t.Fatalf("createChangeReviewFindings: %v", err)
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

func TestCreateChangeReviewFindingsHydratesLineSelectedText(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	testutil.WriteFile(t, tmp, "src/app.go", "package main\nfunc main() {}\n")

	var gotBatch api.ChangeReviewCommentBatchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == changeReviewTestStartPath:
			encodeChangeReviewTestJSON(t, w, api.ChangeReviewStartResponse{ReviewID: "rvw_1", ChangeID: "trl_1", Limits: api.ChangeReviewLimits{MaxCommentsPerBatch: 10}})
		case r.Method == http.MethodPost && r.URL.Path == changeReviewTestCommentsPath:
			if err := json.NewDecoder(r.Body).Decode(&gotBatch); err != nil {
				t.Fatalf("decode batch body: %v", err)
			}
			encodeChangeReviewTestJSON(t, w, api.ChangeReviewCommentBatchResponse{Results: []api.ChangeReviewCommentBatchResult{
				{ClientID: "c1", Status: "created", Comment: &api.ChangeReviewComment{ID: "cm_1", ChangeID: "trl_1", ReviewID: "rvw_1", Status: changeReviewStatusOpen}},
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
	_, err := createChangeReviewFindings(context.Background(), client, "trl_1", []api.ChangeReviewCommentInput{{
		ClientID: "c1",
		Body:     changeReviewStrPtr("body"),
		Location: api.ChangeReviewLocationCreateRequest{Granularity: reviewTrailGranularityLine, FilePath: &filePath, StartLine: &line},
	}})
	if err != nil {
		t.Fatalf("createChangeReviewFindings: %v", err)
	}
	if len(gotBatch.Comments) != 1 {
		t.Fatalf("posted comments = %d, want 1", len(gotBatch.Comments))
	}
	loc := gotBatch.Comments[0].Location
	if loc.Granularity != reviewTrailGranularityLine || loc.SelectedText == nil || *loc.SelectedText != "func main() {}" {
		t.Fatalf("posted location = %+v, want line selected_text", loc)
	}
}

func TestPrepareChangeReviewCommentInputsForCreateDowngradesUnselectableLine(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	testutil.WriteFile(t, tmp, "src/app.go", "package main\n\n")

	filePath := "src/app.go"
	line := 2
	got := prepareChangeReviewCommentInputsForCreate(tmp, []api.ChangeReviewCommentInput{{
		ClientID: "c1",
		Body:     changeReviewStrPtr("body"),
		Location: api.ChangeReviewLocationCreateRequest{Granularity: reviewTrailGranularityLine, FilePath: &filePath, StartLine: &line},
	}})
	if len(got) != 1 {
		t.Fatalf("inputs = %d, want 1", len(got))
	}
	loc := got[0].Location
	if loc.Granularity != reviewTrailGranularityFile || loc.FilePath == nil || *loc.FilePath != filePath || loc.SelectedText != nil {
		t.Fatalf("location = %+v, want file fallback without selected_text", loc)
	}

	missing := "src/missing.go"
	got = prepareChangeReviewCommentInputsForCreate(tmp, []api.ChangeReviewCommentInput{{
		ClientID: "c2",
		Body:     changeReviewStrPtr("body"),
		Location: api.ChangeReviewLocationCreateRequest{Granularity: reviewTrailGranularityLine, FilePath: &missing, StartLine: &line},
	}})
	if loc := got[0].Location; loc.Granularity != reviewTrailGranularityWholeChange {
		t.Fatalf("missing-file location = %+v, want whole_change fallback", loc)
	}
}

func TestCreateChangeReviewFindingSurfacesBatchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case changeReviewTestStartPath:
			encodeChangeReviewTestJSON(t, w, api.ChangeReviewStartResponse{ReviewID: "rvw_1", ChangeID: "trl_1"})
		case changeReviewTestCommentsPath:
			encodeChangeReviewTestJSON(t, w, api.ChangeReviewCommentBatchResponse{Results: []api.ChangeReviewCommentBatchResult{{
				ClientID: "c1",
				Status:   "error",
				Error:    &api.ChangeReviewCommentBatchError{Code: "invalid_location", Message: "bad location"},
			}}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	_, err := createChangeReviewFinding(context.Background(), client, "trl_1", api.ChangeReviewCommentInput{
		ClientID: "c1",
		Body:     changeReviewStrPtr("body"),
		Location: api.ChangeReviewLocationCreateRequest{Granularity: "whole_change"},
	})
	if err == nil {
		t.Fatal("expected an error when the batch result reports status=error")
	}
	if !strings.Contains(err.Error(), "invalid_location") || !strings.Contains(err.Error(), "bad location") {
		t.Fatalf("error = %v, want code+message surfaced", err)
	}
}

func TestPrintChangeReviewDashboard(t *testing.T) {
	t.Parallel()
	high := changeReviewSeverityHigh
	medium := changeReviewSeverityMedium
	path := changeReviewTestFilePath
	line := 88
	comments := []api.ChangeReviewComment{
		{
			ID:       "comment-high-123",
			ReviewID: "review-1",
			Body:     changeReviewStrPtr("Missing expiry skew handling"),
			Severity: &high,
			Status:   changeReviewStatusOpen,
			Location: api.ChangeReviewLocation{
				Granularity: "line",
				FilePath:    &path,
				StartLine:   &line,
			},
		},
		{
			ID:       "comment-medium-123",
			ReviewID: "review-1",
			Body:     changeReviewStrPtr("Retry loop can spin forever"),
			Severity: &medium,
			Status:   changeReviewStatusResolved,
			Location: api.ChangeReviewLocation{Granularity: "whole_change"},
		},
	}
	var out strings.Builder
	printChangeReviewDashboard(&out, changeReviewTarget{Change: api.ChangeResource{
		ID:     "trl_1",
		Number: 42,
		Title:  "Add token refresh",
		Status: "open",
		Branch: "feat/token-refresh",
		Base:   "main",
	}}, comments, false, defaultChangeReviewListOptions(), countChangeReviewComments(comments))
	text := out.String()
	for _, want := range []string{
		"Change #42  Add token refresh",
		"Open findings: 1  high 1  medium 0  low 0",
		"Resolved: 1",
		"FRESHNESS",
		"High",
		changeReviewTestFilePath + ":88",
		"Missing expiry skew handling",
		"Actions:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, text)
		}
	}
}

func TestPrintChangeReviewDashboard_UsesSeparateCountsWhenFilteredCommentsEmpty(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	counts := countChangeReviewComments([]api.ChangeReviewComment{
		{ID: "resolved-1", Status: changeReviewStatusResolved},
		{ID: "dismissed-1", Status: changeReviewStatusDismissed, StaleOutcome: "stale"},
	})
	printChangeReviewDashboard(&out, changeReviewTarget{Change: api.ChangeResource{
		ID:     "trl_1",
		Number: 42,
		Title:  "Add token refresh",
		Status: "open",
		Branch: "feat/token-refresh",
		Base:   "main",
	}}, nil, false, defaultChangeReviewListOptions(), counts)
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

func TestFetchChangeReviewCommentsAndPatchStatus(t *testing.T) {
	var gotPatchBody api.ChangeReviewCommentPatchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/changes/trl_1/reviews/comments":
			if got := r.URL.Query().Get("status"); got != "open" {
				t.Fatalf("status query = %q, want open", got)
			}
			encodeChangeReviewTestJSON(t, w, api.ChangeReviewCommentsResponse{Comments: []api.ChangeReviewComment{
				{ID: changeReviewTestCommentID, ChangeID: "trl_1", ReviewID: "rvw_1", Status: changeReviewStatusOpen, Location: api.ChangeReviewLocation{Granularity: "whole_change"}},
			}})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/changes/trl_1/reviews/rvw_1/comments/cmt_1":
			if err := json.NewDecoder(r.Body).Decode(&gotPatchBody); err != nil {
				t.Fatalf("decode patch body: %v", err)
			}
			encodeChangeReviewTestJSON(t, w, api.ChangeReviewComment{ID: changeReviewTestCommentID, ChangeID: "trl_1", ReviewID: "rvw_1", Status: changeReviewStatusResolved})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	comments, hasMore, err := fetchChangeReviewComments(context.Background(), client, "trl_1", defaultChangeReviewListOptions())
	if err != nil {
		t.Fatalf("fetchChangeReviewComments: %v", err)
	}
	if hasMore || len(comments) != 1 || comments[0].ID != changeReviewTestCommentID {
		t.Fatalf("comments = %#v, hasMore=%v", comments, hasMore)
	}
	updated, err := patchChangeReviewCommentStatus(context.Background(), client, "trl_1", comments[0], changeReviewStatusResolved, "fixed")
	if err != nil {
		t.Fatalf("patchChangeReviewCommentStatus: %v", err)
	}
	if updated.Status != changeReviewStatusResolved {
		t.Fatalf("updated status = %q", updated.Status)
	}
	if gotPatchBody.Status != changeReviewStatusResolved || gotPatchBody.StatusReason == nil || *gotPatchBody.StatusReason != "fixed" {
		t.Fatalf("patch body = %#v", gotPatchBody)
	}
}

func TestFetchAllChangeReviewCommentsStopsOnRepeatedPage(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Query().Get("include_dismissed"); got != strconv.FormatBool(true) {
			t.Errorf("include_dismissed = %q, want true", got)
		}
		wantOffset := ""
		if requests == 2 {
			wantOffset = strconv.Itoa(defaultChangeReviewLimit)
		}
		if got := r.URL.Query().Get("offset"); got != wantOffset {
			t.Errorf("offset = %q, want %q", got, wantOffset)
		}
		encodeChangeReviewTestJSON(t, w, api.ChangeReviewCommentsResponse{
			Comments: []api.ChangeReviewComment{{ID: changeReviewTestCommentID}},
			HasMore:  true,
		})
	}))
	defer srv.Close()
	client := api.NewClientWithBaseURL("tok", srv.URL)

	_, err := fetchAllChangeReviewComments(context.Background(), client, "trl_1", changeReviewSummaryOptions())
	if err == nil || !strings.Contains(err.Error(), "repeated page") {
		t.Fatalf("error = %v, want repeated page", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestFetchChangeReviewStateFollowsCursor(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/changes/trl_1/reviews/rvw_1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if got := r.URL.Query().Get("include_dismissed"); got != strconv.FormatBool(true) {
			t.Fatalf("include_dismissed = %q, want true", got)
		}
		if got := r.URL.Query().Get("limit"); got != strconv.Itoa(defaultChangeReviewLimit) {
			t.Fatalf("limit = %q, want %d", got, defaultChangeReviewLimit)
		}
		if got := r.URL.Query().Get("stale"); got != changeReviewFreshnessAny {
			t.Fatalf("stale = %q, want %q", got, changeReviewFreshnessAny)
		}
		requests++
		switch r.URL.Query().Get("cursor") {
		case "":
			next := "cursor-2"
			encodeChangeReviewTestJSON(t, w, api.ChangeReviewStateResponse{
				Review:      api.ChangeReview{ID: "rvw_1"},
				CodeVersion: api.ChangeReviewCodeVersion{ID: "cv_1"},
				Comments:    []api.ChangeReviewComment{{ID: changeReviewTestCommentID}},
				NextCursor:  &next,
			})
		case "cursor-2":
			encodeChangeReviewTestJSON(t, w, api.ChangeReviewStateResponse{
				Review:      api.ChangeReview{ID: "rvw_1"},
				CodeVersion: api.ChangeReviewCodeVersion{ID: "cv_1"},
				Comments:    []api.ChangeReviewComment{{ID: "cmt_2"}},
			})
		default:
			t.Fatalf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	state, err := fetchChangeReviewState(context.Background(), client, "trl_1", "rvw_1")
	if err != nil {
		t.Fatalf("fetchChangeReviewState: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if len(state.Comments) != 2 || state.Comments[0].ID != changeReviewTestCommentID || state.Comments[1].ID != "cmt_2" {
		t.Fatalf("comments = %#v", state.Comments)
	}
	if state.NextCursor != nil {
		t.Fatalf("NextCursor = %#v, want nil after final page", state.NextCursor)
	}
}

func TestApplyChangeReviewSuggestions_AppliesUnifiedDiff(t *testing.T) {
	repo := newChangeReviewApplyRepo(t)
	writeChangeReviewApplyFile(t, repo, "file.txt")
	comment := changeReviewApplyComment(changeReviewPatch("file.txt", "old"))

	applied, err := applyChangeReviewSuggestions(context.Background(), comment, false, io.Discard)
	if err != nil {
		t.Fatalf("applyChangeReviewSuggestions: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied = %d, want 1", applied)
	}
	if got := readChangeReviewApplyFile(t, repo, "file.txt"); got != "hello\nnew\n" {
		t.Fatalf("file content = %q", got)
	}
}

func TestApplyChangeReviewSuggestions_CheckDoesNotModifyWorktree(t *testing.T) {
	repo := newChangeReviewApplyRepo(t)
	writeChangeReviewApplyFile(t, repo, "file.txt")
	comment := changeReviewApplyComment(changeReviewPatch("file.txt", "old"))

	applied, err := applyChangeReviewSuggestions(context.Background(), comment, true, io.Discard)
	if err != nil {
		t.Fatalf("applyChangeReviewSuggestions --check: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied = %d, want 1", applied)
	}
	if got := readChangeReviewApplyFile(t, repo, "file.txt"); got != changeReviewApplyOriginalContent {
		t.Fatalf("file content = %q", got)
	}
}

func TestApplyChangeReviewSuggestions_FailureDoesNotPartiallyApply(t *testing.T) {
	repo := newChangeReviewApplyRepo(t)
	writeChangeReviewApplyFile(t, repo, "a.txt")
	writeChangeReviewApplyFile(t, repo, "b.txt")
	comment := changeReviewApplyComment(
		changeReviewPatch("a.txt", "old"),
		changeReviewPatch("b.txt", "missing"),
	)

	applied, err := applyChangeReviewSuggestions(context.Background(), comment, false, io.Discard)
	if err == nil {
		t.Fatal("applyChangeReviewSuggestions expected error")
	}
	if applied != 0 {
		t.Fatalf("applied = %d, want 0", applied)
	}
	if got := readChangeReviewApplyFile(t, repo, "a.txt"); got != changeReviewApplyOriginalContent {
		t.Fatalf("a.txt content = %q", got)
	}
	if got := readChangeReviewApplyFile(t, repo, "b.txt"); got != changeReviewApplyOriginalContent {
		t.Fatalf("b.txt content = %q", got)
	}
}

func TestApplyChangeReviewSuggestions_RejectsGitMetadataPaths(t *testing.T) {
	_ = newChangeReviewApplyRepo(t)
	comment := changeReviewApplyComment(`diff --git a/.git/config b/.git/config
--- a/.git/config
+++ b/.git/config
@@ -1,1 +1,1 @@
-old
+new
`)

	_, err := applyChangeReviewSuggestions(context.Background(), comment, false, io.Discard)
	if err == nil {
		t.Fatal("applyChangeReviewSuggestions expected unsafe path error")
	}
	if !strings.Contains(err.Error(), ".git") {
		t.Fatalf("error = %v, want .git mention", err)
	}
}

func newChangeReviewApplyRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Bare `git init` rather than testutil.InitRepo: these tests apply patches to
	// the working tree without committing, so no user/GPG config is needed, and we
	// must avoid testutil's core.autocrlf=true which rewrites patched LF to CRLF.
	runChangeReviewApplyGit(t, dir, "init")
	paths.ClearWorktreeRootCache()
	t.Chdir(dir)
	t.Cleanup(paths.ClearWorktreeRootCache)
	return dir
}

func writeChangeReviewApplyFile(t *testing.T, repo, rel string) {
	t.Helper()
	path := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(changeReviewApplyOriginalContent), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func readChangeReviewApplyFile(t *testing.T, repo, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// changeReviewGitOutput runs git in dir and returns its trimmed stdout, failing
// the test on error. runChangeReviewApplyGit is the same thing where the output
// is not interesting.
func changeReviewGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func runChangeReviewApplyGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	changeReviewGitOutput(t, dir, args...)
}

func changeReviewApplyComment(patches ...string) api.ChangeReviewComment {
	changes := make([]api.ChangeReviewSuggestedChange, len(patches))
	for i, patch := range patches {
		changes[i] = api.ChangeReviewSuggestedChange{
			ID:         "change-" + string(rune('a'+i)),
			ChangeType: "unified_diff",
			Patch:      changeReviewStrPtr(patch),
		}
	}
	return api.ChangeReviewComment{ID: changeReviewTestCommentID, SuggestedChanges: changes}
}

func changeReviewPatch(file, oldText string) string {
	return "diff --git a/" + file + " b/" + file + "\n" +
		"--- a/" + file + "\n" +
		"+++ b/" + file + "\n" +
		"@@ -1,2 +1,2 @@\n" +
		" hello\n" +
		"-" + oldText + "\n" +
		"+new\n"
}

func encodeChangeReviewTestJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func changeReviewStrPtr(s string) *string { return &s }
