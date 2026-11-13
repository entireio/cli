package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

const testDefaultBranchRepoULID = "01KS6KFJR2XS6PZ188MVYE07AP"

func TestDefaultBranchMessage(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "✓ Default branch: refs/heads/master (was refs/heads/main)", defaultBranchMessage(&coreapi.SetDefaultBranchOutputBody{
		Branch: "refs/heads/master", Previous: "refs/heads/main", Changed: true,
	}))
	assert.Equal(t, "Default branch already refs/heads/master", defaultBranchMessage(&coreapi.SetDefaultBranchOutputBody{
		Branch: "refs/heads/master", Previous: "refs/heads/master",
	}))
}

// fakeDefaultBranchServer stands in for core.
type fakeDefaultBranchServer struct {
	mu       sync.Mutex
	status   int    // PUT status; 0 means 200
	detail   string // problem detail on a non-200 PUT
	previous string
	bodies   []map[string]any
}

func (f *fakeDefaultBranchServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/repos/"+testDefaultBranchRepoULID+"/default-branch":
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode body %s: %v", raw, err)
			}
			f.bodies = append(f.bodies, body)
			if f.status != 0 {
				writeCoreProblem(t, w, f.status, f.detail)
				return
			}
			branch, ok := body["branch"].(string)
			if !ok {
				t.Errorf("branch is not a string: %v", body["branch"])
			}
			full := "refs/heads/" + branch
			w.Header().Set("Content-Type", "application/json")
			if err := printJSON(w, map[string]any{
				"repo_id": testDefaultBranchRepoULID, "branch": full, "previous": f.previous, "changed": full != f.previous,
			}); err != nil {
				t.Errorf("encode response: %v", err)
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newDefaultBranchServer(t *testing.T, f *fakeDefaultBranchServer) string {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Not parallel: runCoreCmd swaps a package seam.
func TestRepoDefaultBranch_Sets(t *testing.T) {
	fake := &fakeDefaultBranchServer{previous: "refs/heads/main"}
	srvURL := newDefaultBranchServer(t, fake)

	out, _, err := runCoreCmd(t, newRepoDefaultBranchCmd, srvURL, testDefaultBranchRepoULID, "master")
	require.NoError(t, err)
	assert.Equal(t, "✓ Default branch: refs/heads/master (was refs/heads/main)\n", out)
	require.Len(t, fake.bodies, 1)
	assert.Equal(t, map[string]any{"branch": "master"}, fake.bodies[0])

	out, _, err = runCoreCmd(t, newRepoDefaultBranchCmd, srvURL, testDefaultBranchRepoULID, "master", "--json")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, map[string]any{
		"repo_id":  testDefaultBranchRepoULID,
		"branch":   "refs/heads/master",
		"previous": "refs/heads/main",
		"changed":  true,
	}, got)
}

// Not parallel: runCoreCmd swaps a package seam.
func TestRepoDefaultBranch_Unchanged(t *testing.T) {
	fake := &fakeDefaultBranchServer{previous: "refs/heads/master"}
	srvURL := newDefaultBranchServer(t, fake)

	out, _, err := runCoreCmd(t, newRepoDefaultBranchCmd, srvURL, testDefaultBranchRepoULID, "master")
	require.NoError(t, err)
	assert.Equal(t, "Default branch already refs/heads/master\n", out)
}

// Not parallel: runCoreCmd swaps a package seam.
func TestRepoDefaultBranch_Errors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		detail string
		want   string
	}{
		{name: "github mirror", status: http.StatusConflict, detail: "a GitHub mirror follows GitHub's default branch", want: "a GitHub mirror follows GitHub's default branch"},
		{name: "native mirror placement", status: http.StatusConflict, detail: "not the primary placement", want: "not the primary placement"},
		{name: "missing branch", status: http.StatusNotFound, detail: "branch does not exist", want: "branch does not exist"},
		{name: "forbidden", status: http.StatusForbidden, detail: "permission denied", want: "permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDefaultBranchServer{status: tc.status, detail: tc.detail}
			srvURL := newDefaultBranchServer(t, fake)

			_, _, err := runCoreCmd(t, newRepoDefaultBranchCmd, srvURL, testDefaultBranchRepoULID, "master")
			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())
		})
	}
}

// Not parallel: runCoreCmd swaps a package seam.
func TestRepoDefaultBranch_EmptyBranch(t *testing.T) {
	fake := &fakeDefaultBranchServer{}
	srvURL := newDefaultBranchServer(t, fake)

	_, _, err := runCoreCmd(t, newRepoDefaultBranchCmd, srvURL, testDefaultBranchRepoULID, " ")
	require.Error(t, err)
	assert.Empty(t, fake.bodies, "no request for an empty branch")
}
