package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// serveClusterList answers GET /api/v1/clusters with the given catalog,
// standing in for the control plane behind `entire cluster list`.
func serveClusterList(t *testing.T, clusters []coreapi.Cluster) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/clusters", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if err := printJSON(w, &coreapi.ListClustersOutputBody{Clusters: clusters}); err != nil {
			t.Errorf("encode clusters: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// clusterCatalogFixture is a catalog in registry order: two regions, two US
// clusters of which one is the default, and one row whose publicUrl carries the
// host@evil.com userinfo trick.
func clusterCatalogFixture() []coreapi.Cluster {
	return []coreapi.Cluster{
		{Slug: "aws-us-west", Jurisdiction: "us", PublicUrl: "https://aws-us-west-2.entire.io"},
		{Slug: "aws-eu", Jurisdiction: "eu", PublicUrl: "https://aws-eu-central-1.entire.io/", IsDefault: true, ApiUrl: coreapi.NewOptString("https://aws-eu-central-1.api.entire.io")},
		{Slug: "aws-us-east", Jurisdiction: "us", PublicUrl: "https://aws-us-east-2.entire.io", IsDefault: true},
		{Slug: "poisoned", Jurisdiction: "us", PublicUrl: "https://aws-us-east-2.entire.io@evil.com"},
	}
}

// tableCells splits rendered table output into one []string of cells per line,
// so assertions pin the values and their order without pinning column padding.
func tableCells(out string) [][]string {
	var rows [][]string
	for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		rows = append(rows, strings.Fields(line))
	}
	return rows
}

// The table is what a person copies from: REGION feeds `project create
// --region`, HOST feeds `repo mirror create` / `repo create --cluster-host` /
// `repo clone --cluster`, CLUSTER is the slug placements are keyed by. Rows are
// sorted by region then slug, and a publicUrl that cannot be reduced to a safe
// bare host renders dashed rather than spoofable. A catalog holding a
// non-default cluster gains a DEFAULT column, so a reader can see which
// cluster a region falls back to when a command names the region alone.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestClusterList_RendersRegionsAndHosts(t *testing.T) {
	srv := serveClusterList(t, clusterCatalogFixture())

	out, errOut, err := runCoreCmd(t, newClusterCmd, srv.URL, "list")
	require.NoError(t, err)
	require.Empty(t, errOut)

	require.Equal(t, [][]string{
		{"REGION", "CLUSTER", "HOST", "DEFAULT"},
		{"eu", "aws-eu", "aws-eu-central-1.entire.io", "yes"},
		{"us", "aws-us-east", "aws-us-east-2.entire.io", "yes"},
		{"us", "aws-us-west", "aws-us-west-2.entire.io", "-"},
		{"us", "poisoned", "-", "-"},
	}, tableCells(out))
}

// With one cluster per region every cluster is its region's default, so a
// DEFAULT column would read yes on every row and say nothing. It is added only
// when the catalog holds a non-default cluster; this is the live catalog's
// shape today.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestClusterList_OneClusterPerRegionOmitsDefaultColumn(t *testing.T) {
	srv := serveClusterList(t, []coreapi.Cluster{
		{Slug: "aws-us-east", Jurisdiction: "us", PublicUrl: "https://aws-us-east-2.entire.io", IsDefault: true},
		{Slug: "aws-eu", Jurisdiction: "eu", PublicUrl: "https://aws-eu-central-1.entire.io", IsDefault: true},
	})

	out, errOut, err := runCoreCmd(t, newClusterCmd, srv.URL, "list")
	require.NoError(t, err)
	require.Empty(t, errOut)

	require.Equal(t, [][]string{
		{"REGION", "CLUSTER", "HOST"},
		{"eu", "aws-eu", "aws-eu-central-1.entire.io"},
		{"us", "aws-us-east", "aws-us-east-2.entire.io"},
	}, tableCells(out))
}

// --json is the wire model, in the same order as the table: every catalog
// field survives (apiUrl included, which the table omits, and isDefault, which
// the table shows only when some cluster is not one), and publicUrl is passed
// through verbatim. Merged into each object is a synthesized `host`, the same
// validated bare host the table's HOST column shows, so a script gets the safe
// value without re-implementing hostFromPublicURL; where publicUrl fails
// validation the field is absent rather than dashed or spoofable.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestClusterList_JSONIsTheSortedWireModel(t *testing.T) {
	srv := serveClusterList(t, clusterCatalogFixture())

	out, errOut, err := runCoreCmd(t, newClusterCmd, srv.URL, "list", "--json")
	require.NoError(t, err)
	require.Empty(t, errOut)

	var got []coreapi.Cluster
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got, 4)
	slugs := make([]string, 0, len(got))
	for _, cl := range got {
		slugs = append(slugs, cl.Slug)
	}
	require.Equal(t, []string{"aws-eu", "aws-us-east", "aws-us-west", "poisoned"}, slugs)
	require.Equal(t, "https://aws-eu-central-1.api.entire.io", got[0].ApiUrl.Or(""))
	require.False(t, got[1].ApiUrl.IsSet(), "an unset apiUrl must stay absent, not become an empty string")
	require.True(t, got[1].IsDefault)
	require.False(t, got[2].IsDefault)
	require.Equal(t, "https://aws-us-east-2.entire.io@evil.com", got[3].PublicUrl)

	var objs []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(out), &objs))
	require.JSONEq(t, `"aws-eu-central-1.entire.io"`, string(objs[0]["host"]), "trailing slash and scheme reduced to the bare host")
	require.JSONEq(t, `"aws-us-east-2.entire.io"`, string(objs[1]["host"]))
	require.NotContains(t, objs[3], "host", "an unsafe publicUrl gets no host, not a placeholder")
	require.NotContains(t, objs[0], "AdditionalProps", "the wire encoder, not reflection, must produce the object")
}

// clusterJSON writes its validated host only when the object has no host of
// its own, so the day the catalog gains a first-class `host` the synthesis
// silently stops and the raw server value flows through --json unchecked —
// the state finding 1 on this command's trail flagged. This pins the
// assumption: when it fails, decide whether to validate the server's field or
// retire the synthesis, rather than deleting the test.
func TestClusterWireModelHasNoHostField(t *testing.T) {
	t.Parallel()
	_, has := reflect.TypeOf(coreapi.Cluster{}).FieldByName("Host")
	require.False(t, has, "coreapi.Cluster gained a Host field; clusterJSON's synthesized host is now shadowed by an unvalidated server value")
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestClusterList_EmptyCatalogJSONIsEmptyArray(t *testing.T) {
	srv := serveClusterList(t, nil)

	out, errOut, err := runCoreCmd(t, newClusterCmd, srv.URL, "list", "--json")
	require.NoError(t, err)
	require.Empty(t, errOut)
	require.JSONEq(t, "[]", out)
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestClusterList_EmptyCatalog(t *testing.T) {
	srv := serveClusterList(t, nil)

	out, errOut, err := runCoreCmd(t, newClusterCmd, srv.URL, "list")
	require.NoError(t, err)
	require.Empty(t, errOut)
	require.Equal(t, "No clusters found.\n", out)
}
