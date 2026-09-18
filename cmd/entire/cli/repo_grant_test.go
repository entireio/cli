package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// The project and repo the native lookups below resolve to.
const (
	repoGrantProjULID = "01HZX7QABCDEFGHJKMNPQRSTAA"
	repoGrantRepoULID = "01HZX7QABCDEFGHJKMNPQRSTAB"
)

// grantActiveCoreServer serves what `repo grant` asks the active context's
// core: the project and repo by-name lookups behind a /et/<project>/<repo>
// ref and that repo's grants, plus the placement lookup a mirror ref needs
// before its collaborators can be read on the cluster that holds it.
// placements is what that lookup returns, so a test can make a repo mirrored
// anywhere or nowhere. Every request path is recorded, so a test can assert
// that a command which should not reach the control plane made no call at all.
func grantActiveCoreServer(t *testing.T, paths *[]string, placements ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		var payload any
		switch {
		case strings.HasSuffix(r.URL.Path, "/mirrors/placements"):
			resolved := make([]coreapi.ResolvedPlacement, 0, len(placements))
			for i, host := range placements {
				resolved = append(resolved, coreapi.ResolvedPlacement{ClusterHost: host, MirrorId: fmt.Sprintf("01MIRROR%d", i)})
			}
			payload = &coreapi.ResolvePlacementsOutputBody{Placements: resolved}
		case strings.HasSuffix(r.URL.Path, "/grants"):
			payload = &coreapi.ListRepoGrantsOutputBody{Grants: []coreapi.RepoGrant{{
				GranteeId: "01ACCT", GranteeName: coreapi.NewOptString("github:alice"),
				GranteeType: granteeTypeAccount, Role: "writer", Source: "repo",
			}}}
		case strings.HasSuffix(r.URL.Path, "/repos"):
			payload = &coreapi.ListProjectReposOutputBody{Repo: coreapi.NewOptRepo(coreapi.Repo{ID: repoGrantRepoULID, Name: "web"})}
		case strings.HasSuffix(r.URL.Path, "/projects"):
			payload = &coreapi.ListProjectsOutputBody{Project: coreapi.NewOptProject(coreapi.Project{ID: repoGrantProjULID, Name: "acme", OwnerId: "01HZX7QABCDEFGHJKMNPQRSTAC", OwnerType: coreapi.ProjectOwnerTypeOrg})}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		writeJSONResponse(t, w, http.StatusOK, payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRepoGrantList_AnswersBothForges pins the verb's grammar: one question,
// two sources. A native ref lists the repo's grants from the control plane; a
// mirror ref lists its upstream collaborators, read on a cluster the repo is
// mirrored on.
//
// Not parallel: swaps the package-level core-client seams.
func TestRepoGrantList_AnswersBothForges(t *testing.T) {
	var clusterHosts []string
	mirror := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/mirrors/collaborators") {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		clusterHosts = append(clusterHosts, r.URL.Query().Get("clusterHost"))
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListMirrorCollaboratorsOutputBody{
			Collaborators: []coreapi.MirrorCollaborator{{Handle: coreapi.NewOptString("github:alice"), Role: "reader", AccountId: "01ACCT"}},
		})
	})
	seamClusterCoreClient(t, mirror)
	var nativePaths []string
	srv := grantActiveCoreServer(t, &nativePaths, defaultClusterHost)

	t.Run("a native ref lists the repo's grants", func(t *testing.T) {
		stdout, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/et/acme/web")
		require.NoError(t, err)
		require.Contains(t, stdout, "GRANTEE")
		require.Contains(t, stdout, "github:alice")
		require.Contains(t, stdout, "writer")
		require.Contains(t, nativePaths, "/api/v1/repos/"+repoGrantRepoULID+"/grants")
		require.Empty(t, clusterHosts, "a native repo is not a placement")
	})

	t.Run("a mirror ref lists the upstream collaborators", func(t *testing.T) {
		clusterHosts = nil
		stdout, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget")
		require.NoError(t, err)
		require.Contains(t, stdout, "GRANTEE")
		require.Contains(t, stdout, "github:alice")
		require.Equal(t, []string{defaultClusterHost}, clusterHosts,
			"the repo's placement, which the caller never names")
	})

	t.Run("no region flag to pick with", func(t *testing.T) {
		_, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget", "--cluster", "eu.example")
		require.ErrorContains(t, err, "unknown flag: --cluster")
	})
}

// TestRepoGrantWrite_RefusesAMirrorRef pins that the write verbs' accepted
// grammar stays the native path alone: a mirror's access is the upstream
// GitHub repository's, so it is refused before any request, and the refusal
// names the read path that does answer for a mirror.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRepoGrantWrite_RefusesAMirrorRef(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"add", []string{"add", "/gh/acme/widget", "github:alice", "--role", "reader"}},
		// The ref decides before the flags do: a missing --role must not be
		// what the user is sent to fix on a repo this verb cannot write.
		{"add without a role", []string{"add", "/gh/acme/widget", "github:alice"}},
		{"remove", []string{"remove", "/gh/acme/widget", "github:alice"}},
		// A github.com URL is not the accepted spelling, but it says which
		// repository the user means — answering it with the native path's
		// grammar would send them to rewrite it as the one shape that
		// repository can never have.
		{"a GitHub URL", []string{"add", "https://github.com/acme/widget", "github:alice", "--role", "reader"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			srv := grantActiveCoreServer(t, &paths)
			_, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, tc.args...)
			require.ErrorContains(t, err, "is a GitHub mirror")
			require.ErrorContains(t, err, "manage collaborators on GitHub")
			require.ErrorContains(t, err, "entire repo grant list /gh/acme/widget")
			require.ErrorContains(t, err, "shows who has access to the repo")
			require.Empty(t, paths, "a ref the verb cannot act on costs no round trip")
		})
	}
}

// TestMirrorReadCluster pins the choice among several placements, which is the
// only reason this function exists: the default cluster wherever it appears in
// the list, otherwise the first host in sorted order, and never a host this
// command would refuse to dial. "" means the placements named nothing usable,
// which leaves the caller on the default.
func TestMirrorReadCluster(t *testing.T) {
	t.Parallel()
	placements := func(hosts ...string) []coreapi.ResolvedPlacement {
		out := make([]coreapi.ResolvedPlacement, 0, len(hosts))
		for i, h := range hosts {
			out = append(out, coreapi.ResolvedPlacement{ClusterHost: h, MirrorId: fmt.Sprintf("01MIRROR%d", i)})
		}
		return out
	}
	for _, tc := range []struct {
		name string
		in   []coreapi.ResolvedPlacement
		want string
	}{
		{"none", nil, ""},
		{"one", placements("eu.example"), "eu.example"},
		{"the default wins from anywhere in the list", placements("aa.example", defaultClusterHost, "zz.example"), defaultClusterHost},
		{"without the default, sorted first", placements("zz.example", "aa.example", "mm.example"), "aa.example"},
		{"case folds when matching the default", placements("aa.example", strings.ToUpper(defaultClusterHost)), defaultClusterHost},
		{"an undialable host loses to a usable one", placements("https://aa.example/x", "mm.example"), "mm.example"},
		{"the default still wins past an undialable host", placements("https://aa.example/x", defaultClusterHost), defaultClusterHost},
		{"all undialable", placements("https://aa.example/x", "mm.example:not-a-port"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, mirrorReadCluster(tc.in))
		})
	}
}

// TestRepoGrantList_PlacementLookupIsAHint pins that the placement lookup can
// never veto the read. It is pull-gated; the collaborator endpoint runs a live
// GitHub-admin check — so a caller who cannot pull the mirror, but can be asked
// about its upstream, is still answered, on the default cluster.
//
// Not parallel: swaps the package-level core-client seams.
func TestRepoGrantList_PlacementLookupIsAHint(t *testing.T) {
	var clusterHosts []string
	seamClusterCoreClient(t, newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
		clusterHosts = append(clusterHosts, r.URL.Query().Get("clusterHost"))
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListMirrorCollaboratorsOutputBody{
			Collaborators: []coreapi.MirrorCollaborator{{Handle: coreapi.NewOptString("github:alice"), Role: "reader", AccountId: "01ACCT"}},
		})
	}))
	readsTheDefault := func(t *testing.T, srv *httptest.Server) {
		t.Helper()
		clusterHosts = nil
		stdout, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget")
		require.NoError(t, err)
		require.Contains(t, stdout, "github:alice")
		require.Equal(t, []string{defaultClusterHost}, clusterHosts)
	}

	t.Run("a lookup that resolves nothing", func(t *testing.T) {
		var paths []string
		readsTheDefault(t, grantActiveCoreServer(t, &paths))
	})

	t.Run("a placement naming no dialable host", func(t *testing.T) {
		var paths []string
		readsTheDefault(t, grantActiveCoreServer(t, &paths, "https://eu.example/mirrors"))
	})

	t.Run("a lookup that fails outright", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeCoreProblem(t, w, http.StatusInternalServerError, "placements unavailable")
		}))
		t.Cleanup(srv.Close)
		readsTheDefault(t, srv)
	})
}

// TestRepoGrantList_JSONNamesEachValueOnce pins the machine-readable contract:
// one verb, one answer. A mirror row reads in the grant vocabulary, carries
// nothing twice, and keeps out the one native key it cannot honestly fill.
//
// Not parallel: swaps the package-level core-client seams.
func TestRepoGrantList_JSONNamesEachValueOnce(t *testing.T) {
	seamClusterCoreClient(t, newMirrorRequestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListMirrorCollaboratorsOutputBody{
			Collaborators: []coreapi.MirrorCollaborator{
				{Handle: coreapi.NewOptString("github:toothbrush"), Role: "writer", AccountId: "01ACCTTOOTH"},
				{Role: "reader", AccountId: "01ACCTCAROL"}, // no handle resolved
			},
		})
	}))
	var paths []string
	srv := grantActiveCoreServer(t, &paths, defaultClusterHost)

	decode := func(t *testing.T, ref string) []map[string]any {
		t.Helper()
		stdout, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", ref, "--json")
		require.NoError(t, err)
		var rows []map[string]any
		require.NoError(t, json.Unmarshal([]byte(stdout), &rows))
		return rows
	}

	native := decode(t, "/et/acme/web")
	require.Equal(t, map[string]any{
		"granteeId":   "01ACCT",
		"granteeName": "github:alice",
		"granteeType": granteeTypeAccount,
		"role":        "writer",
		"source":      "repo",
	}, native[0], "the server's own model, untouched")

	mirror := decode(t, "/gh/acme/widget")
	require.Equal(t, map[string]any{
		"granteeId":   "01ACCTTOOTH",
		"granteeName": "github:toothbrush",
		"role":        "writer",
		"source":      repoProviderGitHub,
	}, mirror[0])

	// A name the server did not resolve is absent, as it is on a native row,
	// rather than filled with the id the row is already keyed by.
	require.Equal(t, map[string]any{
		"granteeId": "01ACCTCAROL",
		"role":      "reader",
		"source":    repoProviderGitHub,
	}, mirror[1])
}

// TestRepoGrantList_GuessedClusterSaysSo pins that a cluster nothing pointed at
// never passes for one that did — on a failed read, on an empty one, and with
// the reason the guess happened, since "nothing resolved" and "nothing dialable"
// send the reader to different places.
//
// Not parallel: swaps the package-level core-client seams.
func TestRepoGrantList_GuessedClusterSaysSo(t *testing.T) {
	var collaborators []coreapi.MirrorCollaborator
	var status int
	seamClusterCoreClient(t, newMirrorRequestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			writeCoreProblem(t, w, status, "mirror not found on this cluster")
			return
		}
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListMirrorCollaboratorsOutputBody{Collaborators: collaborators})
	}))

	t.Run("a failed read owns the guess", func(t *testing.T) {
		status = http.StatusNotFound
		var paths []string
		srv := grantActiveCoreServer(t, &paths) // nothing resolved
		_, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget")
		require.ErrorContains(t, err, "asked "+defaultClusterHost)
		require.ErrorContains(t, err, "no placement of acme/widget is visible to this login")
		require.ErrorContains(t, err, "that cluster was a guess")
	})

	t.Run("a login the guessed cluster refuses owns it too", func(t *testing.T) {
		status = http.StatusForbidden
		var paths []string
		srv := grantActiveCoreServer(t, &paths)
		_, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget")
		require.ErrorContains(t, err, "that cluster was a guess",
			"a guessed cluster explains any failure, not only a not-found")
	})

	t.Run("an empty read is not reported as an answer", func(t *testing.T) {
		status, collaborators = http.StatusOK, nil
		var paths []string
		srv := grantActiveCoreServer(t, &paths)
		stdout, stderr, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget")
		require.NoError(t, err)
		require.Contains(t, stdout, "No collaborators reported by "+defaultClusterHost)
		require.NotContains(t, stdout, "its access follows the upstream GitHub repository",
			"that sentence answers for a cluster a placement chose")
		require.Contains(t, stderr, "that cluster was a guess")
	})

	t.Run("--json says so too, on stderr", func(t *testing.T) {
		status, collaborators = http.StatusOK, nil
		var paths []string
		srv := grantActiveCoreServer(t, &paths)
		stdout, stderr, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget", "--json")
		require.NoError(t, err)
		require.Equal(t, "[]\n", stdout, "stdout carries rows and nothing else")
		require.Contains(t, stderr, "that cluster was a guess",
			"an empty array on a guessed cluster reads exactly like a mirror with no collaborators")
	})

	t.Run("placements that resolved say so, and not the opposite", func(t *testing.T) {
		status = http.StatusNotFound
		var paths []string
		srv := grantActiveCoreServer(t, &paths, "https://eu.example/mirrors") // resolved, undialable
		_, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget")
		require.ErrorContains(t, err, "no placement named a cluster host this command can dial")
		require.NotContains(t, err.Error(), "is visible to this login",
			"placements resolved; only their hosts were unusable")
	})

	t.Run("a cluster a placement chose is never called a guess", func(t *testing.T) {
		status, collaborators = http.StatusOK, nil
		var paths []string
		srv := grantActiveCoreServer(t, &paths, defaultClusterHost)
		stdout, stderr, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget")
		require.NoError(t, err)
		require.Contains(t, stdout, "its access follows the upstream GitHub repository")
		require.NotContains(t, stdout, "guess")
		require.NotContains(t, stderr, "guess")
	})

	t.Run("each reason points where it can be answered", func(t *testing.T) {
		status = http.StatusNotFound
		var paths []string
		invisible := grantActiveCoreServer(t, &paths) // nothing the login can see
		_, _, err := runCoreCmd(t, newRepoGrantCmd, invisible.URL, "list", "/gh/acme/widget")
		require.ErrorContains(t, err, "is visible to this login")
		require.ErrorContains(t, err, "--context acts as one",
			"`repo mirror get` reads a narrower directory, so it cannot answer what the placements lookup could not")
		require.NotContains(t, err.Error(), "entire repo mirror get")

		var dialPaths []string
		undialable := grantActiveCoreServer(t, &dialPaths, "https://eu.example/mirrors")
		_, _, err = runCoreCmd(t, newRepoGrantCmd, undialable.URL, "list", "/gh/acme/widget")
		require.ErrorContains(t, err, "entire repo mirror get /gh/acme/widget",
			"placements resolved, so the verb that lists them can answer")
	})
}

// TestRepoGrantList_ReadsAPlacementTheRepoHas pins that the region asked is one
// the repo is actually mirrored on, not a fixed default: every placement
// materializes the same upstream collaborators, so the caller names none.
//
// Not parallel: swaps the package-level core-client seams.
func TestRepoGrantList_ReadsAPlacementTheRepoHas(t *testing.T) {
	var clusterHosts []string
	seamClusterCoreClient(t, newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
		clusterHosts = append(clusterHosts, r.URL.Query().Get("clusterHost"))
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListMirrorCollaboratorsOutputBody{})
	}))
	var paths []string
	srv := grantActiveCoreServer(t, &paths, "eu.example")
	_, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget")
	require.NoError(t, err)
	require.Equal(t, []string{"eu.example"}, clusterHosts)
}

// TestRepoGrant_RefErrorsMatchTheAcceptedGrammar pins that each verb's ref
// error names the refs that verb takes: `list` takes both forges, so a bare
// pair is offered both readings, while `add` names the native path alone.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRepoGrant_RefErrorsMatchTheAcceptedGrammar(t *testing.T) {
	var paths []string
	srv := grantActiveCoreServer(t, &paths)
	run := func(args ...string) error {
		_, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, args...)
		return err
	}

	err := run("list", "acme/widget")
	require.ErrorContains(t, err, "must name its forge")
	require.ErrorContains(t, err, "/et/acme/widget")
	require.ErrorContains(t, err, "/gh/acme/widget")

	err = run("list", "https://github.com/acme/widget.git")
	require.ErrorContains(t, err, "pass GitHub repositories as /gh/acme/widget")

	err = run("add", "acme/widget", "github:alice", "--role", "reader")
	require.ErrorContains(t, err, "must be a /et/<project>/<repo> path")

	require.Empty(t, paths, "a ref that names no repo costs no round trip")
}

func TestMirrorCollaboratorRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   coreapi.MirrorCollaborator
		want []string
	}{
		{
			name: "resolved handle",
			in:   coreapi.MirrorCollaborator{AccountId: "01ACCT", Handle: coreapi.NewOptString("github:alice"), Role: "writer"},
			want: []string{"github:alice", "writer"},
		},
		{
			name: "no handle falls back to the account",
			in:   coreapi.MirrorCollaborator{AccountId: "01ACCT", Role: "reader"},
			want: []string{"01ACCT", "reader"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, mirrorCollaboratorRow(tt.in))
			require.Len(t, tt.want, len(mirrorCollaboratorColumns))
		})
	}
}
