package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

const (
	testProtectionRepoULID = "01KS6KFJR2XS6PZ188MVYE07AN"
	// testProtectionFullName is the <project>/<repo> the fake resolves to
	// testProtectionRepoULID; testProtectionPath is how a user names it.
	testProtectionFullName = "acme/web"
	testProtectionPath     = "/et/" + testProtectionFullName
)

func TestExpandBranchRef(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"main":               "refs/heads/main",
		"release/*":          "refs/heads/release/*",
		"HEAD":               "HEAD",
		"refs/heads/main":    "refs/heads/main",
		"refs/heads/hotfix/": "refs/heads/hotfix/",
	}
	for in, want := range cases {
		got, err := expandBranchRef(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := expandBranchRef("")
	require.Error(t, err)
}

func TestProtectionRow(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"refs/heads/main", protectionLevelMergeOnly}, protectionRow(branchRule{Ref: "refs/heads/main", ServerSideMergeOnly: true}))
	assert.Equal(t, []string{"HEAD", protectionLevelProtected}, protectionRow(branchRule{Ref: "HEAD"}))
}

// fakeProtectionServer stands in for core's branch-protection resource. It
// applies PATCH bodies with the server's upsert-by-ref rule so a test sees the
// same resulting list the real core would return.
type fakeProtectionServer struct {
	mu      sync.Mutex
	rules   []coreapi.BranchRule
	patches []coreapi.UpdateBranchProtectionInputBody
	// mirrors are the GitHub owner/repo names the /repos directory lists;
	// lookups records each filter it was asked for.
	mirrors []string
	lookups []string
}

func (f *fakeProtectionServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/resolve" {
			writeJSONResponse(t, w, http.StatusOK, nativeResolution(testProtectionFullName, testProtectionRepoULID))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos" {
			filter := r.URL.Query().Get("filter")
			f.mu.Lock()
			f.lookups = append(f.lookups, filter)
			mirrors := f.mirrors
			f.mu.Unlock()
			// The directory filter is fuzzy; the near miss proves the CLI
			// matches the full name exactly rather than taking any row.
			rows := []string{`{"id":"01KS6KFJR2XS6PZ188MVYE0NMS","name":"x","full_name":"` + filter + `-fork","cell":"c","clusterSlug":"s","jurisdiction":"us","visibility":"public","placements":[]}`}
			for _, m := range mirrors {
				if m == filter {
					rows = append(rows, `{"id":"01KS6KFJR2XS6PZ188MVYE0MRR","name":"x","full_name":"`+m+`","cell":"c","clusterSlug":"s","jurisdiction":"us","visibility":"public","placements":[]}`)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"repos":[%s],"truncated":false}`, strings.Join(rows, ","))
			return
		}
		if r.URL.Path != "/api/v1/repos/"+testProtectionRepoULID+"/branch-protection" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
		case http.MethodPatch:
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read patch body: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var body coreapi.UpdateBranchProtectionInputBody
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode patch body %s: %v", raw, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.patches = append(f.patches, body)
			f.apply(body)
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := printJSON(w, &coreapi.BranchProtection{Rules: f.rules}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}
}

func (f *fakeProtectionServer) apply(body coreapi.UpdateBranchProtectionInputBody) {
	var next []coreapi.BranchRule
	for _, r := range f.rules {
		removed := false
		for _, ref := range body.RemoveRefs {
			if ref == r.Ref {
				removed = true
			}
		}
		if removed {
			continue
		}
		// Like the server: an entry without a level keeps the rule's.
		for _, a := range body.AddRules {
			if a.Ref == r.Ref && a.ServerSideMergeOnly.IsSet() {
				r.ServerSideMergeOnly = a.ServerSideMergeOnly
			}
		}
		next = append(next, r)
	}
	for _, a := range body.AddRules {
		present := false
		for _, r := range next {
			if r.Ref == a.Ref {
				present = true
			}
		}
		if !present {
			next = append(next, coreapi.BranchRule{Ref: a.Ref, ServerSideMergeOnly: a.ServerSideMergeOnly})
		}
	}
	f.rules = next
}

// newProtectionFixture installs the fake as the active core client.
// Not parallel: swaps the package-level activeCoreClient seam.
func newProtectionFixture(t *testing.T, rules ...coreapi.BranchRule) *fakeProtectionServer {
	t.Helper()
	fake := &fakeProtectionServer{rules: rules}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })
	return fake
}

// rulesView projects wire rules onto the comparable view; the decoded wire
// structs carry an empty AdditionalProps map that a literal does not.
func rulesView(rs []coreapi.BranchRule) []branchRule {
	return branchRulesFromWire(&coreapi.BranchProtection{Rules: rs})
}

// updatesView is rulesView for the rules a patch sends, which the spec types
// separately from the rules a read returns.
func updatesView(us []coreapi.BranchRuleUpdate) []branchRule {
	rs := make([]coreapi.BranchRule, len(us))
	for i, u := range us {
		rs[i] = coreapi.BranchRule{Ref: u.Ref, ServerSideMergeOnly: u.ServerSideMergeOnly}
	}
	return rulesView(rs)
}

func execRepoProtection(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	stdout, _, err = execRepoProtectionBothStreams(t, args...)
	return stdout, err
}

func execRepoProtectionBothStreams(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	parent := &cobra.Command{Use: "repo"}
	addControlPlaneFlags(parent)
	parent.AddCommand(newRepoProtectionCmd())
	var out, errOut bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errOut)
	parent.SetArgs(append([]string{"protection"}, args...))
	err = parent.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

func TestRepoProtection_ListEmpty(t *testing.T) {
	newProtectionFixture(t)
	out, err := execRepoProtection(t, "list", testProtectionPath)
	require.NoError(t, err)
	assert.Equal(t, protectionEmpty+"\n", out)

	out, err = execRepoProtection(t, "list", testProtectionPath, "--json")
	require.NoError(t, err)
	assert.Equal(t, "[]", strings.TrimSpace(out), "an empty list is a JSON array, not null")
}

// A GitHub mirror reads as empty from core, but "nothing is protected" would
// misstate it: its default branch is always protected and its rules are the
// upstream's. list says so once the directory confirms the mirror exists — the
// note claims its default branch is protected, which is false of a typo — and
// --json keeps the plain array on stdout with the caveat on stderr. add and
// remove are refused on the ref's forge alone, with no lookup.
func TestRepoProtection_Mirror(t *testing.T) {
	fake := newProtectionFixture(t)
	fake.mirrors = []string{"acme/widget"}
	const mirror = "/gh/acme/widget"

	out, err := execRepoProtection(t, "list", mirror)
	require.NoError(t, err)
	assert.Equal(t, protectionMirrorNote+"\n", out)
	assert.Equal(t, []string{"acme/widget"}, fake.lookups, "the mirror is looked up by its exact name")

	out, errOut, err := execRepoProtectionBothStreams(t, "list", mirror, "--json")
	require.NoError(t, err)
	assert.Equal(t, "[]", strings.TrimSpace(out), "stdout stays a bare array")
	assert.Equal(t, protectionMirrorNote+"\n", errOut, "the caveat reaches --json callers on stderr")

	out, err = execRepoProtection(t, "list", "/gh/acme/typo")
	require.EqualError(t, err, "no GitHub mirror /gh/acme/typo visible from your login (`entire repo mirror list` shows the mirrors you can see)")
	assert.Empty(t, out, "an unknown mirror gets no claim about its default branch")

	fake.lookups = nil
	const refused = `"` + mirror + `" is a GitHub mirror ref: a mirror's branch protection is governed by its upstream repository, so no rules can be added or removed here`
	_, err = execRepoProtection(t, "add", mirror, "main")
	require.EqualError(t, err, refused)
	_, err = execRepoProtection(t, "remove", mirror, "main")
	require.EqualError(t, err, refused)
	assert.Empty(t, fake.patches)
	assert.Empty(t, fake.lookups, "a write is refused on the forge alone")

	_, err = execRepoProtection(t, "list", "/gh/acme")
	require.ErrorContains(t, err, "/gh/acme", "a malformed mirror ref keeps the parser's reason")
}

func TestRepoProtection_ListShowsLevels(t *testing.T) {
	newProtectionFixture(t,
		coreapi.BranchRule{Ref: "HEAD", ServerSideMergeOnly: coreapi.NewOptBool(true)},
		coreapi.BranchRule{Ref: "refs/heads/release/*"},
	)
	out, err := execRepoProtection(t, "list", testProtectionPath)
	require.NoError(t, err)
	assert.Contains(t, out, "BRANCH")
	assert.Contains(t, out, "HEAD")
	assert.Contains(t, out, protectionLevelMergeOnly)
	assert.Contains(t, out, "refs/heads/release/*")
	assert.Contains(t, out, protectionLevelProtected)

	out, err = execRepoProtection(t, "list", testProtectionPath, "--json")
	require.NoError(t, err)
	var got []branchRule
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, []branchRule{{Ref: "HEAD", ServerSideMergeOnly: true}, {Ref: "refs/heads/release/*"}}, got)
}

// add expands a short branch name and sends a level only when the flag was
// given, so re-adding a branch without it can never lower the rule; remove
// sends only removeRefs. Each verb prints the resulting rules.
func TestRepoProtection_AddAndRemove(t *testing.T) {
	fake := newProtectionFixture(t, coreapi.BranchRule{Ref: "HEAD"})

	out, err := execRepoProtection(t, "add", testProtectionPath, "release/*")
	require.NoError(t, err)
	require.Len(t, fake.patches, 1)
	assert.Equal(t, []branchRule{{Ref: "refs/heads/release/*"}}, updatesView(fake.patches[0].AddRules))
	assert.False(t, fake.patches[0].AddRules[0].ServerSideMergeOnly.IsSet(), "no flag, no level on the wire")
	assert.Empty(t, fake.patches[0].RemoveRefs)
	assert.Contains(t, out, "refs/heads/release/*")

	out, err = execRepoProtection(t, "add", testProtectionPath, "HEAD", "--server-side-merge-only")
	require.NoError(t, err)
	require.Len(t, fake.patches, 2)
	assert.Equal(t, []branchRule{{Ref: "HEAD", ServerSideMergeOnly: true}}, updatesView(fake.patches[1].AddRules))
	assert.True(t, fake.patches[1].AddRules[0].ServerSideMergeOnly.IsSet())
	assert.Contains(t, out, protectionLevelMergeOnly)
	assert.Equal(t, []branchRule{
		{Ref: "HEAD", ServerSideMergeOnly: true},
		{Ref: "refs/heads/release/*"},
	}, rulesView(fake.rules), "re-adding HEAD raised its level in place")

	// The regression the CLI review caught: an add that only names the branch
	// must not lower it. Lowering takes the flag set to false.
	_, err = execRepoProtection(t, "add", testProtectionPath, "HEAD")
	require.NoError(t, err)
	require.Len(t, fake.patches, 3)
	assert.False(t, fake.patches[2].AddRules[0].ServerSideMergeOnly.IsSet())
	assert.True(t, rulesView(fake.rules)[0].ServerSideMergeOnly, "HEAD stays merge-only")

	out, err = execRepoProtection(t, "add", testProtectionPath, "HEAD", "--server-side-merge-only=false")
	require.NoError(t, err)
	require.Len(t, fake.patches, 4)
	assert.Equal(t, coreapi.NewOptBool(false), fake.patches[3].AddRules[0].ServerSideMergeOnly, "an explicit false is sent")
	assert.False(t, rulesView(fake.rules)[0].ServerSideMergeOnly, "and lowers HEAD")
	assert.NotContains(t, out, protectionLevelMergeOnly)

	out, err = execRepoProtection(t, "remove", testProtectionPath, "HEAD")
	require.NoError(t, err)
	require.Len(t, fake.patches, 5)
	assert.Equal(t, []string{"HEAD"}, fake.patches[4].RemoveRefs)
	assert.Empty(t, fake.patches[4].AddRules)
	assert.NotContains(t, out, "HEAD")
	assert.Contains(t, out, "refs/heads/release/*")

	out, err = execRepoProtection(t, "remove", testProtectionPath, "refs/heads/release/*")
	require.NoError(t, err)
	assert.Equal(t, protectionEmpty+"\n", out)
}
