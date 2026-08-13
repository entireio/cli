package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

func TestParseMirrorCloneRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		ref       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{name: "leading slash", ref: "/gh/entirehq/entire-api", wantOwner: "entirehq", wantRepo: "entire-api"},
		{name: "no leading slash", ref: "gh/entirehq/entire-api", wantOwner: "entirehq", wantRepo: "entire-api"},
		{name: "lowercased", ref: "/gh/EntireHQ/Entire-API", wantOwner: "entirehq", wantRepo: "entire-api"},
		{name: "wrong provider", ref: "/gl/entirehq/entire-api", wantErr: true},
		{name: "missing repo", ref: "/gh/entirehq", wantErr: true},
		{name: "extra segment", ref: "/gh/entirehq/entire-api/extra", wantErr: true},
		{name: "dot-only repo", ref: "/gh/entirehq/..", wantErr: true},
		{name: "metachar in repo", ref: "/gh/entirehq/repo?x=1", wantErr: true},
		{name: "empty", ref: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider, owner, repo, err := parseMirrorCloneRef(tt.ref)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "github", provider)
			require.Equal(t, tt.wantOwner, owner)
			require.Equal(t, tt.wantRepo, repo)
		})
	}
}

func TestIsEntireCloneURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ref  string
		want bool
	}{
		{ref: "entire://aws-us-east-2.entire.io/gh/entirehq/entire-api", want: true},
		{ref: "  entire://host/gh/a/b", want: true},
		{ref: "/gh/entirehq/entire-api", want: false},
		{ref: "gh/entirehq/entire-api", want: false},
		{ref: "https://github.com/entirehq/entire-api", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, isEntireCloneURL(tt.ref))
		})
	}
}

func TestMirrorCloneURL(t *testing.T) {
	t.Parallel()
	require.Equal(t,
		"entire://aws-us-east-2.entire.io/gh/entirehq/entire-api",
		mirrorCloneURL("aws-us-east-2.entire.io", "entirehq", "entire-api"))
}

func TestMirrorCellLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mirror coreapi.ResolvedPlacement
		want   string
	}{
		{
			name:   "host only",
			mirror: coreapi.ResolvedPlacement{ClusterHost: "aws-us-east-2.entire.io"},
			want:   "aws-us-east-2.entire.io",
		},
		{
			name: "cell and jurisdiction",
			mirror: coreapi.ResolvedPlacement{
				ClusterHost:  "aws-us-east-2.entire.io",
				Cell:         coreapi.NewOptString("aws-us-east-2"),
				Jurisdiction: coreapi.NewOptString("us"),
			},
			want: "aws-us-east-2 (us) — aws-us-east-2.entire.io",
		},
		{
			name: "cell without jurisdiction",
			mirror: coreapi.ResolvedPlacement{
				ClusterHost: "aws-us-east-2.entire.io",
				Cell:        coreapi.NewOptString("aws-us-east-2"),
			},
			want: "aws-us-east-2 — aws-us-east-2.entire.io",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, mirrorCellLabel(tt.mirror))
		})
	}
}

// TestRepoClone_InvalidClusterFlag locks in that a malformed --cluster is
// rejected up front (before any core is dialled), so the anti-token-leak guard
// validateClusterHost applies to the user-supplied cluster the clone routes to.
func TestRepoClone_InvalidClusterFlag(t *testing.T) {
	t.Parallel()
	cmd := newRepoCloneCmd()
	cmd.SetOut(&nopWriter{})
	cmd.SetErr(&nopWriter{})
	cmd.SetArgs([]string{"/gh/entirehq/entire-api", "--cluster", "aws-us-east-2.entire.io@evil.com"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid --cluster")
}

func newCloneTestCmd() *cobra.Command {
	cmd := newRepoCloneCmd()
	cmd.SetOut(&nopWriter{})
	cmd.SetErr(&nopWriter{})
	return cmd
}

// TestCloneDeviceFlagWiring verifies the clone command exposes --device and that
// deviceLoginRequested reflects it, so an inline re-login can honor the
// device-code flow instead of being pinned to the browser redirect.
func TestCloneDeviceFlagWiring(t *testing.T) {
	t.Parallel()

	t.Run("default is browser redirect (device off)", func(t *testing.T) {
		t.Parallel()
		require.False(t, deviceLoginRequested(newCloneTestCmd()))
	})

	t.Run("--device flips deviceLoginRequested on", func(t *testing.T) {
		t.Parallel()
		cmd := newCloneTestCmd()
		require.NoError(t, cmd.Flags().Set("device", "true"))
		require.True(t, deviceLoginRequested(cmd))
	})
}

type nopWriter struct{}

func (*nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestSelectCloneTarget(t *testing.T) {
	t.Parallel()

	usEast := coreapi.ResolvedPlacement{ClusterHost: "aws-us-east-2.entire.io"}
	euWest := coreapi.ResolvedPlacement{ClusterHost: "aws-eu-west-1.entire.io"}

	t.Run("single placement returns directly", func(t *testing.T) {
		t.Parallel()
		got, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast}, "")
		require.NoError(t, err)
		require.Equal(t, "aws-us-east-2.entire.io", got.ClusterHost)
	})

	t.Run("dedupes repeated host to a single placement", func(t *testing.T) {
		t.Parallel()
		got, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, usEast}, "")
		require.NoError(t, err)
		require.Equal(t, "aws-us-east-2.entire.io", got.ClusterHost)
	})

	t.Run("--cluster picks the matching placement", func(t *testing.T) {
		t.Parallel()
		got, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "aws-eu-west-1.entire.io")
		require.NoError(t, err)
		require.Equal(t, "aws-eu-west-1.entire.io", got.ClusterHost)
	})

	t.Run("--cluster matches case-insensitively", func(t *testing.T) {
		t.Parallel()
		// DNS hosts are case-insensitive: a mixed-case --cluster must still match
		// the API's lowercase ClusterHost rather than falsely "not mirrored".
		got, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "AWS-EU-West-1.Entire.IO")
		require.NoError(t, err)
		require.Equal(t, "aws-eu-west-1.entire.io", got.ClusterHost)
	})

	t.Run("--cluster with no match errors and lists hosts", func(t *testing.T) {
		t.Parallel()
		_, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "aws-ap-south-1.entire.io")
		require.Error(t, err)
		require.Contains(t, err.Error(), "aws-us-east-2.entire.io")
		require.Contains(t, err.Error(), "aws-eu-west-1.entire.io")
	})

	t.Run("multiple placements with no terminal errors with a --cluster pointer", func(t *testing.T) {
		t.Parallel()
		// go test is non-interactive, so the picker path is unreachable here.
		_, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{usEast, euWest}, "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "--cluster")
	})
}

// TestResolvePullablePlacements_ReturnsPlacements verifies the clone-discovery
// resolver hits the pull-gated /mirrors/placements endpoint with the upstream
// coords and returns every placement (host + cell + jurisdiction) for the
// picker. A public mirror the caller holds no grant on resolves here even
// though it never would via the affiliation-scoped list — the whole point of
// the endpoint.
func TestResolvePullablePlacements_ReturnsPlacements(t *testing.T) {
	t.Parallel()
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		body := &coreapi.ResolvePlacementsOutputBody{Placements: []coreapi.ResolvedPlacement{
			{MirrorId: "01AAA", ClusterHost: "aws-us-east-2.entire.io", Cell: coreapi.NewOptString("aws-us-east-2"), Jurisdiction: coreapi.NewOptString("us")},
			{MirrorId: "01BBB", ClusterHost: "aws-eu-west-1.entire.io", Cell: coreapi.NewOptString("aws-eu-west-1"), Jurisdiction: coreapi.NewOptString("eu")},
		}}
		if err := printJSON(w, body); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := coreapi.NewWithBearer(srv.URL, "tok")
	require.NoError(t, err)

	got, err := resolvePullablePlacements(t.Context(), c, "karthik-rameshkumar", "my-entire")
	require.NoError(t, err)

	require.Equal(t, "/api/v1/mirrors/placements", gotPath)
	require.Contains(t, gotQuery, "provider=github")
	require.Contains(t, gotQuery, "owner=karthik-rameshkumar")
	require.Contains(t, gotQuery, "repo=my-entire")

	require.Len(t, got, 2)
	require.Equal(t, "aws-us-east-2.entire.io", got[0].ClusterHost)
	require.Equal(t, "aws-us-east-2", got[0].Cell.Or(""))
	require.Equal(t, "us", got[0].Jurisdiction.Or(""))
	require.Equal(t, "01AAA", got[0].MirrorId)
	require.Equal(t, "aws-eu-west-1.entire.io", got[1].ClusterHost)
}

// TestListMirrorsForRepo_FiltersByRepo verifies the client-side repo filter:
// the list API filters provider+owner server-side, but the repo match (which
// the API has no param for) is applied locally.
func TestListMirrorsForRepo_FiltersByRepo(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := &coreapi.ListMirrorsOutputBody{Mirrors: []coreapi.Mirror{
			{Owner: "entirehq", Repo: "entire-api", ClusterHost: "aws-us-east-2.entire.io"},
			{Owner: "entirehq", Repo: "entire-api", ClusterHost: "aws-eu-west-1.entire.io"},
			{Owner: "entirehq", Repo: "entire-cli", ClusterHost: "aws-us-east-2.entire.io"},
		}}
		if err := printJSON(w, body); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := coreapi.NewWithBearer(srv.URL, "tok")
	require.NoError(t, err)

	got, err := listMirrorsForRepo(t.Context(), c, "github", "entirehq", "entire-api")
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, m := range got {
		require.Equal(t, "entire-api", m.Repo)
	}
}
