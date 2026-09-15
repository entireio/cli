package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

func TestRepoRemoteURL_Local(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		args          []string
		want, wantErr string
	}{
		{name: "full URL", args: []string{"entire://example.com/et/project/repo"}, want: "entire://example.com/et/project/repo\n"},
		{name: "full URL ignores cluster", args: []string{"entire://example.com/et/project/repo", "--cluster", "other.com"}, want: "entire://example.com/et/project/repo\n"},
		{name: "forge required", args: []string{"project/repo"}, wantErr: "did you mean"},
		{name: "native cluster rejected", args: []string{"/et/project/repo", "--cluster", "example.com"}, wantErr: "--cluster applies"},
		{name: "unknown flag", args: []string{"--unknown"}, wantErr: "unknown flag"},
		{name: "missing ref", wantErr: "accepts 1 arg"},
		{name: "extra ref", args: []string{"a", "b"}, wantErr: "accepts 1 arg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := newRepoRemoteURLCmd()
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs(tc.args)
			err := cmd.ExecuteContext(t.Context())
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				require.Empty(t, errOut.String())
			}
			require.Equal(t, tc.want, out.String())
		})
	}
}

// Not parallel: replaces the process-global activeCoreClient and changes CWD.
func TestRepoRemoteURL_Native(t *testing.T) {
	t.Chdir(t.TempDir()) // URL resolution must work outside a git repository.
	for _, tc := range []struct{ name, host, path, want, wantErr string }{
		{"ready", "aws-us-east-2.entire.io", "/et/paul/dogbark", "entire://aws-us-east-2.entire.io/et/paul/dogbark\n", ""},
		{"provisioning", "", "", "", "no clone URL"},
		{"invalid host", "example.com@evil.com", "/et/paul/dogbark", "", "invalid cluster host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := serveNativeRepo(t, coreapi.Repo{ID: testNativeRepoULID, Name: "dogbark", OwningProjectId: testProjectULID, ClusterHost: coreapi.NewOptString(tc.host), Path: coreapi.NewOptString(tc.path)})
			prev := activeCoreClient
			activeCoreClient = func(context.Context) (*coreapi.Client, error) { return client, nil }
			t.Cleanup(func() { activeCoreClient = prev })
			cmd := newRepoCmd()
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs([]string{"remote-url", "/et/paul/dogbark.git"})
			err := cmd.ExecuteContext(t.Context())
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				require.Empty(t, errOut.String())
			}
			require.Equal(t, tc.want, out.String())
		})
	}
}

// Not parallel: runCoreCmd replaces the process-global activeCoreClient.
func TestRepoRemoteURL_Mirror(t *testing.T) {
	for _, tc := range []struct {
		name          string
		hosts         []string
		cluster       string
		want, wantErr string
	}{
		{"single", []string{"aws-us-east-2.entire.io"}, "", "entire://aws-us-east-2.entire.io/gh/owner/repo\n", ""},
		{"multiple", []string{"aws-us-east-2.entire.io", "eu-west-1.entire.io"}, "", "", "pass --cluster"},
		{"explicit cluster", []string{"aws-us-east-2.entire.io", "eu-west-1.entire.io"}, "eu-west-1.entire.io", "entire://eu-west-1.entire.io/gh/owner/repo\n", ""},
		{"none", nil, "", "", "no mirror found"},
		{"invalid host", []string{"example.com@evil.com"}, "", "", "invalid cluster host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/mirrors/placements", r.URL.Path)
				placements := make([]coreapi.ResolvedPlacement, 0, len(tc.hosts))
				for _, host := range tc.hosts {
					placements = append(placements, coreapi.ResolvedPlacement{ClusterHost: host})
				}
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, printJSON(w, &coreapi.ResolvePlacementsOutputBody{Placements: placements}))
			}))
			t.Cleanup(srv.Close)
			args := []string{"/gh/owner/repo"}
			if tc.cluster != "" {
				prev := clusterCoreClient
				clusterCoreClient = func(_ context.Context, host string) (*coreapi.Client, error) {
					require.Equal(t, tc.cluster, host)
					return coreapi.NewWithBearer(srv.URL, "tok")
				}
				t.Cleanup(func() { clusterCoreClient = prev })
				args = append(args, "--cluster", tc.cluster)
			}
			out, errOut, err := runCoreCmd(t, newRepoRemoteURLCmd, srv.URL, args...)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				require.Empty(t, errOut)
			}
			require.Equal(t, tc.want, out)
		})
	}
}
