package cli

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

func nativeTestRepo() *coreapi.Repo {
	return &coreapi.Repo{
		ID:           "01REPO",
		Name:         "web",
		Provider:     coreapi.NewOptString(repoProviderEntire),
		State:        coreapi.NewOptString(repoStateActive),
		ClusterSlug:  coreapi.NewOptString("aws-us-east-2"),
		Jurisdiction: coreapi.NewOptString("us"),
		Path:         coreapi.NewOptString("/et/acme/web"),
	}
}

var nativeTestClusters = []coreapi.Cluster{
	{Slug: "aws-us-east-2", Jurisdiction: "us", PublicUrl: "https://aws-us-east-2.entire.io"},
	{Slug: "aws-eu-central-1", Jurisdiction: "eu", PublicUrl: "https://aws-eu-central-1.entire.io"},
}

// TestCheckNativeMirrorTarget covers every refusal the CLI makes before it
// writes anything. Each has a server-side counterpart; the point of deciding
// locally is that the message can name the repo's own primary and region.
func TestCheckNativeMirrorTarget(t *testing.T) {
	t.Parallel()

	t.Run("a cross-region target on an active native repo is accepted", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, checkNativeMirrorTarget(nativeTestRepo(), nativeTestClusters, "aws-eu-central-1", "/et/acme/web"))
	})

	t.Run("a GitHub-backed repo has no native mirrors", func(t *testing.T) {
		t.Parallel()
		repo := nativeTestRepo()
		repo.Provider = coreapi.NewOptString(repoProviderGitHub)
		err := checkNativeMirrorTarget(repo, nativeTestClusters, "aws-eu-central-1", "/et/acme/web")
		require.ErrorContains(t, err, "not an Entire-native repository")
	})

	t.Run("a repo that is still provisioning is refused", func(t *testing.T) {
		t.Parallel()
		repo := nativeTestRepo()
		repo.State = coreapi.NewOptString(repoStateProvisioning)
		err := checkNativeMirrorTarget(repo, nativeTestClusters, "aws-eu-central-1", "/et/acme/web")
		require.ErrorContains(t, err, "provisioning")
	})

	// An unset state is "the server did not say", which is the server's call to
	// make, not a reason to refuse locally.
	t.Run("an unset state is left to the server", func(t *testing.T) {
		t.Parallel()
		repo := nativeTestRepo()
		repo.State = coreapi.OptString{}
		require.NoError(t, checkNativeMirrorTarget(repo, nativeTestClusters, "aws-eu-central-1", "/et/acme/web"))
	})

	t.Run("the repo's own cluster is its primary, not a mirror of it", func(t *testing.T) {
		t.Parallel()
		err := checkNativeMirrorTarget(nativeTestRepo(), nativeTestClusters, "aws-us-east-2", "/et/acme/web")
		require.ErrorContains(t, err, "primary")
	})

	t.Run("an unknown cluster names the available ones", func(t *testing.T) {
		t.Parallel()
		err := checkNativeMirrorTarget(nativeTestRepo(), nativeTestClusters, "aws-ap-south-1", "/et/acme/web")
		require.ErrorContains(t, err, "unknown cluster")
		require.ErrorContains(t, err, "aws-eu-central-1")
	})

	// v1 places a native mirror cross-jurisdiction: the point is reach, not
	// redundancy inside one region. In production there is one cluster per
	// region, so this and the primary-cluster case coincide there — they are
	// separate rules, and only a unit test can tell them apart.
	t.Run("a same-region target is refused even when it is not the primary", func(t *testing.T) {
		t.Parallel()
		clusters := append([]coreapi.Cluster{{Slug: "aws-us-west-1", Jurisdiction: "us", PublicUrl: "https://aws-us-west-1.entire.io"}}, nativeTestClusters...)
		err := checkNativeMirrorTarget(nativeTestRepo(), clusters, "aws-us-west-1", "/et/acme/web")
		require.ErrorContains(t, err, "different one")
		require.ErrorContains(t, err, "us")
	})
}

// TestNativeMirrorIsFresh pins the only signal that separates a placement this
// command created from one it found: the create endpoint is idempotent and
// answers identically either way.
func TestNativeMirrorIsFresh(t *testing.T) {
	t.Parallel()
	fresh := coreapi.NativeMirrorPlacement{
		Stage:  coreapi.NativeMirrorPlacementStagePending,
		Status: coreapi.NativeMirrorPlacementStatusProcessing,
	}
	require.True(t, nativeMirrorIsFresh(fresh))

	retried := fresh
	retried.Attempts = 1
	require.False(t, nativeMirrorIsFresh(retried), "a retried intent already existed")

	seeded := fresh
	seeded.Stage = coreapi.NativeMirrorPlacementStageSeeded
	require.False(t, nativeMirrorIsFresh(seeded), "progress past pending means it already existed")

	ready := fresh
	ready.Status = coreapi.NativeMirrorPlacementStatusReady
	require.False(t, nativeMirrorIsFresh(ready))
}

// TestNativeRepoDetailRow pins what `mirror get /et/...` shows: the primary
// first, then every mirror, each labelled by role — because the difference
// decides what a reader can do with it.
func TestNativeRepoDetailRow(t *testing.T) {
	t.Parallel()

	t.Run("the primary leads and mirrors follow in slug order", func(t *testing.T) {
		t.Parallel()
		row := nativeRepoDetailRow("/et/acme/web", nativeTestRepo(), []coreapi.NativeMirrorPlacement{
			{ClusterSlug: "aws-eu-central-1", Status: coreapi.NativeMirrorPlacementStatusReady, Stage: coreapi.NativeMirrorPlacementStageAnnounced},
		}, nativeTestClusters)
		require.Equal(t, "/et/acme/web", row.Repo)
		require.Equal(t, []repoDirPlacement{
			{Cluster: "aws-us-east-2", Status: "active", Role: placementRolePrimary, CloneURL: "entire://aws-us-east-2.entire.io/et/acme/web"},
			{Cluster: "aws-eu-central-1", Status: "ready", Role: placementRoleNativeMirror, CloneURL: "entire://aws-eu-central-1.entire.io/et/acme/web"},
		}, row.Placements)
	})

	// Stage says how far a seed has got. It is carried only while the placement
	// is processing, because that is the only time the answer is useful and the
	// only time it is not simply "announced".
	t.Run("a seeding mirror carries its stage", func(t *testing.T) {
		t.Parallel()
		row := nativeRepoDetailRow("/et/acme/web", nativeTestRepo(), []coreapi.NativeMirrorPlacement{
			{ClusterSlug: "aws-eu-central-1", Status: coreapi.NativeMirrorPlacementStatusProcessing, Stage: coreapi.NativeMirrorPlacementStageSeeded},
		}, nativeTestClusters)
		require.Equal(t, "seeded", row.Placements[1].Stage)
		require.Equal(t, "processing", row.Placements[1].Status, "the status field stays the server's own value")
	})

	// A placement on its way out stays listed: creating on that cluster is
	// refused until it is gone, so hiding it would hide the reason.
	t.Run("a placement being torn down is marked, not hidden", func(t *testing.T) {
		t.Parallel()
		row := nativeRepoDetailRow("/et/acme/web", nativeTestRepo(), []coreapi.NativeMirrorPlacement{
			{ClusterSlug: "aws-eu-central-1", Status: coreapi.NativeMirrorPlacementStatusReady, DesiredState: coreapi.NativeMirrorPlacementDesiredStateDeleted},
		}, nativeTestClusters)
		require.Len(t, row.Placements, 2)
		require.True(t, row.Placements[1].Removing)
		require.Equal(t, "ready", row.Placements[1].Status)
	})

	t.Run("a cluster with no usable host gets no clone URL", func(t *testing.T) {
		t.Parallel()
		row := nativeRepoDetailRow("/et/acme/web", nativeTestRepo(), []coreapi.NativeMirrorPlacement{
			{ClusterSlug: "ghost", Status: coreapi.NativeMirrorPlacementStatusReady},
		}, nativeTestClusters)
		require.Empty(t, row.Placements[1].CloneURL)
		require.Equal(t, "ghost", row.Placements[1].Cluster, "the slug still names the placement")
	})
}

// TestNativeUsePlacements pins which clusters `repo remote use` will point a
// remote at: the primary always, and only mirrors that can actually serve a
// fetch.
func TestNativeUsePlacements(t *testing.T) {
	t.Parallel()
	got := nativeUsePlacements(nativeTestRepo(), []coreapi.NativeMirrorPlacement{
		{ClusterSlug: "aws-eu-central-1", Status: coreapi.NativeMirrorPlacementStatusReady},
	}, nativeTestClusters)
	require.Len(t, got, 2)
	require.Equal(t, "aws-us-east-2.entire.io", got[0].ClusterHost)
	require.Equal(t, "aws-us-east-2", got[0].Cell.Or(""), "the picker names clusters by slug")
	require.Equal(t, "us", got[0].Jurisdiction.Or(""))
	require.Equal(t, "aws-eu-central-1.entire.io", got[1].ClusterHost)

	t.Run("a mirror that cannot serve a fetch is not offered", func(t *testing.T) {
		t.Parallel()
		for _, m := range []coreapi.NativeMirrorPlacement{
			{ClusterSlug: "aws-eu-central-1", Status: coreapi.NativeMirrorPlacementStatusProcessing},
			{ClusterSlug: "aws-eu-central-1", Status: coreapi.NativeMirrorPlacementStatusFailed},
			{ClusterSlug: "aws-eu-central-1", Status: coreapi.NativeMirrorPlacementStatusSuspended},
			{ClusterSlug: "aws-eu-central-1", Status: coreapi.NativeMirrorPlacementStatusReady, DesiredState: coreapi.NativeMirrorPlacementDesiredStateDeleted},
		} {
			got := nativeUsePlacements(nativeTestRepo(), []coreapi.NativeMirrorPlacement{m}, nativeTestClusters)
			require.Len(t, got, 1, "only the primary is left for %s/%s", m.Status, m.DesiredState)
		}
	})

	t.Run("a cluster with no usable host is never offered as a remote", func(t *testing.T) {
		t.Parallel()
		got := nativeUsePlacements(nativeTestRepo(), []coreapi.NativeMirrorPlacement{
			{ClusterSlug: "ghost", Status: coreapi.NativeMirrorPlacementStatusReady},
		}, nativeTestClusters)
		require.Len(t, got, 1)
	})
}

// TestRenderNativeMirrorCreateError pins the one server refusal the CLI adds
// to. Every other one is rendered from the server's own detail, so this must
// not fire on them.
func TestRenderNativeMirrorCreateError(t *testing.T) {
	t.Parallel()
	other := &coreapi.ErrorModelStatusCode{StatusCode: 409, Response: coreapi.ErrorModel{
		Detail: coreapi.NewOptString("repo is not active"),
	}}
	got := renderNativeMirrorCreateError(other, "/et/acme/web", "aws-eu-central-1")
	require.EqualError(t, got, "repo is not active")
	require.NotContains(t, got.Error(), "torn down")

	deleting := &coreapi.ErrorModelStatusCode{StatusCode: 409, Response: coreapi.ErrorModel{
		Detail: coreapi.NewOptString("native mirror is being deleted"),
	}}
	got = renderNativeMirrorCreateError(deleting, "/et/acme/web", "aws-eu-central-1")
	require.ErrorContains(t, got, "still being torn down")
	require.ErrorContains(t, got, "entire repo mirror get /et/acme/web")
}

// TestCreateOneNativeMirror_RefusalReachesTheResult runs the refusal through
// the caller rather than the helper, because the helper alone cannot show the
// bug this replaces: the call site rendered the error first, and rendering
// flattens the API error into a plain one, so the detail the hint matches on
// was gone by the time it looked. The helper's own test passed throughout.
func TestCreateOneNativeMirror_RefusalReachesTheResult(t *testing.T) {
	t.Parallel()
	c := newMirrorRequestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeCoreProblem(t, w, http.StatusConflict, "native mirror is being deleted")
	})
	target := mirrorTarget{
		forge: nativeCloneForge, owner: "acme", repo: "web",
		region:     regionChoice{slug: "aws-eu-central-1", jurisdiction: "eu", host: "aws-eu-central-1.entire.io"},
		nativeRepo: nativeTestRepo(),
	}
	res := createOneNativeMirror(t.Context(), target, c, nil, mirrorAddOptions{noWait: true}, func(string, bool, bool) {})

	require.Equal(t, mirrorStatusError, res.status)
	require.ErrorContains(t, res.err, "still being torn down")
	require.ErrorContains(t, res.err, "/et/acme/web")
}

// testMirrorClusterSlug is the cluster every wait-loop fixture places on; the
// loops key on the slug, so one is enough to exercise them.
const testMirrorClusterSlug = "aws-eu-central-1"

// nativeMirrorPoller serves a scripted sequence of /native-mirrors responses,
// one per poll, holding the last once the script runs out.
func nativeMirrorPoller(t *testing.T, pages [][]coreapi.NativeMirrorPlacement) *coreapi.Client {
	t.Helper()
	var calls int
	return newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/native-mirrors") {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		page := pages[min(calls, len(pages)-1)]
		calls++
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListNativeMirrorsOutputBody{NativeMirrors: page})
	})
}

// nativeMirrorAt builds a placement the generated decoder will accept: stage
// and desiredState are required with closed enums, so a response omitting
// either fails validation before any of this code sees it.
func nativeMirrorAt(status coreapi.NativeMirrorPlacementStatus) coreapi.NativeMirrorPlacement {
	return coreapi.NativeMirrorPlacement{
		PlacementId:  "01PLACEMENT",
		ClusterSlug:  testMirrorClusterSlug,
		Status:       status,
		Stage:        coreapi.NativeMirrorPlacementStagePending,
		DesiredState: coreapi.NativeMirrorPlacementDesiredStateActive,
	}
}

// TestAwaitNativeMirrorReady pins every way the wait ends. Readiness is the
// status and only the status: seeding retries server-side, so the loop must sit
// through a failed attempt and stop only on a terminal verdict.
func TestAwaitNativeMirrorReady(t *testing.T) {
	useFastMirrorPolling(t)
	const slug = testMirrorClusterSlug

	t.Run("waits through processing and a server-side retry, then succeeds", func(t *testing.T) {
		retrying := nativeMirrorAt(coreapi.NativeMirrorPlacementStatusProcessing)
		retrying.Attempts = 3
		retrying.CreationFailedAt = coreapi.NewOptDateTime(time.Now())
		retrying.LastError = coreapi.NewOptString("transient")
		c := nativeMirrorPoller(t, [][]coreapi.NativeMirrorPlacement{
			{nativeMirrorAt(coreapi.NativeMirrorPlacementStatusProcessing)},
			{retrying},
			{nativeMirrorAt(coreapi.NativeMirrorPlacementStatusReady)},
		})
		got, err := awaitNativeMirrorReady(t.Context(), c, "01REPO", slug)
		require.NoError(t, err)
		require.Equal(t, coreapi.NativeMirrorPlacementStatusReady, got.Status)
	})

	t.Run("a stage that has run ahead of the status is not readiness", func(t *testing.T) {
		announced := nativeMirrorAt(coreapi.NativeMirrorPlacementStatusProcessing)
		announced.Stage = coreapi.NativeMirrorPlacementStageAnnounced
		c := nativeMirrorPoller(t, [][]coreapi.NativeMirrorPlacement{
			{announced},
			{nativeMirrorAt(coreapi.NativeMirrorPlacementStatusReady)},
		})
		got, err := awaitNativeMirrorReady(t.Context(), c, "01REPO", slug)
		require.NoError(t, err)
		require.Equal(t, coreapi.NativeMirrorPlacementStatusReady, got.Status)
	})

	for name, tc := range map[string]struct {
		page []coreapi.NativeMirrorPlacement
		want string
	}{
		"failed is terminal":    {[]coreapi.NativeMirrorPlacement{nativeMirrorAt(coreapi.NativeMirrorPlacementStatusFailed)}, "failed"},
		"suspended is terminal": {[]coreapi.NativeMirrorPlacement{nativeMirrorAt(coreapi.NativeMirrorPlacementStatusSuspended)}, "suspended"},
	} {
		t.Run(name, func(t *testing.T) {
			c := nativeMirrorPoller(t, [][]coreapi.NativeMirrorPlacement{tc.page})
			_, err := awaitNativeMirrorReady(t.Context(), c, "01REPO", slug)
			require.ErrorContains(t, err, tc.want)
		})
	}

	// The create hands back a row the listing may not carry yet, so one absent
	// poll is tolerated. Exactly one: a row that never appears has to fail
	// rather than spin, which is what waiting on "seen at least once" did — it
	// hung the whole package until the test binary's deadline.
	t.Run("a row missing from the first listing is waited for, not mourned", func(t *testing.T) {
		c := nativeMirrorPoller(t, [][]coreapi.NativeMirrorPlacement{
			nil, // the listing has not caught up with the create
			{nativeMirrorAt(coreapi.NativeMirrorPlacementStatusReady)},
		})
		got, err := awaitNativeMirrorReady(t.Context(), c, "01REPO", slug)
		require.NoError(t, err)
		require.Equal(t, coreapi.NativeMirrorPlacementStatusReady, got.Status)
	})

	t.Run("a row still missing after the grace tick is terminal", func(t *testing.T) {
		c := nativeMirrorPoller(t, [][]coreapi.NativeMirrorPlacement{nil})
		_, err := awaitNativeMirrorReady(t.Context(), c, "01REPO", slug)
		require.ErrorContains(t, err, "no longer listed")
	})

	t.Run("a teardown that overtakes the wait stops it", func(t *testing.T) {
		deleting := nativeMirrorAt(coreapi.NativeMirrorPlacementStatusProcessing)
		deleting.DesiredState = coreapi.NativeMirrorPlacementDesiredStateDeleted
		c := nativeMirrorPoller(t, [][]coreapi.NativeMirrorPlacement{{deleting}})
		_, err := awaitNativeMirrorReady(t.Context(), c, "01REPO", slug)
		require.ErrorContains(t, err, "being torn down")
	})
}

// TestAwaitNativeMirrorRemoved pins the teardown wait: gone means done, and a
// row that stops being on its way out will never satisfy this wait.
func TestAwaitNativeMirrorRemoved(t *testing.T) {
	useFastMirrorPolling(t)
	const slug = testMirrorClusterSlug

	t.Run("waits until the row disappears", func(t *testing.T) {
		deleting := nativeMirrorAt(coreapi.NativeMirrorPlacementStatusReady)
		deleting.DesiredState = coreapi.NativeMirrorPlacementDesiredStateDeleted
		c := nativeMirrorPoller(t, [][]coreapi.NativeMirrorPlacement{{deleting}, {deleting}, nil})
		require.NoError(t, awaitNativeMirrorRemoved(t.Context(), c, "01REPO", slug))
	})

	// One tick of grace, matching the create wait: the first listing after a
	// delete may not carry the intent yet, and a row that still reads active
	// then is lag rather than a re-creation.
	t.Run("a row not yet marked deleted is waited for, not mourned", func(t *testing.T) {
		deleting := nativeMirrorAt(coreapi.NativeMirrorPlacementStatusReady)
		deleting.DesiredState = coreapi.NativeMirrorPlacementDesiredStateDeleted
		c := nativeMirrorPoller(t, [][]coreapi.NativeMirrorPlacement{
			{nativeMirrorAt(coreapi.NativeMirrorPlacementStatusReady)}, // delete not propagated yet
			{deleting},
			nil,
		})
		require.NoError(t, awaitNativeMirrorRemoved(t.Context(), c, "01REPO", slug))
	})

	t.Run("a re-created placement stops the wait instead of looping", func(t *testing.T) {
		c := nativeMirrorPoller(t, [][]coreapi.NativeMirrorPlacement{
			{nativeMirrorAt(coreapi.NativeMirrorPlacementStatusReady)},
		})
		require.ErrorContains(t, awaitNativeMirrorRemoved(t.Context(), c, "01REPO", slug), "re-created")
	})
}

// TestBuildRepoDir_ForgeFilter pins what `--forge` selects. The default view is
// GitHub and must stay byte-identical to what it was before native repos could
// appear here: a script written against it never asked for the change.
func TestBuildRepoDir_ForgeFilter(t *testing.T) {
	t.Parallel()
	hosts := map[string]string{"us": "aws-us-east-2.entire.io"}
	entries := []coreapi.RepoIndexEntry{
		onboardedEntry("acme/web", "private", "us"),
		nativeEntry("acme/native", "private", "us"),
		candidateEntry("acme/mkt", "public", coreapi.RepoCandidateAccessAdmin, true),
	}

	t.Run("gh keeps mirrors and onboardable candidates", func(t *testing.T) {
		t.Parallel()
		rows := buildRepoDir(entries, hosts, mirrorCloneForge)
		require.Equal(t, []string{"/gh/acme/web", "/gh/acme/mkt"}, repoNamesOf(rows))
	})

	// A native repo's clone URL names its own forge. Synthesising a /gh/ one —
	// which is what the directory used to do before it dropped these rows — would
	// hand out a URL that points nowhere.
	t.Run("et keeps native repos and names them by their own forge", func(t *testing.T) {
		t.Parallel()
		rows := buildRepoDir(entries, hosts, nativeCloneForge)
		require.Equal(t, []string{"/et/acme/native"}, repoNamesOf(rows))
		require.Equal(t, "entire://aws-us-east-2.entire.io/et/acme/native", rows[0].Placements[0].CloneURL)
	})

	t.Run("all merges both directories", func(t *testing.T) {
		t.Parallel()
		rows := buildRepoDir(entries, hosts, forgeFilterAll)
		require.Equal(t, []string{"/gh/acme/web", "/et/acme/native", "/gh/acme/mkt"}, repoNamesOf(rows))
	})

	// A provider this build does not know is a definite "neither of ours", so
	// the row is in no view — not even --forge all. Guessing from the placement
	// flag would print it as /gh/… with a clone URL that resolves to nothing.
	t.Run("an unrecognised provider is in no forge's view", func(t *testing.T) {
		t.Parallel()
		future := []coreapi.RepoIndexEntry{{
			FullName: "acme/web", Visibility: "public",
			Provider:   coreapi.NewOptString("gitlab"),
			Placements: []coreapi.RepoPlacement{{ClusterSlug: "us", Status: coreapi.RepoPlacementStatusReady, Mirror: true}},
		}}
		for _, forge := range []string{mirrorCloneForge, nativeCloneForge, forgeFilterAll} {
			require.Empty(t, buildRepoDir(future, hosts, forge), "forge %q", forge)
		}
	})

	// provider is the field that answers "which forge", but it is optional on
	// the wire. A row that OMITS it falls back to its placements' mirror flag
	// rather than dropping out of every view.
	t.Run("an entry with no provider falls back to its placement flag", func(t *testing.T) {
		t.Parallel()
		noProvider := []coreapi.RepoIndexEntry{
			{FullName: "acme/web", Visibility: "public", Placements: []coreapi.RepoPlacement{
				{ClusterSlug: "us", Status: coreapi.RepoPlacementStatusReady, Mirror: true},
			}},
			{FullName: "acme/native", Visibility: "public", Placements: []coreapi.RepoPlacement{
				{ClusterSlug: "us", Status: coreapi.RepoPlacementStatusReady, Mirror: false},
			}},
		}
		require.Equal(t, []string{"/gh/acme/web"}, repoNamesOf(buildRepoDir(noProvider, hosts, mirrorCloneForge)))
		require.Equal(t, []string{"/et/acme/native"}, repoNamesOf(buildRepoDir(noProvider, hosts, nativeCloneForge)))
	})
}

func repoNamesOf(rows []repoDirRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Repo)
	}
	return out
}

// TestRepoMirrorList_ForgeFlag pins the flag's surface: an unknown value fails
// before any request, and the default is GitHub.
func TestRepoMirrorList_ForgeFlag(t *testing.T) {
	t.Parallel()
	cmd := newRepoMirrorListCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--forge", "gitlab"})
	err := cmd.ExecuteContext(t.Context())
	require.ErrorContains(t, err, "invalid --forge")
	require.ErrorContains(t, err, "all")

	require.Equal(t, mirrorCloneForge, newRepoMirrorListCmd().Flag("forge").DefValue,
		"the default view must stay GitHub: native rows are opt-in")
}

// TestMirrorRefOwner_BothForges pins that --owner reads the owner segment of a
// NAME cell on either forge. Stripping only the `gh/` token read "/et/acme/web"
// as owner "et", so `--forge et --owner acme` matched nothing while
// `--owner et` matched every native row.
func TestMirrorRefOwner_BothForges(t *testing.T) {
	t.Parallel()
	require.Equal(t, "acme", mirrorRefOwner("/gh/acme/web"))
	require.Equal(t, "acme", mirrorRefOwner("/et/acme/web"))
	require.Equal(t, "acme", mirrorRefOwner("et/acme/web"), "the leading slash is optional")
	require.Equal(t, "acme", mirrorRefOwner("acme/web"), "a value carrying no forge keeps its first segment")
}

// TestBuildRepoDir_ProviderOutranksThePlacementFlag pins that once `provider`
// has classified a row, the placements are kept as they came. Re-deriving the
// forge per placement can only disagree with the provider, and a disagreement
// empties the row — dropping the repo from the directory altogether.
func TestBuildRepoDir_ProviderOutranksThePlacementFlag(t *testing.T) {
	t.Parallel()
	hosts := map[string]string{"us": "aws-us-east-2.entire.io"}
	// A native repo whose placement is marked mirror:true — the shape
	// cell_fanout.go warns the index can produce.
	entry := coreapi.RepoIndexEntry{
		FullName:   "acme/native",
		Visibility: "private",
		Provider:   coreapi.NewOptString(repoProviderEntire),
		Placements: []coreapi.RepoPlacement{
			{ClusterSlug: "us", Status: coreapi.RepoPlacementStatusReady, Mirror: true},
		},
	}
	rows := buildRepoDir([]coreapi.RepoIndexEntry{entry}, hosts, nativeCloneForge)
	require.Len(t, rows, 1, "the provider says native, so the row survives its placement flag")
	require.Equal(t, "/et/acme/native", rows[0].Repo)
	require.Equal(t, "entire://aws-us-east-2.entire.io/et/acme/native", rows[0].Placements[0].CloneURL)
}

// TestRepoMirrorAdd_ClusterSlugIsAnsweredWithHosts pins what happens when
// someone types the CLUSTER column of `entire cluster list` instead of the HOST
// column. --cluster takes a host, so a slug is an unknown cluster — and the
// refusal has to name the hosts, since a list of slugs would send them round
// the same loop.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoMirrorAdd_ClusterSlugIsAnsweredWithHosts(t *testing.T) {
	serveClusters(t, testClusterCatalog)
	cmd := newRepoMirrorAddCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"/gh/acme/web", "--cluster", "aws-us-east-2"})
	err := cmd.ExecuteContext(t.Context())
	require.ErrorContains(t, err, "unknown cluster")
	require.ErrorContains(t, err, defaultClusterHost, "the answer is the host to type instead")
}

// TestRegionByHost_FoldsCase pins the matching rule for --cluster. DNS is
// case-insensitive, so a mixed-case host must resolve to the same cluster —
// and to the SLUG behind it, which is what the native-mirror API is keyed by.
func TestRegionByHost_FoldsCase(t *testing.T) {
	t.Parallel()
	regions := clustersToRegions([]coreapi.Cluster{
		{Slug: "aws-eu-central-1", PublicUrl: "https://aws-eu-central-1.entire.io"},
	})
	for _, spelling := range []string{"aws-eu-central-1.entire.io", "AWS-EU-CENTRAL-1.ENTIRE.IO", "Aws-Eu-Central-1.Entire.io"} {
		got, ok := regionByHost(regions, spelling)
		require.True(t, ok, spelling)
		require.Equal(t, "aws-eu-central-1", got.slug, "the slug is what the native create is addressed by")
	}
}

// TestRepoRemoteUse_RefusesARepoWithNoCloneURL pins that a repo whose path is
// not set yet — still provisioning — cannot silently blank a remote. `git
// remote set-url` accepts an empty URL and exits 0, so the guard has to be
// ours: verified in a scratch repo, `set-url origin ""` leaves `origin` with no
// URL and reports success.
func TestRepoRemoteUse_RefusesARepoWithNoCloneURL(t *testing.T) {
	t.Parallel()
	provisioning := &coreapi.Repo{
		ID:          "01REPO",
		Provider:    coreapi.NewOptString(repoProviderEntire),
		ClusterSlug: coreapi.NewOptString("aws-us-east-2"),
		// Path unset: the server has not minted clone coordinates yet.
	}
	require.Empty(t, nativeRepoURLAt(provisioning, "aws-us-east-2.entire.io"),
		"no path means no URL, which is what the command must refuse to write")
}
