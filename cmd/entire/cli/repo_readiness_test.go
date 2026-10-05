package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/entireio/cli/internal/coreapi"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Not parallel: runCoreCmd replaces the process-global client seam.
// serveRepoView answers the four reads `repo view` makes for a native repo:
// the plain repo read, the cluster catalog, the native-mirror list, and the
// authoritative repo read that fills the primary's STATUS. repoJSON is the
// body for both repo reads; authoritative, when non-nil, overrides the status
// and body of the authoritative one so a readiness failure can be simulated
// without breaking the plain read.
func serveRepoView(t *testing.T, repoJSON string, authoritative func(w http.ResponseWriter) bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var authReads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// A /et/<project>/<repo> ref resolves through repos/resolve, so this
		// harness serves the path spelling as well as the ULID one. The
		// resolution carries the server's own full name, which is what names a
		// repo whose own path has not been minted yet.
		case r.URL.Path == "/api/v1/repos/resolve":
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, printJSON(w, nativeResolution("acme/web", testDeleteULID)))
		case strings.HasSuffix(r.URL.Path, "/native-mirrors"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"nativeMirrors":[]}`)
		case r.URL.Path == "/api/v1/clusters":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"clusters":[]}`)
		case r.URL.Query().Get("authoritative") == "true":
			authReads.Add(1)
			if authoritative != nil && authoritative(w) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, repoJSON)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, repoJSON)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &authReads
}

// testNativeRepoPath is the one spelling `repo view` takes for a native repo.
// serveRepoView answers POST /repos/resolve for it with testDeleteULID.
const testNativeRepoPath = "/et/acme/web"

// TestRepoView_AuthoritativeSnapshot pins that `repo view` always reads the
// repo authoritatively, so the primary's STATUS says whether it is usable
// rather than dashing. The provision reason rides along in --json, which is
// the field that says why a repo stopped where it did; the human view shows it
// only where it has a failure to explain.
//
// Not parallel: runCoreCmd replaces the shared client constructor.
func TestRepoView_AuthoritativeSnapshot(t *testing.T) {
	for _, state := range []string{"provisioning", "active", "failed", "", "future"} {
		t.Run(state, func(t *testing.T) {
			body := fmt.Sprintf(`{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","path":"/et/acme/web","state":%q,"provisionReason":"max retries exhausted","capabilities":{"canManage":false,"canPush":false,"canPull":true}}`, testDeleteULID, testProjectULID, state)
			srv, authReads := serveRepoView(t, body, nil)

			// The reason is scoped to the state it explains. provisionReason
			// outlives the failure it describes, so a repo reading `ready` used
			// to carry "max retries exhausted" underneath it. This fixture has
			// no clusterSlug, so a failed repo has no placement and reports on
			// stdout, where there is no table to keep clean; every other state
			// reports nothing at all.
			out, stderr, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath)
			require.NoError(t, err)
			if state == repoStateFailed {
				require.Contains(t, out, "Provisioning failed: max retries exhausted")
			} else {
				require.NotContains(t, out, "max retries exhausted")
				require.NotContains(t, stderr, "max retries exhausted",
					"a reason without a failure to explain is noise under a healthy repo")
			}

			out, _, err = runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath, "--json")
			require.NoError(t, err)
			var row repoDirRow
			require.NoError(t, json.Unmarshal([]byte(out), &row))
			require.Equal(t, "max retries exhausted", row.ProvisionReason)
			require.Equal(t, testDeleteULID, row.ID)

			require.EqualValues(t, 2, authReads.Load(), "one authoritative snapshot per invocation")
		})
	}
}

// TestRepoView_ReasonFollowsTheAuthoritativeState pins that the why comes from
// the same read as the state it explains. The registry copy can carry a reason
// from an earlier attempt, or none for a failure it has not caught up with, and
// either way the STATUS cell would be left with nothing that accounts for it.
//
// Not parallel: runCoreCmd replaces the shared client constructor.
func TestRepoView_ReasonFollowsTheAuthoritativeState(t *testing.T) {
	repoJSON := func(state, reason string) string {
		return fmt.Sprintf(`{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","path":"/et/acme/web","state":%q,"provisionReason":%q}`,
			testDeleteULID, testProjectULID, state, reason)
	}
	for _, tc := range []struct {
		name         string
		plain, fresh string
		wantReason   string
	}{
		{
			name:       "a failure the registry has not caught up with",
			plain:      repoJSON("provisioning", ""),
			fresh:      repoJSON("failed", "cluster quota exceeded"),
			wantReason: "cluster quota exceeded",
		},
		{
			name:       "a reason left over from an earlier attempt",
			plain:      repoJSON("failed", "max retries exhausted"),
			fresh:      repoJSON("active", ""),
			wantReason: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serveRepoView(t, tc.plain, func(w http.ResponseWriter) bool {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.fresh)
				return true
			})

			out, stderr, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath)
			require.NoError(t, err)
			jsonOut, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath, "--json")
			require.NoError(t, err)
			var row repoDirRow
			require.NoError(t, json.Unmarshal([]byte(jsonOut), &row))

			require.Equal(t, tc.wantReason, row.ProvisionReason)
			if tc.wantReason == "" {
				require.Empty(t, stderr)
				return
			}
			// The fresh state here is "failed" and the fixture has no
			// clusterSlug, so there is no table and the reason leads on stdout.
			require.Contains(t, out, tc.wantReason)
		})
	}
}

// TestRepoView_BeforeThePrimaryIsPlaced pins the read that lands in the seconds
// between `repo create` returning clone coordinates and the registry carrying
// them — the window waitForRepoClonable polls through, where clusterSlug and
// path are both absent.
//
// A native repo always has exactly one primary, so neither of the two lines
// this view prints may suggest otherwise: the repo is still named by its path,
// and the empty table says the READ was early rather than that the repo is
// mirrored nowhere.
//
// Both names come from the server — the repo's own path when it has one, and
// otherwise the full name the resolution carried. The path the user typed is
// never promoted to a canonical one the server has not confirmed (COR-1892).
//
// Not parallel: runCoreCmd replaces the shared client constructor.
func TestRepoView_BeforeThePrimaryIsPlaced(t *testing.T) {
	// No clusterSlug, no path: only what the schema makes required, plus the
	// state. Both fields are optional on the wire for exactly this reason.
	body := fmt.Sprintf(`{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","state":"provisioning","visibility":"private"}`,
		testDeleteULID, testProjectULID)

	t.Run("the resolution's full name stands in until the repo has a path", func(t *testing.T) {
		srv, _ := serveRepoView(t, body, nil)
		out, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, "/et/acme/web")
		require.NoError(t, err)
		requireOrder(t, out, "Name:", "/et/acme/web", "Visibility:", "Private")
		require.Contains(t, out, "No cluster holds this repository yet.")
		require.NotContains(t, out, "Not mirrored on any cluster",
			"a repo Entire holds a record for is not a GitHub upstream")
	})

	t.Run("a ULID is not a repository's name, so it is refused", func(t *testing.T) {
		srv, _ := serveRepoView(t, body, nil)
		_, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testDeleteULID)
		require.Error(t, err, "a repo is named /et/<project>/<repo> here and no other way")
		require.ErrorContains(t, err, nativeCloneForge, "the refusal names the grammar that works")
	})

	t.Run("--json carries the project the resolved name spells", func(t *testing.T) {
		srv, _ := serveRepoView(t, body, nil)
		out, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, "/et/acme/web", "--json")
		require.NoError(t, err)
		var row repoDirRow
		require.NoError(t, json.Unmarshal([]byte(out), &row))
		require.Equal(t, "/et/acme/web", row.Repo)
		require.Equal(t, "acme", row.Project, "the resolved full name is the only place the project NAME appears")
		require.Empty(t, row.Placements)
	})
}

// TestRepoView_UnplacedRepoStatesItsLifecycle pins the two answers an empty
// placements table can have, and that they are told apart by the repo's own
// lifecycle rather than by the mere existence of a record.
//
// A failed repo has no placement either, so explaining that away as a read that
// arrived early sends the reader back to wait for something never coming — and
// `repo create`'s own recovery hint sends them to this very command.
//
// Not parallel: runCoreCmd replaces the shared client constructor.
func TestRepoView_UnplacedRepoStatesItsLifecycle(t *testing.T) {
	body := func(state, extra string) string {
		return fmt.Sprintf(`{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","state":%q%s}`,
			testDeleteULID, testProjectULID, state, extra)
	}

	t.Run("a failed repo says so, with the reason, not that the read was early", func(t *testing.T) {
		srv, _ := serveRepoView(t, body("failed", `,"provisionReason":"cluster quota exceeded"`), nil)
		out, stderr, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath)
		require.NoError(t, err)
		// Lead with the problem, not with an explanation of the missing table.
		require.Contains(t, out, "Provisioning failed: cluster quota exceeded.")
		require.NotContains(t, out, "No cluster holds this repository yet.",
			"the lifecycle already answered; this is a failure, not a wait")
		require.Empty(t, stderr, "with no table to keep clean, the reason leads on stdout")
	})

	t.Run("a provisioning repo is still the early read", func(t *testing.T) {
		srv, _ := serveRepoView(t, body("provisioning", ""), nil)
		out, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath)
		require.NoError(t, err)
		require.Contains(t, out, "No cluster holds this repository yet.")
	})

	// Visibility is a security assertion, so the absent case is not the
	// permissive one: an operator reads this to confirm a repo is restricted.
	t.Run("an unstated visibility is dashed, never rendered Public", func(t *testing.T) {
		srv, _ := serveRepoView(t, body("provisioning", ""), nil)
		out, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath)
		require.NoError(t, err)
		requireOrder(t, out, "Visibility:", "-")
		require.NotContains(t, out, "Public")
	})

	t.Run("--json omits private when unstated and carries the raw state", func(t *testing.T) {
		srv, _ := serveRepoView(t, body("failed", `,"provisionReason":"cluster quota exceeded"`), nil)
		out, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath, "--json")
		require.NoError(t, err)
		require.NotContains(t, out, `"private"`, "an absent visibility is absent, not false")
		var row repoDirRow
		require.NoError(t, json.Unmarshal([]byte(out), &row))
		require.Nil(t, row.Private)
		require.Equal(t, "failed", row.State, "the server's own lifecycle word survives the placement shape")
	})
}

// TestRepoView_AnInterruptedReadinessReadIsNotSwallowed pins the arm that tells
// a cancelled command apart from a server that cannot answer.
//
// The authoritative read is best-effort, so a server failure costs a dashed
// STATUS and the table still prints. A context error is not that: the command
// was STOPPED. Without the distinction it fell through every case, printed a
// table built on the plain read's stale state, and exited 0 — success reported
// for work the user interrupted.
//
// The cancellation tests elsewhere in this file all exercise awaitRepoActive,
// the create/poll path; this is the view's own read, which had none.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoView_AnInterruptedReadinessReadIsNotSwallowed(t *testing.T) {
	body := fmt.Sprintf(`{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","path":"/et/acme/web","clusterSlug":"us","state":"active","visibility":"private"}`,
		testDeleteULID, testProjectULID)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/native-mirrors"):
			fmt.Fprint(w, `{"nativeMirrors":[]}`)
		case r.URL.Path == "/api/v1/clusters":
			fmt.Fprint(w, `{"clusters":[]}`)
		case r.URL.Path == "/api/v1/repos/resolve":
			assert.NoError(t, printJSON(w, nativeResolution("acme/web", testDeleteULID)))
		case r.URL.Query().Get("authoritative") == "true":
			// Ctrl-C lands while this read is in flight. Block until the
			// cancellation actually reaches the transport, so the client sees a
			// context error rather than racing a written body.
			cancel()
			<-r.Context().Done()
		default:
			fmt.Fprint(w, body)
		}
	}))
	t.Cleanup(srv.Close)
	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })

	cmd := newRepoViewCmd()
	var out, errW bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errW)
	cmd.SetArgs([]string{testNativeRepoPath})
	err := cmd.ExecuteContext(ctx)

	require.Error(t, err, "an interrupted command must not report success")
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, out.String(), "CLUSTER",
		"no table may be built on the state the interrupted read failed to refresh")
}

// TestRepoView_JSONIsTheRecordPlusTheView pins the shape `repo view --json`
// answers with for a native repo: the repo as the SERVER describes it, plus the
// keys this view computed. Replacing the record with a hand-built row turned
// `.capabilities.canPush` into null at exit 0 — falsy to jq, on the question
// asked before attempting a push.
//
// The computed keys are the ones a GitHub upstream also carries, so the common
// core parses the same for either forge. `placements` is omitted when empty for
// that reason: a repo nothing holds and a GitHub candidate are the same answer,
// and must not differ by which path built the JSON.
//
// Not parallel: runCoreCmd replaces the shared client constructor.
func TestRepoView_JSONIsTheRecordPlusTheView(t *testing.T) {
	t.Run("the record's own fields survive", func(t *testing.T) {
		body := fmt.Sprintf(`{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","path":"/et/acme/web","clusterSlug":"us","state":"active","visibility":"private","capabilities":{"canManage":true,"canPush":true,"canPull":false}}`,
			testDeleteULID, testProjectULID)
		srv, _ := serveRepoView(t, body, nil)
		out, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath, "--json")
		require.NoError(t, err)

		var got map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		// The four the spec guarantees besides id, which the row shape dropped.
		require.Equal(t, testProjectULID, got["owningProjectId"], "a ULID, where .project carries the mutable name")
		require.Equal(t, "entire", got["provider"])
		require.Equal(t, "web", got["name"])
		require.Equal(t, map[string]any{"canManage": true, "canPush": true, "canPull": false}, got["capabilities"],
			"the permissions answer nothing else in this command gives")
		// And the view's own keys alongside, under the spellings mirror list uses.
		require.Equal(t, "/et/acme/web", got["repo"])
		require.Equal(t, true, got["private"])
		require.Equal(t, "/et/acme/web", got["path"], "the server's key too, so create and view agree")
	})

	// The record carries a `placements` list of its own, in the server's shape
	// (id, cell, mirror, apiBaseUrl). One key cannot hold two shapes, so the
	// view's rows replace it — and when the view has none, the record's list is
	// dropped rather than surfacing under the same name in a different shape.
	t.Run("the record's own placements never surface in the server's shape", func(t *testing.T) {
		serverList := `,"placements":[{"id":"01PLACEMENT","cell":"cell-a","clusterSlug":"us","jurisdiction":"us","mirror":false,"status":"ready"}]`
		body := fmt.Sprintf(`{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","path":"/et/acme/web","clusterSlug":"us","state":"active","visibility":"private"%s}`,
			testDeleteULID, testProjectULID, serverList)
		srv, _ := serveRepoView(t, body, nil)
		out, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath, "--json")
		require.NoError(t, err)

		var got map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		list, ok := got["placements"].([]any)
		require.True(t, ok, "the view's own list must be there")
		require.Len(t, list, 1)
		first, isObject := list[0].(map[string]any)
		require.True(t, isObject, "each placement is an object")
		require.Contains(t, first, "role", "the view's shape, which the record's list has no notion of")
		require.Contains(t, first, "cluster", "and its host-named cluster key")
		require.NotContains(t, first, "cell", "the record's shape must not leak through")
		require.NotContains(t, first, "mirror", "nor its boolean for what the view spells as a role")
	})

	t.Run("the record's placements are dropped when the view has none", func(t *testing.T) {
		// No clusterSlug, so the view builds no rows — but the record still
		// carries a list, which must not be what a consumer reads.
		body := fmt.Sprintf(`{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","state":"provisioning","placements":[{"id":"01PLACEMENT","cell":"cell-a","clusterSlug":"us","jurisdiction":"us","mirror":false,"status":"ready"}]}`,
			testDeleteULID, testProjectULID)
		srv, _ := serveRepoView(t, body, nil)
		out, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath, "--json")
		require.NoError(t, err)

		var got map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		require.NotContains(t, got, "placements",
			"the key is absent for a repo nothing holds, in either forge and from either source")
	})

	t.Run("placements is omitted when nothing holds the repo", func(t *testing.T) {
		body := fmt.Sprintf(`{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","state":"provisioning"}`,
			testDeleteULID, testProjectULID)
		srv, _ := serveRepoView(t, body, nil)
		out, _, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath, "--json")
		require.NoError(t, err)

		var got map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		require.NotContains(t, got, "placements",
			"a GitHub candidate omits it, and the two must not differ by code path")
	})
}

// TestRepoView_AuthoritativeRefusesAStatelessAnswer pins the promise the flag
// makes: "Fail if the server cannot confirm provisioning state". A 200 whose
// body omits `state` is exactly that — the read SUCCEEDED and still cannot say
// whether the repo is usable — so a zero exit would report a confirmation
// nobody made. `state` is optional on the wire, and awaitRepoActive already
// refuses the same answer when `repo create` waits for readiness.
//
// Without the flag the view still renders: a dashed STATUS costs less than
// losing the table, which is the whole distinction the flag draws.
//
// Not parallel: runCoreCmd replaces the shared client constructor.
func TestRepoView_AuthoritativeRefusesAStatelessAnswer(t *testing.T) {
	// Only what the spec makes required, plus the coordinates the view renders.
	stateless := fmt.Sprintf(`{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","path":"/et/acme/web","clusterSlug":"us","visibility":"private","capabilities":{"canManage":false,"canPush":false,"canPull":true}}`,
		testDeleteULID, testProjectULID)

	srv, _ := serveRepoView(t, stateless, nil)
	_, stderr, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath, "--authoritative")
	require.Error(t, err, "the flag exists to turn an unconfirmed state into an exit code")
	require.Contains(t, err.Error()+stderr, "readiness information")

	srv, _ = serveRepoView(t, stateless, nil)
	out, stderr, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath)
	require.NoError(t, err, "without the flag an unreadable state dashes the cell, it does not sink the view")
	require.Contains(t, out, testNativeRepoPath)
	// Dashed AND disclosed. A read that succeeds without answering leaves the
	// same empty cell as one that fails, and is the harder of the two to
	// diagnose precisely because nothing visibly went wrong — so it is not the
	// one left silent.
	require.Contains(t, stderr, "no provisioning state",
		"the failing read explains its dash; the succeeding one must too")
}

// TestRepoView_ACancelledCommandDoesNotPrintAWonRace pins the half the error
// check could never cover: a cancellation that lands while the authoritative
// read is in flight, where the read still SUCCEEDS. Deciding on the read's
// error alone takes the nil arm, prints a full table and exits 0 — reporting
// work the user had already stopped. Only the context knows.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoView_ACancelledCommandDoesNotPrintAWonRace(t *testing.T) {
	body := fmt.Sprintf(`{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","path":"/et/acme/web","clusterSlug":"us","state":"active","visibility":"private"}`,
		testDeleteULID, testProjectULID)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/repos/resolve":
			assert.NoError(t, printJSON(w, nativeResolution("acme/web", testDeleteULID)))
		case strings.HasSuffix(r.URL.Path, "/native-mirrors"):
			fmt.Fprint(w, `{"nativeMirrors":[]}`)
		case r.URL.Path == "/api/v1/clusters":
			fmt.Fprint(w, `{"clusters":[]}`)
		case r.URL.Query().Get("authoritative") == "true":
			// Ctrl-C lands, and this read answers anyway — the race the error
			// check cannot see, because there is no error to inspect.
			cancel()
			fmt.Fprint(w, body)
		default:
			fmt.Fprint(w, body)
		}
	}))
	t.Cleanup(srv.Close)
	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })

	cmd := newRepoViewCmd()
	var out, errW bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errW)
	cmd.SetArgs([]string{testNativeRepoPath})
	err := cmd.ExecuteContext(ctx)

	require.Error(t, err, "an interrupted command must not report success, however the read fared")
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, out.String(), "CLUSTER", "no table for work the user stopped")
}

// TestRepoView_GitHubIgnoredFlagNote pins that the note fires on a flag the
// caller actually asked for. --authoritative=false requests exactly what the
// GitHub path does, so reporting it as ignored tells someone a flag they turned
// off was disregarded. Nothing covered this warning at either call site.
//
// Not parallel: runCoreCmd replaces the shared client constructor.
func TestRepoView_GitHubIgnoredFlagNote(t *testing.T) {
	serve := func(t *testing.T) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/clusters":
				fmt.Fprint(w, `{"clusters":[]}`)
			default:
				assert.NoError(t, printJSON(w, &coreapi.ListReposOutputBody{
					Repos: []coreapi.RepoIndexEntry{{
						ID: "01R", Name: "hello", FullName: "octocat/hello",
						Org:      coreapi.NewOptString("octocat"),
						Provider: coreapi.NewOptString(repoProviderGitHub), Visibility: "public",
						Jurisdiction: "us", Cell: "c", ClusterSlug: "us",
					}},
				}))
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("--authoritative is reported as ignored", func(t *testing.T) {
		_, stderr, err := runCoreCmd(t, newRepoViewCmd, serve(t).URL, "/gh/octocat/hello", "--authoritative")
		require.NoError(t, err)
		require.Contains(t, stderr, "--authoritative is ignored")
	})

	t.Run("--authoritative=false is not", func(t *testing.T) {
		_, stderr, err := runCoreCmd(t, newRepoViewCmd, serve(t).URL, "/gh/octocat/hello", "--authoritative=false")
		require.NoError(t, err)
		require.NotContains(t, stderr, "is ignored",
			"the caller asked for exactly the behaviour they got")
	})

	t.Run("omitted says nothing", func(t *testing.T) {
		_, stderr, err := runCoreCmd(t, newRepoViewCmd, serve(t).URL, "/gh/octocat/hello")
		require.NoError(t, err)
		require.NotContains(t, stderr, "is ignored")
	})
}

func TestRepoCreateReadinessFlags(t *testing.T) {
	// Not parallel: shared client seam.
	for _, tc := range []struct {
		name        string
		args        []string
		wantErr     bool
		wantCreates int32
	}{
		{name: "no wait", args: []string{"--no-wait"}, wantCreates: 1},
		{name: "zero", args: []string{"--wait-timeout=0"}, wantErr: true},
		{name: "negative", args: []string{"--wait-timeout=-1s"}, wantErr: true},
		{name: "invalid", args: []string{"--wait-timeout=oops"}, wantErr: true},
		{name: "no wait still validates", args: []string{"--no-wait", "--wait-timeout=0"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var creates atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("unexpected %s", r.Method)
				}
				creates.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				fmt.Fprintf(w, `{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","state":"provisioning","capabilities":{"canManage":true,"canPush":true,"canPull":true}}`, testDeleteULID, testProjectULID)
			}))
			defer srv.Close()
			args := append([]string{"web", "--project", testProjectULID, "--json"}, tc.args...)
			out, stderr, err := runCoreCmd(t, func() *cobra.Command {
				cmd := newRepoCreateCmd()
				// The real root delegates error output to main, not Cobra.
				cmd.SilenceErrors = true
				return cmd
			}, srv.URL, args...)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Contains(t, out, testDeleteULID)
				require.Contains(t, stderr, "unconfirmed")
			}
			require.Equal(t, tc.wantCreates, creates.Load())
		})
	}
}

// Not parallel: replaces the repository cadence seam.
func TestRepoCreateReadinessResults(t *testing.T) {
	prev := repoPollInterval
	repoPollInterval = time.Millisecond
	t.Cleanup(func() { repoPollInterval = prev })
	for _, tc := range []struct {
		name, initial, final string
		pollStatus, polls    int
		wantErr              bool
		foreign, mismatched  bool
	}{
		{name: "already active", initial: "active", final: "active", polls: 1},
		{name: "active creation but region still provisioning", initial: "active", final: "active", polls: 3},
		{name: "active creation but failed region", initial: "active", final: "failed", polls: 1, wantErr: true},
		{name: "active creation but unavailable region", initial: "active", pollStatus: 503, polls: 6, wantErr: true},
		{name: "active creation but foreign snapshot", initial: "active", final: "active", foreign: true, polls: 1, wantErr: true},
		{name: "active creation but missing lifecycle", initial: "active", final: "", polls: 1, wantErr: true},
		{name: "multiple pending", initial: "provisioning", final: "active", polls: 3},
		{name: "failed snapshot with restored access", initial: "provisioning", final: "failed", polls: 1, wantErr: true},
		{name: "old server missing state", initial: "", wantErr: true},
		{name: "unknown initial", initial: "future", wantErr: true},
		{name: "missing on poll", initial: "provisioning", final: "", polls: 1, wantErr: true},
		{name: "unknown on poll", initial: "provisioning", final: "future", polls: 1, wantErr: true},
		{name: "creator access removed by cleanup", initial: "provisioning", pollStatus: 403, polls: 2, wantErr: true},
		{name: "foreign registry removed by cleanup", initial: "provisioning", pollStatus: 404, polls: 2, wantErr: true},
		{name: "routing unavailable", initial: "provisioning", pollStatus: 503, polls: 6, wantErr: true},
		{name: "missing home host", initial: "provisioning", pollStatus: 500, polls: 6, wantErr: true},
		{name: "foreign on poll", initial: "provisioning", final: "active", foreign: true, polls: 1, wantErr: true},
		{name: "mismatched ID", initial: "provisioning", final: "active", mismatched: true, polls: 1, wantErr: true},
		{name: "rejected parameter", initial: "provisioning", pollStatus: 422, polls: 2, wantErr: true},
		{name: "rate limited", initial: "provisioning", pollStatus: 429, polls: 6, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, asJSON := range []bool{false, true} {
				var posts, gets atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					state := tc.initial
					switch r.Method {
					case http.MethodPost:
						posts.Add(1)
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusCreated)
						fmt.Fprintf(w, `{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","state":%q,"commitToken":"tok-abc","clusterHost":"cell.example","path":"/et/project/web","capabilities":{"canManage":true,"canPush":true,"canPull":true}}`, testDeleteULID, testProjectULID, state)
						return
					case http.MethodGet:
						n := gets.Add(1)
						if r.URL.Query().Get("authoritative") != "true" {
							t.Error("missing authoritative query")
						}
						if tc.pollStatus == http.StatusTooManyRequests {
							http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
							return
						}
						if tc.pollStatus != 0 {
							w.Header().Set("Content-Type", "application/problem+json")
							w.WriteHeader(tc.pollStatus)
							detail := map[int]string{403: "permission denied", 404: "repo not found", 503: "repository lifecycle unavailable on this core", 500: `cluster jurisdiction "eu" has no auth host wired on this core`}[tc.pollStatus]
							if tc.pollStatus == 422 {
								fmt.Fprint(w, `{"detail":"validation failed","errors":[{"message":"unknown query parameter","location":"query.authoritative"}]}`)
							} else {
								fmt.Fprintf(w, `{"status":%d,"title":%q,"detail":%q}`, tc.pollStatus, http.StatusText(tc.pollStatus), detail)
							}
							return
						}
						state = "provisioning"
						if int(n) >= tc.polls {
							state = tc.final
						}
					default:
						t.Errorf("unexpected request: %s", r.Method)
					}
					w.Header().Set("Content-Type", "application/json")
					// Real enrichment can omit remote coordinates and owning project after cleanup.
					snapshotID := testDeleteULID
					if tc.mismatched {
						snapshotID = testProjectULID
					}
					fmt.Fprintf(w, `{"id":%q,"name":"web","owningProjectId":"","provider":"entire","state":%q,"provisionReason":"max retries exhausted","foreign":%t,"capabilities":{"canManage":false,"canPush":false,"canPull":true}}`, snapshotID, state, tc.foreign)
				}))
				args := []string{"web", "--project", testProjectULID}
				if asJSON {
					args = append(args, "--json")
				}
				out, stderr, err := runCoreCmd(t, func() *cobra.Command {
					cmd := newRepoCreateCmd()
					// The real root delegates error output to main, not Cobra.
					cmd.SilenceErrors = true
					return cmd
				}, srv.URL, args...)
				srv.Close()
				if tc.wantErr {
					require.Error(t, err)
					require.Contains(t, stderr, "creation succeeded")
					require.Contains(t, stderr, "repo view /et/project/web",
						"the hint must name a ref `repo view` accepts, and a ULID is not one")
					require.Contains(t, stderr, "support")
					require.Contains(t, stderr, "--authoritative")
					if tc.pollStatus == 422 {
						require.Contains(t, stderr, "query.authoritative")
						require.Contains(t, stderr, "unknown query parameter")
						require.Contains(t, stderr, "--no-wait")
					}
					if tc.pollStatus == http.StatusForbidden {
						var statusErr *coreapi.ErrorModelStatusCode
						require.ErrorAs(t, err, &statusErr)
						var silent *SilentError
						require.ErrorAs(t, err, &silent)
						// Model main's rendering gate: removing runCoreClient's guard
						// must produce a second copy of the problem detail here.
						if !errors.As(err, &silent) {
							stderr += fmt.Sprintln(renderCoreError(err))
						}
						require.Equal(t, 1, strings.Count(stderr, "permission denied"))
					}
				} else {
					require.NoError(t, err)
				}
				require.Contains(t, out, testDeleteULID)
				require.Contains(t, out, "entire://cell.example/et/project/web")
				require.EqualValues(t, 1, posts.Load())
				require.EqualValues(t, tc.polls, gets.Load())
				if asJSON {
					var obj map[string]any
					require.NoError(t, json.Unmarshal([]byte(out), &obj))
					require.Equal(t, "tok-abc", obj["commitToken"])
					require.Equal(t, testProjectULID, obj["owningProjectId"])
					require.Equal(t, testDeleteULID, obj["id"])
					expectedState := tc.initial
					if tc.polls > 0 && tc.pollStatus == 0 && !tc.mismatched {
						expectedState = tc.final
					}
					require.Equal(t, expectedState, obj["state"])
				}
			}
		})
	}
}

type repoReadFunc func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error)

func (f repoReadFunc) GetRepo(ctx context.Context, p coreapi.GetRepoParams) (*coreapi.RepoHeaders, error) {
	return repoHeaders(f(ctx, p))
}

func TestAwaitRepoActive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		foreign bool
		state   string
	}{
		{name: "foreign registry", foreign: true},
		{name: "foreign active is not authoritative", foreign: true, state: "active"},
		{name: "unknown", state: "future"},
		{name: "missing"},
		{name: "failed", state: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString(tc.state), Foreign: coreapi.NewOptBool(tc.foreign)}
			calls := 0
			err := awaitRepoActive(t.Context(), repoReadFunc(func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error) {
				calls++
				return nil, errors.New("unexpected")
			}), result, nil)
			require.Error(t, err)
			require.Zero(t, calls)
			require.Equal(t, testDeleteULID, result.ID)
		})
	}
	t.Run("errors reset after successful read", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("provisioning")}
			calls := 0
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			err := awaitRepoActive(ctx, repoReadFunc(func(_ context.Context, p coreapi.GetRepoParams) (*coreapi.Repo, error) {
				calls++
				require.True(t, p.Authoritative.Or(false))
				if calls != 3 && calls != 6 {
					return nil, errors.New("temporary read error")
				}
				state := "provisioning"
				if calls == 6 {
					state = "active"
				}
				return &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString(state)}, nil
			}), result, nil)
			require.NoError(t, err)
			require.Equal(t, 6, calls)
			require.Equal(t, "active", result.State.Or(""))
		})
	})
	for _, inFlight := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline in flight %v", inFlight), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("provisioning")}
				calls := 0
				err := awaitRepoActive(ctx, repoReadFunc(func(ctx context.Context, _ coreapi.GetRepoParams) (*coreapi.Repo, error) {
					calls++
					if inFlight {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return result, nil
				}), result, nil)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Equal(t, "provisioning", result.State.Or(""))
				if inFlight {
					require.Equal(t, 1, calls)
				} else {
					// The lower jitter bounds allow a third probe at 4.8s.
					require.GreaterOrEqual(t, calls, 2)
					require.LessOrEqual(t, calls, 3)
				}
			})
		})
	}
	t.Run("canceled before polling", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("provisioning")}
		err := awaitRepoActive(ctx, repoReadFunc(func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error) {
			t.Error("read after cancellation")
			return nil, errors.New("unexpected read")
		}), result, nil)
		require.ErrorIs(t, err, context.Canceled)
	})
}

// Not parallel: replaces activeCoreClient. The HTTP request is canceled only
// after POST has succeeded, pinning both output preservation and error identity
// used by main's signal-exit path.
func TestRepoCreateInterruptedAfterCreation(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout %v", timeout), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var posts, gets atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					fmt.Fprintf(w, `{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","state":"provisioning","commitToken":"tok-abc","clusterHost":"cell.example","path":"/et/project/web","capabilities":{"canManage":true,"canPush":true,"canPull":true}}`, testDeleteULID, testProjectULID)
					return
				}
				gets.Add(1)
				if !timeout {
					cancel()
				}
				<-r.Context().Done()
			}))
			defer srv.Close()
			prev := activeCoreClient
			activeCoreClient = func(context.Context) (*coreapi.Client, error) { return coreapi.NewWithBearer(srv.URL, "tok") }
			t.Cleanup(func() { activeCoreClient = prev })
			cmd := newRepoCreateCmd()
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			args := []string{"web", "--project", testProjectULID, "--json"}
			if timeout {
				args = append(args, "--wait-timeout=1s")
			}
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(ctx)
			if timeout {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				require.ErrorIs(t, err, context.Canceled)
			}
			var obj map[string]any
			require.NoError(t, json.Unmarshal(out.Bytes(), &obj))
			require.Equal(t, testDeleteULID, obj["id"])
			require.Equal(t, "entire://cell.example/et/project/web", obj["remote"])
			require.Contains(t, stderr.String(), "creation succeeded")
			require.EqualValues(t, 1, posts.Load())
			require.EqualValues(t, 1, gets.Load())
		})
	}
}

func TestReportRepoCreationNoWaitReason(t *testing.T) {
	t.Parallel()
	cmd := newRepoCreateCmd()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("failed"), ProvisionReason: coreapi.NewOptString("max retries exhausted")}
	require.NoError(t, reportRepoCreation(cmd, result, true, nil))
	require.Contains(t, out.String(), "max retries exhausted")
	require.Contains(t, stderr.String(), "unconfirmed")
}

// TestReportRepoCreationWithoutAPath pins that a recovery hint never names a
// command that cannot work. `repo view` takes the /et/<project>/<repo> path and
// nothing else, and the create response has no path in exactly the window where
// readiness goes unconfirmed — so there is no ref to offer, and offering the
// bare name would hand someone already stuck a command this verb refuses.
//
// Both branches that print a hint are covered: the readiness failure, and
// --no-wait leaving a non-active state. The repository ID survives either way,
// because that is what support is asked for.
func TestReportRepoCreationWithoutAPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		noWait  bool
		waitErr error
		// A readiness failure is reported AND returned, so the exit code says
		// so; --no-wait asked not to wait, so an unconfirmed state is not an
		// error there.
		wantErr bool
	}{
		{name: "readiness failed", waitErr: errors.New("readiness unconfirmed"), wantErr: true},
		{name: "--no-wait with a non-active state", noWait: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := newRepoCreateCmd()
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			// No Path: the server has not minted clone coordinates yet.
			result := &coreapi.Repo{ID: testDeleteULID, Name: "web",
				State: coreapi.NewOptString("provisioning")}

			err := reportRepoCreation(cmd, result, tc.noWait, tc.waitErr)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			require.NotContains(t, stderr.String(), "entire repo view",
				"no path means no ref this verb accepts, so no command is offered")
			require.NotContains(t, stderr.String(), "repo view web",
				"the bare name in particular: `repo view` refuses it")
			require.Contains(t, stderr.String(), testDeleteULID,
				"the ID stays — it is what support is asked for")
		})
	}

	t.Run("with a path the hint is offered", func(t *testing.T) {
		t.Parallel()
		cmd := newRepoCreateCmd()
		var out, stderr bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&stderr)
		result := &coreapi.Repo{ID: testDeleteULID, Name: "web",
			Path: coreapi.NewOptString("/et/acme/web"), State: coreapi.NewOptString("provisioning")}

		require.Error(t, reportRepoCreation(cmd, result, false, errors.New("readiness unconfirmed")))

		require.Contains(t, stderr.String(), "entire repo view /et/acme/web",
			"the path is a ref the verb takes, so the hint is worth printing")
	})
}

func TestRepoCreateAlreadyReportedCoreError(t *testing.T) {
	t.Parallel()
	statusErr := &coreapi.ErrorModelStatusCode{StatusCode: http.StatusForbidden,
		Response: coreapi.ErrorModel{Detail: coreapi.NewOptString("permission denied")}}
	original := fmt.Errorf("command failed: %w", NewSilentError(errors.Join(statusErr, context.Canceled)))
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	err := runCoreClient(cmd, func(context.Context) (*coreapi.Client, error) { return &coreapi.Client{}, nil },
		func(context.Context, *coreapi.Client) error { return original })
	var silent *SilentError
	require.ErrorAs(t, err, &silent)
	require.ErrorIs(t, err, statusErr)
	require.ErrorIs(t, err, context.Canceled)
	// Display callers (notably the mirror wizard) still need a plain message.
	rendered := renderCoreError(original)
	require.EqualError(t, rendered, "permission denied")
	require.NotErrorAs(t, rendered, &silent)
}

// Not parallel: replaces the random-sample seam to pin the jittered schedule.
func TestAwaitRepoActiveBackoff(t *testing.T) {
	previous := repoPollRandom
	samples := []float64{0, 0.5, 1, 0, 0.5, 1}
	next := 0
	repoPollRandom = func() float64 {
		sample := samples[next%len(samples)]
		next++
		return sample
	}
	t.Cleanup(func() { repoPollRandom = previous })
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var probes []time.Duration
		result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("provisioning")}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		err := awaitRepoActive(ctx, repoReadFunc(func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error) {
			probes = append(probes, time.Since(start))
			state := "provisioning"
			if len(probes) == 7 {
				state = "active"
			}
			return &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString(state)}, nil
		}), result, nil)
		require.NoError(t, err)
		require.Zero(t, probes[0], "the first probe must be immediate")
		// Explicit values pin the production backoff and both jitter extremes,
		// independently of repoPollDelay's implementation.
		for i, want := range []time.Duration{
			1600 * time.Millisecond, 4 * time.Second, 9600 * time.Millisecond,
			12800 * time.Millisecond, 30 * time.Second, 36 * time.Second,
		} {
			require.Equal(t, want, probes[i+1]-probes[i])
		}
		require.Equal(t, 6, next, "each sleep gets a fresh jitter sample")
	})
}

func TestAwaitRepoActiveErrorBudget(t *testing.T) {
	t.Parallel()
	for _, status := range []int{403, 404, 408, 429, 500, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				calls := 0
				problem := &coreapi.ErrorModelStatusCode{StatusCode: status}
				result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("provisioning")}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
				defer cancel()
				err := awaitRepoActive(ctx, repoReadFunc(func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error) {
					calls++
					return nil, problem
				}), result, nil)
				require.ErrorIs(t, err, problem)
				if status == 403 || status == 404 {
					require.Equal(t, 2, calls)
					require.LessOrEqual(t, time.Since(start), 10*time.Second)
				} else {
					require.GreaterOrEqual(t, calls, 5)
					require.LessOrEqual(t, calls, 6)
					require.LessOrEqual(t, time.Since(start), time.Minute)
				}
			})
		})
	}
}

func TestRepoCreateMirrorReadinessFlags(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"-1s", "oops"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			for _, constructor := range []func() *cobra.Command{newRepoCreateCmd, newRepoMirrorAddCmd} {
				cmd := constructor()
				cmd.RunE = func(*cobra.Command, []string) error { t.Error("invalid timeout reached RunE"); return nil }
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&out)
				timeoutFlag := "--timeout="
				if cmd.Flags().Lookup("wait-timeout") != nil {
					timeoutFlag = "--wait-timeout="
				}
				args := []string{"foo", timeoutFlag + value}
				if cmd.Flags().Lookup("project") != nil {
					args = append(args, "--project", testProjectULID)
				}
				cmd.SetArgs(args)
				require.Error(t, cmd.ExecuteContext(t.Context()))
				require.False(t, cmd.SilenceUsage, "invalid flags are usage errors on both commands")
			}
		})
	}
}

// TestRepoViewAuthoritativeFlag pins what --authoritative now decides. The
// authoritative read happens either way — it is what makes the primary's
// STATUS mean anything — so the flag says only whether a readiness answer is
// REQUIRED: without it a failed readiness read dashes the cell and the view
// still renders; with it the command fails.
//
// hint marks the failures a plain read could still answer. Every other status
// is a statement about the repository, so retrying without the readiness check
// changes nothing and the hint must stay away.
//
// Not parallel: runCoreCmd replaces the shared client constructor.
func TestRepoViewAuthoritativeFlag(t *testing.T) {
	const repoBody = `{"id":"` + testDeleteULID + `","name":"web","owningProjectId":"` + testProjectULID + `","provider":"entire","path":"/et/acme/web","capabilities":{"canManage":false,"canPush":false,"canPull":true}}`
	for _, tc := range []struct {
		name, body string
		status     int
		hint       bool
	}{
		{name: "unavailable", status: 503, hint: true},
		{name: "rejected parameter", status: 422, hint: true,
			body: `{"detail":"repository read failed","errors":[{"message":"unknown query parameter","location":"query.authoritative"}]}`},
		{name: "unrelated validation", status: 422,
			body: `{"detail":"repository read failed","errors":[{"message":"expected a ULID","location":"path.repo_id"}]}`},
		{name: "forbidden", status: 403},
		{name: "missing", status: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fail := func(w http.ResponseWriter) bool {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(tc.status)
				if tc.body != "" {
					fmt.Fprint(w, tc.body)
				} else {
					fmt.Fprintf(w, `{"status":%d,"detail":"repository read failed"}`, tc.status)
				}
				return true
			}

			// Without the flag the view still prints — losing it costs more
			// than a dashed STATUS — but the failure is DISCLOSED. Silence let
			// a core outage downgrade every `repo view` to a table asserting
			// nothing was wrong, at exit 0.
			srv, _ := serveRepoView(t, repoBody, fail)
			out, stderr, err := runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath)
			require.NoError(t, err, "a failed readiness read must not sink the view")
			require.Contains(t, out, "/et/acme/web")
			require.Contains(t, stderr, "could not confirm provisioning state")
			// The --authoritative recovery hint stays out of the flagless path:
			// it tells the reader to drop a flag they never passed.
			require.NotContains(t, stderr, "to inspect repository details")

			srv, _ = serveRepoView(t, repoBody, fail)
			_, stderr, err = runCoreCmd(t, newRepoViewCmd, srv.URL, testNativeRepoPath, "--authoritative")
			require.Error(t, err)
			var silent *SilentError
			if !errors.As(err, &silent) {
				stderr += err.Error()
			}
			// The server's own message reaches the user either way.
			require.Contains(t, stderr, "repository read failed")
			if tc.hint {
				require.Contains(t, stderr, "entire repo view "+testNativeRepoPath+" without --authoritative")
				require.NotContains(t, stderr, "without a readiness check",
					"the read is unconditional now, so dropping the flag skips nothing — it only changes the failure from an error to a warning")
			} else {
				require.NotContains(t, stderr, "readiness check")
			}
			if tc.hint && tc.status == 422 {
				require.Contains(t, stderr, "query.authoritative")
				require.Contains(t, stderr, "unknown query parameter")
			}
			// The plain read is the default; naming a flag value would send the
			// user to restate one they never had to pass.
			require.NotContains(t, stderr, "--authoritative=false")
			require.NotContains(t, stderr, "--no-wait")
		})
	}
}

func TestAwaitRepoActiveJitterBounds(t *testing.T) {
	t.Parallel()
	// Pin both ends and the midpoint independently of the random source used
	// by production; the synctest poll test checks the actual timer schedule.
	for _, sample := range []struct {
		value float64
		want  time.Duration
	}{
		{0, 24 * time.Second}, {0.5, 30 * time.Second}, {1, 36 * time.Second},
	} {
		require.Equal(t, sample.want, repoPollDelay(30*time.Second, sample.value))
	}
}

func TestAwaitRepoActivePollingCallback(t *testing.T) {
	t.Parallel()
	for _, initial := range []string{"active", "failed", "", "future", "provisioning"} {
		t.Run(initial, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				started, reads := 0, 0
				result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString(initial)}
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				err := awaitRepoActive(ctx, repoReadFunc(func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error) {
					require.Equal(t, 1, started, "progress starts before the first read")
					reads++
					state := "provisioning"
					if reads == 2 {
						state = "active"
					}
					return &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString(state)}, nil
				}), result, func() { started++ })
				if initial == "provisioning" || initial == "active" {
					require.NoError(t, err)
					require.Equal(t, 1, started)
					require.Equal(t, 2, reads)
				} else {
					require.Zero(t, started)
					require.Zero(t, reads)
				}
				if initial == "" {
					require.ErrorContains(t, err, "the server did not return repository readiness information")
				}
			})
		})
	}
}

func TestAwaitRepoActiveErrorWindowBoundsInflight(t *testing.T) {
	t.Parallel()
	for _, status := range []int{404, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				problem := &coreapi.ErrorModelStatusCode{StatusCode: status}
				calls := 0
				result := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("provisioning")}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
				defer cancel()
				err := awaitRepoActive(ctx, repoReadFunc(func(ctx context.Context, _ coreapi.GetRepoParams) (*coreapi.Repo, error) {
					calls++
					if calls == 1 {
						return nil, problem
					}
					<-ctx.Done()
					return nil, ctx.Err()
				}), result, nil)
				require.ErrorIs(t, err, problem, "keep the observed API failure when its retry window expires")
				require.Equal(t, 2, calls)
				want := time.Minute
				if status == 404 {
					want = 10 * time.Second
				}
				require.Equal(t, want, time.Since(start))
				require.NoError(t, ctx.Err(), "error window is distinct from the command deadline")
			})
		})
	}
}

func TestAwaitRepoActiveRetainsOnlyCreationCoordinates(t *testing.T) {
	t.Parallel()
	result := &coreapi.Repo{ID: testDeleteULID, Name: "web", OwningProjectId: testProjectULID,
		State: coreapi.NewOptString("provisioning"), ProvisionReason: coreapi.NewOptString("stale"),
		ClusterHost: coreapi.NewOptString("cell.example"), Path: coreapi.NewOptString("/et/acme/web"),
		AdditionalProps: coreapi.RepoAdditional{"remote": []byte(`"entire://cell.example/et/acme/web"`)}}
	snapshot := &coreapi.Repo{ID: testDeleteULID, State: coreapi.NewOptString("active")}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	err := awaitRepoActive(ctx, repoReadFunc(func(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error) { return snapshot, nil }), result, nil)
	require.NoError(t, err)
	require.Equal(t, "web", result.Name)
	require.Equal(t, testProjectULID, result.OwningProjectId)
	require.Equal(t, "cell.example", result.ClusterHost.Or(""))
	require.Equal(t, "/et/acme/web", result.Path.Or(""))
	require.JSONEq(t, `"entire://cell.example/et/acme/web"`, string(result.AdditionalProps["remote"]))
	require.Equal(t, "active", result.State.Or(""))
	require.False(t, result.ProvisionReason.IsSet(), "do not preserve stale lifecycle enrichment")
	// Exactly the snapshot plus the creation coordinates: nothing else from
	// the creation response survives.
	require.Equal(t, coreapi.Repo{
		ID: testDeleteULID, Name: "web", OwningProjectId: testProjectULID,
		State:       coreapi.NewOptString("active"),
		ClusterHost: coreapi.NewOptString("cell.example"), Path: coreapi.NewOptString("/et/acme/web"),
		AdditionalProps: coreapi.RepoAdditional{"remote": []byte(`"entire://cell.example/et/acme/web"`)},
	}, *result)
}

func TestRepoMirrorZeroTimeout(t *testing.T) {
	t.Parallel()
	cmd := newRepoMirrorAddCmd()
	called := false
	cmd.RunE = func(*cobra.Command, []string) error { called = true; return nil }
	cmd.SetArgs([]string{"foo", "--timeout=0"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	require.True(t, called)
}

func TestRepoPollMixedErrors(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var failures repoPollFailures
		require.False(t, failures.record(&coreapi.ErrorModelStatusCode{StatusCode: 404}))
		time.Sleep(2 * time.Second)
		require.False(t, failures.record(&coreapi.ErrorModelStatusCode{StatusCode: 503}))
		time.Sleep(9 * time.Second)
		require.False(t, failures.expired(), "transient response restores the longer window")
		time.Sleep(49 * time.Second)
		require.True(t, failures.expired(), "window still starts at the first failure")
	})
}

func TestRetainRepoAdditionalProperties(t *testing.T) {
	t.Parallel()
	result := &coreapi.Repo{AdditionalProps: coreapi.RepoAdditional{
		"commitToken": []byte(`"tok-abc"`), "future": []byte(`{"version":1}`),
	}}
	snapshot := &coreapi.Repo{AdditionalProps: coreapi.RepoAdditional{
		"future": []byte(`{"version":2}`),
	}}
	retainRepoCreation(result, snapshot)
	require.JSONEq(t, `"tok-abc"`, string(result.AdditionalProps["commitToken"]))
	require.JSONEq(t, `{"version":2}`, string(result.AdditionalProps["future"]))
}

func TestRepoReadErrorUnrelatedValidation(t *testing.T) {
	t.Parallel()
	problem := &coreapi.ErrorModelStatusCode{StatusCode: 422, Response: coreapi.ErrorModel{
		Detail: coreapi.NewOptString("invalid repository ID"),
		Errors: []coreapi.ErrorDetail{{Location: coreapi.NewOptString("path.repoId"),
			Message: coreapi.NewOptString("invalid value")}},
	}}
	require.EqualError(t, renderRepoReadError(problem), "invalid repository ID")
}

func TestRepoPollTransientThenOrdinaryError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var failures repoPollFailures
		require.False(t, failures.record(&coreapi.ErrorModelStatusCode{StatusCode: 503}))
		time.Sleep(2 * time.Second)
		require.True(t, failures.record(&coreapi.ErrorModelStatusCode{StatusCode: 404}),
			"ordinary failures still enforce the two-attempt limit")
	})
}
