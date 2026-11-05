package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const guardRepoID = "01ARZ3NDEKTSV4RRFFQ69G5FRP"

// setupGuardRepo checks out feature/mine in a clone of gh/acme/widget.
func setupGuardRepo(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	testutil.RunGit(t, dir, "checkout", "-b", "feature/mine")
	t.Chdir(dir)
}

// guardServer serves project trail #42 and answers its PATCH with patch,
// recording each PATCH query.
type guardServer struct {
	patches []url.Values
	patch   func(w http.ResponseWriter)
	baseURL string
}

func serveGuardProject(t *testing.T) *guardServer {
	t.Helper()
	g := &guardServer{patch: func(w http.ResponseWriter) {
		w.Header().Set("Entire-Branch-Check", "checked")
		w.Header().Set("ETag", `W/"v2"`)
		assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
	}}
	_, server := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			g.patches = append(g.patches, r.URL.Query())
			g.patch(w)
			return
		}
		w.Header().Set("ETag", `W/"v1"`)
		assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
	})
	g.baseURL = server.URL
	stubGuardPlacement(t, func(context.Context, string, string, string) (repoCellPlacement, error) {
		return repoCellPlacement{RepoID: guardRepoID, Target: &auth.CellTarget{BaseURL: server.URL, Jurisdiction: "eu"}}, nil
	})
	return g
}

func stubGuardPlacement(t *testing.T, fn func(context.Context, string, string, string) (repoCellPlacement, error)) {
	t.Helper()
	old := resolveTrailUpdatePlacement
	resolveTrailUpdatePlacement = fn
	t.Cleanup(func() { resolveTrailUpdatePlacement = old })
}

func refuseWith(t *testing.T, code string, extra map[string]any) func(http.ResponseWriter) {
	t.Helper()
	return func(w http.ResponseWriter) {
		body := map[string]any{"error": "refused", "code": code}
		for k, v := range extra {
			body[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		assert.NoError(t, json.NewEncoder(w).Encode(body))
	}
}

const guardAnyway = "To update #42 anyway, rerun with " + projectTrailTestID + " --project gh/acme in place of 42, keeping your other flags."

// A bare number names a project trail, which can belong to someone else: a
// change number (cli/1744) or pull request number typed as `update 1744`
// reached unrelated project trail #1744 and replaced its description. The
// update now carries the branch, and the server refuses it atomically.
func TestProjectTrailUpdateBareNumberSendsTheBranchForTheServerToCheck(t *testing.T) {
	setupGuardRepo(t)
	srv := serveGuardProject(t)

	_, errOut, err := executeProjectTrailTest(t, "update", "42", "--body", "x")

	require.NoError(t, err)
	require.Len(t, srv.patches, 1)
	require.Equal(t, url.Values{"from_repo_id": {guardRepoID}, "from_branch": {"feature/mine"}, "branch_check": {"required"}}, srv.patches[0])
	require.Empty(t, errOut, "a checked update prints no note")
}

func TestProjectTrailUpdateBareNumberWordsTheServerRefusal(t *testing.T) {
	for _, tt := range []struct {
		name    string
		code    string
		extra   map[string]any
		want    []string
		notWant []string
	}{
		{name: "another trail", code: "trail_branch_mismatch", extra: map[string]any{"change_number": 7}, want: []string{
			`refusing to update trail #42 "Project intent": branch "feature/mine" is work on another trail, not on #42; nothing was updated.`,
			"To update this branch's trail, rerun without 42, keeping your other flags.", guardAnyway,
		}, notWant: []string{"change number"}},
		{name: "the number is this branch's change number", code: "trail_branch_mismatch", extra: map[string]any{"change_number": 42}, want: []string{
			"42 is this branch's change number, and a bare number names a project trail. For the change, rerun with widget/42 in place of 42",
		}},
		{name: "the server could not check", code: "trail_branch_unchecked", want: []string{
			`branch "feature/mine" could not be checked by the server; nothing was updated`, guardAnyway,
		}, notWant: []string{"rerun without 42"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupGuardRepo(t)
			srv := serveGuardProject(t)
			srv.patch = refuseWith(t, tt.code, tt.extra)

			_, _, err := executeProjectTrailTest(t, "update", "42", "--body", "x")

			require.Error(t, err)
			for _, want := range tt.want {
				require.Contains(t, err.Error(), want)
			}
			for _, notWant := range tt.notWant {
				require.NotContains(t, err.Error(), notWant)
			}
		})
	}
}

// Another 409 is not a branch refusal and keeps its own message.
func TestProjectTrailUpdateBareNumberPassesOtherConflictsThrough(t *testing.T) {
	setupGuardRepo(t)
	srv := serveGuardProject(t)
	srv.patch = refuseWith(t, "conflict", nil)

	_, _, err := executeProjectTrailTest(t, "update", "42", "--body", "x")

	require.ErrorContains(t, err, "API error: refused (status 409)")
	require.NotContains(t, err.Error(), "refusing to update")
}

// An older server ignores the parameters and writes without checking; the
// write cannot be undone, but the user is told to verify it.
func TestProjectTrailUpdateBareNumberNotesAnUnconfirmedCheck(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%v", jsonOut), func(t *testing.T) {
			setupGuardRepo(t)
			srv := serveGuardProject(t)
			args := []string{"update", "42", "--body", "x"}
			if jsonOut {
				args = append(args, "--json")
			}
			want, _, err := executeProjectTrailTest(t, args...)
			require.NoError(t, err)
			srv.patch = func(w http.ResponseWriter) {
				w.Header().Set("ETag", `W/"v2"`)
				assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
			}

			out, errOut, err := executeProjectTrailTest(t, args...)

			require.NoError(t, err)
			require.Equal(t, want, out, "stdout must not change")
			require.Contains(t, errOut, `Note: the server did not confirm that branch "feature/mine" has no work on another trail; check #42 (`+projectTrailTestID+")")
		})
	}
}

// Where the branch is not what names the trail, nothing is sent.
func TestProjectTrailUpdateSendsNoBranchWhenItCannotMisfire(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "ULID names the trail exactly", args: []string{"update", projectTrailTestID, "--project", "gh/acme", "--body", "x"}},
		{name: "another project", args: []string{"update", "42", "--project", "gh/other", "--body", "x"}},
		{name: "--repo names another repository", args: []string{"update", "42", "--repo", "gh/acme/other", "--body", "x"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupGuardRepo(t)
			srv := serveGuardProject(t)
			stubGuardPlacement(t, func(context.Context, string, string, string) (repoCellPlacement, error) {
				t.Error("no repository resolution when the guard does not apply")
				return repoCellPlacement{}, errors.New("unexpected")
			})

			_, _, err := executeProjectTrailTest(t, tt.args...)

			require.NoError(t, err)
			require.Len(t, srv.patches, 1)
			require.Empty(t, srv.patches[0])
		})
	}
}

// --repo naming this clone's repository still targets #42 by bare number, so
// the branch is still sent.
func TestProjectTrailUpdateBareNumberWithThisRepoKeepsTheGuard(t *testing.T) {
	setupGuardRepo(t)
	srv := serveGuardProject(t)

	_, _, err := executeProjectTrailTest(t, "update", "42", "--repo", "gh/acme/widget", "--body", "x")

	require.NoError(t, err)
	require.Len(t, srv.patches, 1)
	require.Equal(t, "feature/mine", srv.patches[0].Get("from_branch"))
}

// A repository not onboarded to Entire cannot have trail work, so the update
// proceeds without asking the server to check.
func TestProjectTrailUpdateBareNumberProceedsForARepositoryNotOnboarded(t *testing.T) {
	setupGuardRepo(t)
	srv := serveGuardProject(t)
	stubGuardPlacement(t, func(context.Context, string, string, string) (repoCellPlacement, error) {
		return repoCellPlacement{}, &repoNotOnboardedError{inner: errors.New("not found")}
	})

	_, _, err := executeProjectTrailTest(t, "update", "42", "--body", "x")

	require.NoError(t, err)
	require.Len(t, srv.patches, 1)
	require.Empty(t, srv.patches[0])
}

// Where nothing checkable can be sent, a bare number could still name someone
// else's trail, so the update refuses locally and names the ULID rerun.
func TestProjectTrailUpdateBareNumberRefusesWhenNothingCanBeChecked(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, srv *guardServer)
		want  []string
	}{
		{name: "detached HEAD", setup: func(t *testing.T, _ *guardServer) {
			t.Helper()
			testutil.RunGit(t, ".", "checkout", "--detach")
		}, want: []string{"HEAD is not on a branch", "rerun with --branch <branch> added"}},
		{name: "repository does not resolve; error text sanitized to one line", setup: func(t *testing.T, _ *guardServer) {
			t.Helper()
			stubGuardPlacement(t, func(context.Context, string, string, string) (repoCellPlacement, error) {
				return repoCellPlacement{}, errors.New("boom\x1b]0;title\x07\x1b[2J\nsecond\tline")
			})
		}, want: []string{`branch "feature/mine" could not be checked: its repository did not resolve (boom second line)`}},
		{name: "repository processed in another cell", setup: func(t *testing.T, _ *guardServer) {
			t.Helper()
			stubGuardPlacement(t, func(context.Context, string, string, string) (repoCellPlacement, error) {
				return repoCellPlacement{RepoID: guardRepoID, Target: &auth.CellTarget{BaseURL: "https://other-cell.example", Jurisdiction: "us"}}, nil
			})
		}, want: []string{"its repository is processed in another cell than the trail"}},
		{name: "repository id the server would reject", setup: func(t *testing.T, srv *guardServer) {
			t.Helper()
			stubGuardPlacement(t, func(context.Context, string, string, string) (repoCellPlacement, error) {
				return repoCellPlacement{RepoID: "not-a-ulid", Target: &auth.CellTarget{BaseURL: srv.baseURL}}, nil
			})
		}, want: []string{"the server cannot accept this branch or repository for checking"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupGuardRepo(t)
			srv := serveGuardProject(t)
			tt.setup(t, srv)

			_, _, err := executeProjectTrailTest(t, "update", "42", "--body", "x")

			require.Error(t, err)
			require.Empty(t, srv.patches, "no PATCH may reach a trail nothing could check")
			msg := err.Error()
			require.Contains(t, msg, `refusing to update trail #42 "Project intent"`)
			require.Contains(t, msg, guardAnyway)
			require.NotContains(t, msg, "rerun without 42", "the no-selector rerun depends on the same unknown")
			require.NotContains(t, msg, "\x1b")
			require.NotContains(t, msg, "\x07")
			for _, want := range tt.want {
				require.Contains(t, msg, want)
			}
		})
	}
}

func TestProjectTrailNumberMismatchSanitizesServerText(t *testing.T) {
	t.Parallel()
	current := api.ProjectTrail{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV\x1b]0;x\x07", Title: "T\x1b[2J"}
	g := projectTrailNumberGuard{number: 42, current: current, target: &projectTrailTarget{Host: "gh", Project: "acme"}, repo: "widget\x1b[2J", branch: "feature/mine"}
	for _, msg := range []string{g.mismatch(7), g.mismatch(42)} {
		require.NotContains(t, msg, "\x1b")
		require.NotContains(t, msg, "\x07")
	}
}

// A pair the server's validation rejects would fail the whole update, so it is
// never sent. Branch names over the limit cannot be created on most file
// systems, so the length bound is tested here rather than through git.
func TestBranchCheckSendable(t *testing.T) {
	t.Parallel()
	require.True(t, branchCheckSendable(guardRepoID, "feature/mine"))
	require.True(t, branchCheckSendable(guardRepoID, strings.Repeat("x", maxTrailBranchCheckBytes)))
	require.False(t, branchCheckSendable(guardRepoID, strings.Repeat("x", maxTrailBranchCheckBytes+1)))
	require.False(t, branchCheckSendable("not-a-ulid", "feature/mine"))
	require.False(t, branchCheckSendable(guardRepoID, ""))
}
