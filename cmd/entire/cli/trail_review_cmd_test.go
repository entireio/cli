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
	trailReviewApplyOriginalContent = "hello\nold\n"
	trailReviewTestCommentID        = "cmt_1"
	trailReviewTestFilePath         = "src/auth/session.ts"
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
	testutil.RunGit(t, repoDir, "remote", "add", "origin", "git@gitlab.com:acme/my-app.git")
	t.Chdir(repoDir)

	_, err := resolveTrailReviewTarget(context.Background(), api.NewClient("tok"), "", "", "")
	if err == nil {
		t.Fatal("expected error for gitlab.com origin, got nil")
	}
	if !strings.Contains(err.Error(), "not on a forge supported by Entire trails") {
		t.Fatalf("error message does not mention unsupported forge: %v", err)
	}
}

func TestTrailReviewCommentsPathUsesReviewQueryContract(t *testing.T) {
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
		FilePath:    trailReviewTestFilePath,
		StartLine:   88,
		EndLine:     91,
		ClientID:    "agent-run-1:finding-7",
		Instruction: "Allow a five minute skew.",
	}, nil)
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
	if input.Location.Granularity != "range" || input.Location.FilePath == nil || *input.Location.FilePath != trailReviewTestFilePath {
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
	input, err := buildTrailReviewCommentInput(trailReviewCommentAddOptions{Body: "finding body"}, nil)
	if err != nil {
		t.Fatalf("buildTrailReviewCommentInput: %v", err)
	}
	if input.ClientID == "" {
		t.Fatal("expected a generated client_id when --client-id is omitted")
	}
}

// TestBuildTrailReviewCommentInputSendsFullPatchAnchor guards the contract the
// API enforces: a unified_diff is rejected unless it carries expected_file_path,
// expected_file_hash, expected_start_line, expected_end_line and expected_lines.
// Sending only change_type and patch is what made --patch fail with a 400.
func TestBuildTrailReviewCommentInputSendsFullPatchAnchor(t *testing.T) {
	t.Parallel()
	anchor := &trailReviewPatchAnchor{
		FilePath:  trailReviewTestFilePath,
		FileHash:  "0cfbf08886fca9a91cb753ec8734c84fcbe52c9f",
		StartLine: 88,
		EndLine:   91,
		Lines:     "old line\n",
	}
	patch := trailReviewPatch(trailReviewTestFilePath, "old")
	input, err := buildTrailReviewCommentInput(trailReviewCommentAddOptions{
		Body:  "Token refresh should allow clock skew.",
		Patch: patch,
	}, anchor)
	if err != nil {
		t.Fatalf("buildTrailReviewCommentInput: %v", err)
	}
	// Compare the whole request: that pins Instruction to nil too, and fails
	// loudly if the API request struct later grows a field nothing populates.
	trimmedPatch := strings.TrimSpace(patch)
	want := api.TrailReviewSuggestedChangeCreateRequest{
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

// TestBuildTrailReviewCommentInputLocatesPatchOnlyFinding covers a --patch with
// no --file: the patch already names the file and the lines it rewrites, so the
// finding is placed there instead of landing on the trail as a whole.
func TestBuildTrailReviewCommentInputLocatesPatchOnlyFinding(t *testing.T) {
	t.Parallel()
	anchor := &trailReviewPatchAnchor{FilePath: trailReviewTestFilePath, StartLine: 40, EndLine: 98}
	input, err := buildTrailReviewCommentInput(trailReviewCommentAddOptions{
		Body:  "finding body",
		Patch: trailReviewPatch(trailReviewTestFilePath, "old"),
	}, anchor)
	if err != nil {
		t.Fatalf("buildTrailReviewCommentInput: %v", err)
	}
	loc := input.Location
	if loc.Granularity != reviewTrailGranularityRange {
		t.Fatalf("Granularity = %q, want range", loc.Granularity)
	}
	if loc.FilePath == nil || *loc.FilePath != trailReviewTestFilePath {
		t.Fatalf("FilePath = %#v", loc.FilePath)
	}
	if loc.StartLine == nil || *loc.StartLine != 40 || loc.EndLine == nil || *loc.EndLine != 98 {
		t.Fatalf("lines = %#v/%#v, want 40/98", loc.StartLine, loc.EndLine)
	}
}

// TestBuildTrailReviewCommentInputExplicitFileWinsOverAnchor pins the precedence:
// the anchor only fills a gap, it never overrides what the caller typed.
func TestBuildTrailReviewCommentInputExplicitFileWinsOverAnchor(t *testing.T) {
	t.Parallel()
	// Same file spelled with a leading ./ — must not read as a mismatch. The
	// caller's line stays put even though the patch spans a different range.
	anchor := &trailReviewPatchAnchor{FilePath: trailReviewTestFilePath, StartLine: 40, EndLine: 98}
	input, err := buildTrailReviewCommentInput(trailReviewCommentAddOptions{
		Body:     "finding body",
		FilePath: "./" + trailReviewTestFilePath,
		Line:     45,
		Patch:    trailReviewPatch(trailReviewTestFilePath, "old"),
	}, anchor)
	if err != nil {
		t.Fatalf("buildTrailReviewCommentInput: %v", err)
	}
	if input.Location.Granularity != reviewTrailGranularityLine {
		t.Fatalf("Granularity = %q, want line", input.Location.Granularity)
	}
	if input.Location.StartLine == nil || *input.Location.StartLine != 45 {
		t.Fatalf("StartLine = %#v, want the explicit 45", input.Location.StartLine)
	}
}

// TestBuildTrailReviewCommentInputRejectsFileAnchorMismatch refuses a finding
// whose location and fix point at different files.
func TestBuildTrailReviewCommentInputRejectsFileAnchorMismatch(t *testing.T) {
	t.Parallel()
	anchor := &trailReviewPatchAnchor{FilePath: "cmd/b.go", StartLine: 1, EndLine: 2}
	_, err := buildTrailReviewCommentInput(trailReviewCommentAddOptions{
		Body:     "finding body",
		FilePath: "cmd/a.go",
		Patch:    trailReviewPatch("cmd/b.go", "old"),
	}, anchor)
	if err == nil {
		t.Fatal("expected an error when --file and the patch name different files")
	}
	if !strings.Contains(err.Error(), "cmd/a.go") || !strings.Contains(err.Error(), "cmd/b.go") {
		t.Fatalf("error = %v, want it to name both paths", err)
	}
}

// TestBuildTrailReviewCommentInputRejectsUnanchoredPatch keeps an unanchored
// patch off the wire rather than letting the API reject it.
func TestBuildTrailReviewCommentInputRejectsUnanchoredPatch(t *testing.T) {
	t.Parallel()
	_, err := buildTrailReviewCommentInput(trailReviewCommentAddOptions{
		Body:  "finding body",
		Patch: trailReviewPatch("file.txt", "old"),
	}, nil)
	if err == nil {
		t.Fatal("expected an error when a patch has no resolved anchor")
	}
}

func TestResolveTrailReviewPatchAnchor(t *testing.T) {
	repo := newTrailReviewApplyRepo(t)
	writeTrailReviewApplyFile(t, repo, "dir/file.txt")

	anchor, err := resolveTrailReviewPatchAnchor(context.Background(), trailReviewPatch("dir/file.txt", "old"))
	if err != nil {
		t.Fatalf("resolveTrailReviewPatchAnchor: %v", err)
	}
	if anchor.FilePath != "dir/file.txt" {
		t.Fatalf("FilePath = %q", anchor.FilePath)
	}
	if anchor.StartLine != 1 || anchor.EndLine != 2 {
		t.Fatalf("range = %d-%d, want 1-2", anchor.StartLine, anchor.EndLine)
	}
	// expected_lines is the exact pre-image slice, line endings included.
	if anchor.Lines != trailReviewApplyOriginalContent {
		t.Fatalf("Lines = %q, want %q", anchor.Lines, trailReviewApplyOriginalContent)
	}
	// The hash must be the blob OID git itself would print for the file.
	want := trailReviewGitOutput(t, repo, "hash-object", "dir/file.txt")
	if anchor.FileHash != want {
		t.Fatalf("FileHash = %q, want %q", anchor.FileHash, want)
	}
}

func TestResolveTrailReviewPatchAnchorErrors(t *testing.T) {
	repo := newTrailReviewApplyRepo(t)
	writeTrailReviewApplyFile(t, repo, "file.txt")

	tests := []struct {
		name  string
		patch string
		want  string
	}{
		{
			name:  "missing file",
			patch: trailReviewPatch("absent.txt", "old"),
			want:  "anchor the suggested change",
		},
		{
			name:  "hunk past end of file",
			patch: "--- a/file.txt\n+++ b/file.txt\n@@ -40,2 +40,2 @@\n old\n",
			want:  "the file has 2",
		},
		{
			name:  "multiple files",
			patch: trailReviewPatch("file.txt", "old") + trailReviewPatch("other.txt", "old"),
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
			_, err := resolveTrailReviewPatchAnchor(context.Background(), tc.patch)
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestParseTrailReviewPatchTargetSkipsHunkBodies covers diff *content* that
// looks like a diff *header*. Deleting a line that itself starts with "-- "
// (SQL, Lua and Haskell comments, CLI flag docs) serializes as "--- ...", and
// an added line starting with "++ " serializes as "+++ ...". Neither is a file
// header, and treating them as one rejected valid single-file patches.
func TestParseTrailReviewPatchTargetSkipsHunkBodies(t *testing.T) {
	t.Parallel()
	patch := "--- a/schema.sql\n+++ b/schema.sql\n" +
		"@@ -10,4 +10,4 @@\n" +
		" CREATE TABLE t (\n" +
		"--- legacy column, drop after migration\n" +
		"+++ replacement column\n" +
		" );\n"
	target, err := parseTrailReviewPatchTarget(patch)
	if err != nil {
		t.Fatalf("parseTrailReviewPatchTarget: %v", err)
	}
	if target.Path != "schema.sql" {
		t.Fatalf("Path = %q, want schema.sql", target.Path)
	}
	if target.StartLine != 10 || target.EndLine != 13 {
		t.Fatalf("range = %d-%d, want 10-13", target.StartLine, target.EndLine)
	}
}

// TestParseTrailReviewPatchTargetKeepsRealPathPrefix guards a file genuinely
// living under b/ (or a/): the diff header "--- a/b/pkg.go" must resolve to
// b/pkg.go, not pkg.go. Stripping both prefixes in sequence read the wrong file.
func TestParseTrailReviewPatchTargetKeepsRealPathPrefix(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"a", "b"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			want := dir + "/pkg.go"
			patch := "--- a/" + want + "\n+++ b/" + want + "\n@@ -1,2 +1,2 @@\n-old\n+new\n"
			target, err := parseTrailReviewPatchTarget(patch)
			if err != nil {
				t.Fatalf("parseTrailReviewPatchTarget: %v", err)
			}
			if target.Path != want {
				t.Fatalf("Path = %q, want %q", target.Path, want)
			}
		})
	}
}

// TestParseTrailReviewPatchTargetPrependToExistingFile covers "@@ -0,0 +1,N @@",
// which `diff -u0` emits both for a brand-new file and for an insertion above
// line 1 of an existing one. Only a /dev/null old path means creation; the
// prepend anchors on the line it is inserted before.
func TestParseTrailReviewPatchTargetPrependToExistingFile(t *testing.T) {
	t.Parallel()
	patch := "--- a/file.txt\n+++ b/file.txt\n@@ -0,0 +1,1 @@\n+added\n"
	target, err := parseTrailReviewPatchTarget(patch)
	if err != nil {
		t.Fatalf("parseTrailReviewPatchTarget: %v", err)
	}
	if target.StartLine != 1 || target.EndLine != 1 {
		t.Fatalf("range = %d-%d, want 1-1", target.StartLine, target.EndLine)
	}
}

// TestParseTrailReviewPatchTargetAllowsDeletion keeps "delete this" a valid
// suggestion: a /dev/null *new* side still has a real old file to anchor to,
// unlike a /dev/null old side.
func TestParseTrailReviewPatchTargetAllowsDeletion(t *testing.T) {
	t.Parallel()
	patch := "--- a/file.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-hello\n-old\n"
	target, err := parseTrailReviewPatchTarget(patch)
	if err != nil {
		t.Fatalf("parseTrailReviewPatchTarget: %v", err)
	}
	if target.Path != "file.txt" || target.StartLine != 1 || target.EndLine != 2 {
		t.Fatalf("target = %+v, want file.txt 1-2", target)
	}
}

// TestParseTrailReviewPatchTargetRejectsRename gives renames an actionable
// message: the old path is gone from the worktree, so anchoring would otherwise
// fail with a confusing "read <old path>" error.
func TestParseTrailReviewPatchTargetRejectsRename(t *testing.T) {
	t.Parallel()
	patch := "diff --git a/old.go b/new.go\nrename from old.go\nrename to new.go\n" +
		"--- a/old.go\n+++ b/new.go\n@@ -1,2 +1,2 @@\n-old\n+new\n"
	_, err := parseTrailReviewPatchTarget(patch)
	if err == nil {
		t.Fatal("expected a rename patch to be rejected")
	}
	if !strings.Contains(err.Error(), "renames") {
		t.Fatalf("error = %v, want it to mention the rename", err)
	}
}

// TestParseTrailReviewPatchTargetSpansHunks checks that a multi-hunk patch
// anchors to the full span it touches, not just its first hunk.
func TestParseTrailReviewPatchTargetSpansHunks(t *testing.T) {
	t.Parallel()
	// Hunk bodies must match the counts their headers declare, exactly as git
	// emits them — the parser consumes bodies by those counts.
	patch := "--- a/file.txt\n+++ b/file.txt\n" +
		"@@ -20,3 +20,3 @@\n a\n-b\n+c\n g\n" +
		"@@ -5,2 +5,2 @@\n d\n-e\n+f\n"
	target, err := parseTrailReviewPatchTarget(patch)
	if err != nil {
		t.Fatalf("parseTrailReviewPatchTarget: %v", err)
	}
	if target.StartLine != 5 || target.EndLine != 22 {
		t.Fatalf("range = %d-%d, want 5-22", target.StartLine, target.EndLine)
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
	path := trailReviewTestFilePath
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
		trailReviewTestFilePath + ":88",
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

func TestFetchAllTrailReviewCommentsStopsOnRepeatedPage(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Query().Get("include_dismissed"); got != strconv.FormatBool(true) {
			t.Errorf("include_dismissed = %q, want true", got)
		}
		wantOffset := ""
		if requests == 2 {
			wantOffset = strconv.Itoa(defaultTrailReviewLimit)
		}
		if got := r.URL.Query().Get("offset"); got != wantOffset {
			t.Errorf("offset = %q, want %q", got, wantOffset)
		}
		encodeTrailReviewTestJSON(t, w, api.TrailReviewCommentsResponse{
			Comments: []api.TrailReviewComment{{ID: trailReviewTestCommentID}},
			HasMore:  true,
		})
	}))
	defer srv.Close()
	client := api.NewClientWithBaseURL("tok", srv.URL)

	_, err := fetchAllTrailReviewComments(context.Background(), client, "trl_1", trailReviewSummaryOptions())
	if err == nil || !strings.Contains(err.Error(), "repeated page") {
		t.Fatalf("error = %v, want repeated page", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestFetchTrailReviewStateFollowsCursor(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/trails/trl_1/reviews/rvw_1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if got := r.URL.Query().Get("include_dismissed"); got != strconv.FormatBool(true) {
			t.Fatalf("include_dismissed = %q, want true", got)
		}
		if got := r.URL.Query().Get("limit"); got != strconv.Itoa(defaultTrailReviewLimit) {
			t.Fatalf("limit = %q, want %d", got, defaultTrailReviewLimit)
		}
		if got := r.URL.Query().Get("stale"); got != trailReviewFreshnessAny {
			t.Fatalf("stale = %q, want %q", got, trailReviewFreshnessAny)
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

// trailReviewGitOutput runs git in dir and returns its trimmed stdout, failing
// the test on error. runTrailReviewApplyGit is the same thing where the output
// is not interesting.
func trailReviewGitOutput(t *testing.T, dir string, args ...string) string {
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

func runTrailReviewApplyGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	trailReviewGitOutput(t, dir, args...)
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
