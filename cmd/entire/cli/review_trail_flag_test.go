package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	cliReview "github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const reviewTrailTestHead = "0123456789abcdef0123456789abcdef01234567"

// setupReviewTrailFlag serves trail #42, whose only change is widget/7 on
// feature/work, to a clone of acme/widget, and reports head as the head the
// review starts on. It returns the number of findings batches posted.
// Not parallel: changes CWD and replaces client constructors.
func setupReviewTrailFlag(t *testing.T, head *string) *int {
	t.Helper()
	return setupReviewTrailFlagMoving(t, head, new(bool))
}

// setupReviewTrailFlagMoving is setupReviewTrailFlag with the change served
// under another project trail once *moved is set.
func setupReviewTrailFlagMoving(t *testing.T, head *string, moved *bool) *int {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.RunGit(t, dir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	t.Chdir(dir)
	otherPath := "/api/v1/gh/acme/trails/" + reviewTrailOtherParent
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case *moved && r.URL.Path == otherPath:
			parent := workingProjectTestResource()
			parent.ID, parent.Number = reviewTrailOtherParent, 43
			assert.NoError(t, json.NewEncoder(w).Encode(parent))
		case *moved && r.URL.Path == otherPath+"/changes/"+projectTrailTestChange:
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": projectTrailTestChange, "number": 7, "trailId": reviewTrailOtherParent, "repositoryId": "repo-id", "branch": "feature/work", "base": "main", "status": "open"}))
		case !serveWorkingProjectRead(t, w, r):
			t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	batches := 0
	work := api.TrailResource{ID: projectTrailTestChange, Number: 7, Branch: "feature/work",
		Parent: &api.TrailParentReference{ID: projectTrailTestID, Number: 42, ProjectID: projectTrailTestProject,
			Host: "gh", Project: "acme", Path: projectTrailTestPath, Jurisdiction: "eu", PrimaryProcessingCell: "project-cell"}}
	served := func() api.TrailResource {
		if !*moved {
			return work
		}
		parent := *work.Parent
		parent.ID, parent.Number, parent.Path = reviewTrailOtherParent, 43, otherPath
		changed := work
		changed.Parent = &parent
		return changed
	}
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/repos/repo-id/changes":
			assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{served()}}))
		case "GET /api/v1/repos/repo-id/changes/7":
			assert.NoError(t, json.NewEncoder(w).Encode(served()))
		case "POST " + trailReviewTestStartPath:
			assert.NoError(t, json.NewEncoder(w).Encode(api.TrailReviewStartResponse{ReviewID: "rvw_1", TrailID: projectTrailTestChange, HeadSHA: head}))
		case "POST " + trailReviewTestCommentsPath:
			batches++
			var batch api.TrailReviewCommentBatchRequest
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&batch))
			results := make([]api.TrailReviewCommentBatchResult, 0, len(batch.Comments))
			for _, c := range batch.Comments {
				results = append(results, api.TrailReviewCommentBatchResult{ClientID: c.ClientID, Status: "created",
					Comment: &api.TrailReviewComment{ID: "cm_1", ReviewID: "rvw_1", Status: trailReviewStatusOpen}})
			}
			assert.NoError(t, json.NewEncoder(w).Encode(api.TrailReviewCommentBatchResponse{Results: results}))
		default:
			t.Errorf("unexpected repository request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	return &batches
}

func reviewTrailTestCommand(out *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{Use: "review"}
	cmd.SetContext(context.Background())
	cmd.SetOut(out)
	cmd.SetErr(out)
	return cmd
}

// --trail resolves the trail by number or by branch before the review runs,
// then posts only when the trail's head is the commit that was reviewed:
// findings carry no commit, so a stale head would attach them to code the
// review never read. Not parallel: changes CWD and replaces client constructors.
func TestResolveReviewTrailPostsOnlyOnTheReviewedHead(t *testing.T) {
	head := reviewTrailTestHead
	other := "fedcba9876543210fedcba9876543210fedcba98"
	for _, tt := range []struct {
		name     string
		selector string
		head     *string
		reviewed string
		posted   bool
		wantErr  string
	}{
		{"number on its head", "42", &head, strings.ToUpper(head), true, ""},
		{"branch on its head", "feature/work", &head, head, true, ""},
		{"head moved on", "42", &other, head, false, "the trail's head is fedcba98, not the reviewed commit 01234567"},
		{"head not reported", "42", nil, head, false, "the trail did not report its head commit"},
		{"reviewed commit unknown", "42", &head, "", false, "the reviewed commit is unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			batches := setupReviewTrailFlag(t, tt.head)
			var out bytes.Buffer
			cmd := reviewTrailTestCommand(&out)
			post, err := resolveReviewTrail(cmd, tt.selector)
			require.NoError(t, err)
			require.Contains(t, out.String(), "Findings will be posted to trail #42")
			require.Contains(t, out.String(), "feature/work")

			out.Reset()
			err = post(context.Background(), &out, "general", cliReview.TrailReport{Prose: "a finding", HeadSHA: tt.reviewed})
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				require.Contains(t, out.String(), "Posted the review verdict to trail #42")
			}
			require.Equal(t, map[bool]int{true: 1, false: 0}[tt.posted], *batches)
		})
	}
}

// twoChangeReviewTrail serves trail #42 to a clone of acme/widget checked out
// on feature/work. Before the review the trail's changes in this repository
// are before; afterwards, after.
// Not parallel: changes CWD and replaces client constructors.
func twoChangeReviewTrail(t *testing.T, before, after []api.ChangeSummary) (posting *bool) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.RunGit(t, dir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	testutil.WriteFile(t, dir, "README.md", "widget")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "initial")
	testutil.RunGit(t, dir, "checkout", "-b", "feature/work")
	t.Chdir(dir)
	posting = new(bool)
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		parent := projectTrailTestResource()
		parent.Changes = before
		if *posting {
			parent.Changes = after
		}
		if r.URL.Path == "/api/v1/gh/acme/trails/42" || r.URL.Path == projectTrailTestPath {
			assert.NoError(t, json.NewEncoder(w).Encode(parent))
			return
		}
		for _, change := range parent.Changes {
			if r.URL.Path == projectTrailTestPath+"/changes/"+change.ID {
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": change.ID, "number": change.Number, "trailId": projectTrailTestID, "repositoryId": "repo-id", "branch": change.Branch, "status": "open"}))
				return
			}
		}
		t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodGet:
			t.Errorf("must not post: %s %s", r.Method, r.URL.Path)
		case strings.HasSuffix(r.URL.Path, "/approvals"):
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"approvals": []any{}}))
			return
		}
		// A failed lookup also re-probes trails enablement.
		http.NotFound(w, r)
	})
	return posting
}

const (
	reviewTrailOtherChange = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
	reviewTrailOtherParent = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
)

// The findings go only to the change announced before the review, even when
// the selector resolves to another one by the time they are posted.
// Not parallel: changes CWD and replaces client constructors.
func TestResolveReviewTrailPostsOnlyToTheAnnouncedChange(t *testing.T) {
	posting := twoChangeReviewTrail(t,
		[]api.ChangeSummary{{ID: projectTrailTestChange, RepositoryID: "repo-id", Repository: "widget", Number: 7, Title: "Work", Branch: "feature/work"}},
		[]api.ChangeSummary{{ID: reviewTrailOtherChange, RepositoryID: "repo-id", Repository: "widget", Number: 8, Title: "Other", Branch: "feature/other"}})

	var out bytes.Buffer
	post, err := resolveReviewTrail(reviewTrailTestCommand(&out), "42")
	require.NoError(t, err)
	require.Contains(t, out.String(), "feature/work")
	*posting = true
	err = post(context.Background(), &out, "general", cliReview.TrailReport{Prose: "a finding", HeadSHA: reviewTrailTestHead})
	require.ErrorContains(t, err, "--trail 42 now resolves to")
	require.ErrorContains(t, err, "so the findings were not posted")
}

// A change moved to another project trail during the review is not the
// trail the run announced, so nothing is posted to it.
// Not parallel: changes CWD and replaces client constructors.
func TestResolveReviewTrailPostsOnlyToTheAnnouncedTrail(t *testing.T) {
	head := reviewTrailTestHead
	moved := new(bool)
	batches := setupReviewTrailFlagMoving(t, &head, moved)
	var out bytes.Buffer
	post, err := resolveReviewTrail(reviewTrailTestCommand(&out), "feature/work")
	require.NoError(t, err)
	require.Contains(t, out.String(), "Findings will be posted to trail #42")
	*moved = true
	err = post(context.Background(), &out, "general", cliReview.TrailReport{Prose: "a finding", HeadSHA: head})
	require.ErrorContains(t, err, "--trail feature/work now resolves to")
	require.ErrorContains(t, err, "so the findings were not posted")
	require.Zero(t, *batches)
}

// What is checked out never picks the change a review posts to: a trail with
// several changes in this repository is listed with the command to re-run.
// Not parallel: changes CWD and replaces client constructors.
func TestResolveReviewTrailListsSeveralLocalChanges(t *testing.T) {
	changes := []api.ChangeSummary{
		{ID: projectTrailTestChange, RepositoryID: "repo-id", Repository: "widget", Number: 7, Title: "Work", Branch: "feature/work"},
		{ID: reviewTrailOtherChange, RepositoryID: "repo-id", Repository: "widget", Number: 8, Title: "Other", Branch: "feature/other"},
	}
	twoChangeReviewTrail(t, changes, changes)

	var out bytes.Buffer
	_, err := resolveReviewTrail(reviewTrailTestCommand(&out), "42")
	require.ErrorContains(t, err, "widget/7 - Work")
	require.ErrorContains(t, err, "widget/8 - Other")
	require.ErrorContains(t, err, "review --trail=widget/")
	require.NotContains(t, out.String(), "Findings will be posted")
}

// A branch with no trail fails at once, before any reviewer would run.
// Not parallel: changes CWD and replaces client constructors.
func TestResolveReviewTrailRejectsABranchWithoutATrail(t *testing.T) {
	batches := setupReviewTrailFlag(t, nil)
	var out bytes.Buffer
	_, err := resolveReviewTrail(reviewTrailTestCommand(&out), "feature/none")
	require.ErrorContains(t, err, "feature/none")
	require.Zero(t, *batches)
}

func TestReviewTrailSelectorIsBranch(t *testing.T) {
	t.Parallel()
	for selector, want := range map[string]bool{
		"42":                         false,
		projectTrailTestID:           false,
		"cli/12":                     false,
		"feature/work":               true,
		"main":                       true,
		"claude/project-thread-abcd": true,
	} {
		assert.Equal(t, want, reviewTrailSelectorIsBranch(selector), selector)
	}
}

// A review's retry hint names the change with --trail, since review takes no
// trail positional argument.
func TestTrailChangeRetryCommandForReview(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "entire"}
	root.AddCommand(cliReview.NewCommand(buildReviewDeps()))
	cmd, rest, err := root.Find([]string{"review", "general", "--timeout", "5m", "--trail", "42"})
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(rest))
	require.Equal(t, "entire review general --timeout=5m0s --trail=widget/7", trailChangeRetryCommand(cmd, "42", "widget/7", "gh/acme"))
}
