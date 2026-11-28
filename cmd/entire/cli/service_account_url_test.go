package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// Not parallel: swaps the package-level activeCoreClient seam.
func TestSAGrantRevoke_EntireComURLs(t *testing.T) {
	grantRoute := "POST " + saBase + "/" + saTestID + "/grants"
	revokeRoute := "DELETE " + saBase + "/" + saTestID + "/grants/repo/" + saTestRepoID
	refs := []string{"/et/acme/web", "https://entire.com/et/acme/web", "entire.com/et/acme/web"}

	// The server names the repo: output and requests match the path form.
	for _, verb := range []string{"grant", "revoke"} {
		t.Run(verb+" is identical for every spelling", func(t *testing.T) {
			var wantOut, wantBody string
			for i, ref := range refs {
				f, srv := newSAFakeCore(t)
				args := []string{verb, "acme/ci", "--repo", ref}
				if verb == "grant" {
					args = append(args, "--role", "writer")
				}
				out, err := runSA(t, srv.URL, args...)
				require.NoError(t, err, ref)
				require.NotContains(t, out, "entire.com", ref)
				body := f.body(grantRoute)
				if verb == "revoke" {
					require.Contains(t, f.callList(), revokeRoute, ref)
				}
				if i == 0 {
					wantOut, wantBody = out, body
					continue
				}
				require.Equal(t, wantOut, out, ref)
				require.Equal(t, wantBody, body, ref)
			}
		})
	}

	// The server omits FullName: the label must fall back to the canonical
	// ref, not the URL the user pasted.
	t.Run("success label is canonical when the server omits FullName", func(t *testing.T) {
		for _, ref := range refs {
			f, srv := newSAFakeCore(t)
			f.resolution = &coreapi.ResolveReposResponse{Resolutions: []coreapi.RepoResolution{{
				Provider:          repoProviderEntire,
				RequestedFullName: "acme/web",
				Status:            coreapi.RepoResolutionStatusReady,
				RepoId:            coreapi.NewOptString(saTestRepoID),
			}}}
			out, err := runSA(t, srv.URL, "grant", "acme/ci", "--repo", ref, "--role", "writer")
			require.NoError(t, err, ref)
			require.Contains(t, out, "✓ Granted writer on repo /et/acme/web ("+saTestRepoID+") to ", ref)
			require.NotContains(t, out, "entire.com", ref)
		}
	})
}
