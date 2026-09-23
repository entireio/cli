package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
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
		// A printed URL has no `git clone` behind it to reject a malformed one,
		// so the passthrough is validated here and nowhere else — `repo clone`
		// still forwards both of these verbatim.
		{name: "bare scheme rejected", args: []string{"entire://"}, wantErr: "invalid entire URL"},
		{name: "embedded newline rejected", args: []string{"entire://example.com/et/p/r\nfoo"}, wantErr: "whitespace or a control character"},
		{name: "forge required", args: []string{"project/repo"}, wantErr: "did you mean"},
		{name: "native malformed cluster rejected", args: []string{"/et/project/repo", "--cluster", "example.com@evil.com"}, wantErr: "invalid --cluster"},
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
				// Not an exact-empty check on out: cobra writes usage to
				// OutOrStderr(), which returns the SetOut buffer once a test
				// calls SetOut, so an arg/flag error lands here even though it
				// goes to os.Stderr in production. What must hold either way is
				// that a failed run emits no URL for a `$(...)` to capture.
				require.NotContains(t, out.String(), entireCloneURLScheme)
				return
			}
			require.NoError(t, err)
			require.Empty(t, errOut.String())
			require.Equal(t, tc.want, out.String())
		})
	}
}

// Not parallel: replaces the process-global activeCoreClient and changes CWD.
//
// The ref keeps its `.git` suffix end to end: on a native ref the suffix is
// part of the repo name, so it reaches the server verbatim and the URL is the
// server's own path for the repo that name resolved to.
func TestRepoRemoteURL_Native(t *testing.T) {
	t.Chdir(t.TempDir()) // URL resolution must work outside a git repository.
	for _, tc := range []struct{ name, host, path, want, wantErr string }{
		{"ready", "aws-us-east-2.entire.io", "/et/paul/dogbark.git", "entire://aws-us-east-2.entire.io/et/paul/dogbark.git\n", ""},
		{"provisioning", "", "", "", "no clone URL"},
		{"invalid host", "example.com@evil.com", "/et/paul/dogbark.git", "", "invalid cluster host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queriedFullName string
			client := serveNativeRepoFixture(t, nativeRepoFixture{
				repo:            coreapi.Repo{ID: testNativeRepoULID, Name: "dogbark.git", OwningProjectId: testProjectULID, ClusterHost: coreapi.NewOptString(tc.host), Path: coreapi.NewOptString(tc.path)},
				queriedFullName: &queriedFullName,
			})
			prev := activeCoreClient
			activeCoreClient = func(context.Context) (*coreapi.Client, error) { return client, nil }
			t.Cleanup(func() { activeCoreClient = prev })
			cmd := newRepoCmd()
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs([]string{"remote", "url", "/et/paul/dogbark.git"})
			err := cmd.ExecuteContext(t.Context())
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				require.Empty(t, errOut.String())
			}
			require.Equal(t, tc.want, out.String())
			// `.git` is part of a native repo's name: the lookup must be asked
			// for "dogbark.git", not silently trimmed to "dogbark" before it
			// ever reaches the request.
			require.Equal(t, "paul/dogbark.git", queriedFullName)
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
		// Every GitHub mirror set includes the default cluster, so a run with no
		// terminal resolves it rather than demanding --cluster.
		{"multiple", []string{"eu-west-1.entire.io", defaultClusterHost}, "", "entire://" + defaultClusterHost + "/gh/owner/repo\n", ""},
		{"multiple without the default cluster", []string{"eu-west-1.entire.io", "aws-ap-south-1.entire.io"}, "", "", "pass --cluster"},
		// --cluster names the cluster host, which is both what the command dials
		// and what lands in the URL.
		{"explicit cluster", []string{"aws-us-east-2.entire.io", "eu-west-1.entire.io"}, "eu-west-1.entire.io", "entire://eu-west-1.entire.io/gh/owner/repo\n", ""},
		{"none", nil, "", "", "no mirror found"},
		{"invalid host", []string{"example.com@evil.com"}, "", "", "invalid cluster host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				assert.Equal(t, "/api/v1/mirrors/placements", r.URL.Path)
				placements := make([]coreapi.ResolvedPlacement, 0, len(tc.hosts))
				for _, host := range tc.hosts {
					placements = append(placements, coreapi.ResolvedPlacement{ClusterHost: host})
				}
				assert.NoError(t, printJSON(w, &coreapi.ResolvePlacementsOutputBody{Placements: placements}))
			}))
			t.Cleanup(srv.Close)
			args := []string{"/gh/owner/repo"}
			if tc.cluster != "" {
				prev := clusterCoreClient
				clusterCoreClient = func(_ context.Context, host string) (*coreapi.Client, error) {
					require.Equal(t, tc.cluster, host, "--cluster is the host dialled")
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

// TestRepoRemoteURL_UnknownClusterReadsTheSameOnBothForges pins the one
// message a mistyped --cluster produces, whichever forge the ref names. The
// /gh/ path dials the named cluster before looking anything up, so that it can
// see mirrors held in another federation; an unreachable host used to surface
// that dial's DNS failure while the /et/ path — which lists from the active
// context — answered with the repo's actual clusters. Same mistake, same
// answer now.
//
// Not parallel: swaps the package-global activeCoreClient/clusterCoreClient.
func TestRepoRemoteURL_UnknownClusterReadsTheSameOnBothForges(t *testing.T) {
	const unknown = "wrongcluster"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.Equal(t, "/api/v1/mirrors/placements", r.URL.Path)
		assert.NoError(t, printJSON(w, &coreapi.ResolvePlacementsOutputBody{Placements: []coreapi.ResolvedPlacement{
			{ClusterHost: defaultClusterHost},
			{ClusterHost: "aws-eu-central-1.entire.io"},
		}}))
	}))
	t.Cleanup(srv.Close)

	// The name does not resolve, which is the shape discovery really produces
	// for a typo and the only one the fallback keys on, so the fake carries a
	// genuine *net.DNSError under the same sentinel.
	prev := clusterCoreClient
	clusterCoreClient = func(_ context.Context, host string) (*coreapi.Client, error) {
		require.Equal(t, unknown, host)
		return nil, fmt.Errorf("%w: dial tcp: %w", clusterdiscovery.ErrUnreachable,
			&net.DNSError{Err: "no such host", Name: host, IsNotFound: true})
	}
	t.Cleanup(func() { clusterCoreClient = prev })

	_, _, err := runCoreCmd(t, newRepoRemoteURLCmd, srv.URL, "/gh/owner/repo", "--cluster", unknown)
	require.ErrorContains(t, err, `repo is not mirrored on "`+unknown+`"`)
	require.Contains(t, err.Error(), defaultClusterHost, "the answer names the clusters the repo is actually on")
	require.NotContains(t, err.Error(), "no such host", "the dial failure is a debug detail, not the user's answer")
}

// TestRepoRemoteURL_ReachableClusterKeepsItsOwnError is the other half of the
// unknown-cluster rule. Only an unreachable host may be answered with the
// active context's placement list; a cluster that exists but rejects the
// selected login is mirroring the repo perfectly well, so replacing its
// "pick another context" instruction with "not mirrored" would state the
// opposite of the truth and strand the user one flag away from success.
//
// Not parallel: swaps the package-global activeCoreClient/clusterCoreClient.
func TestRepoRemoteURL_ReachableClusterKeepsItsOwnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, printJSON(w, &coreapi.ResolvePlacementsOutputBody{Placements: []coreapi.ResolvedPlacement{
			{ClusterHost: defaultClusterHost},
		}}))
	}))
	t.Cleanup(srv.Close)

	prev := clusterCoreClient
	clusterCoreClient = func(context.Context, string) (*coreapi.Client, error) {
		return nil, errors.New("cluster other.example does not accept the login selected by --context")
	}
	t.Cleanup(func() { clusterCoreClient = prev })

	_, _, err := runCoreCmd(t, newRepoRemoteURLCmd, srv.URL, "/gh/owner/repo", "--cluster", "other.example")
	require.ErrorContains(t, err, "does not accept the login")
	require.NotContains(t, err.Error(), "not mirrored")
}

// TestRepoRemoteURL_BothLookupsFailingReportsBoth covers the one path where
// the fallback has nothing to offer. The named cluster did not answer and the
// active context could not stand in for it, and the two failures are
// independent things to fix — reporting only the first would have the user
// correct the host, re-run, and only then discover their login is gone.
//
// Not parallel: swaps the package-global activeCoreClient/clusterCoreClient.
func TestRepoRemoteURL_BothLookupsFailingReportsBoth(t *testing.T) {
	prevCluster := clusterCoreClient
	clusterCoreClient = func(_ context.Context, host string) (*coreapi.Client, error) {
		return nil, fmt.Errorf("%w: dial tcp: %w", clusterdiscovery.ErrUnreachable,
			&net.DNSError{Err: "no such host", Name: host, IsNotFound: true})
	}
	t.Cleanup(func() { clusterCoreClient = prevCluster })

	prevActive := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return nil, errors.New("active login has expired")
	}
	t.Cleanup(func() { activeCoreClient = prevActive })

	cmd := newRepoRemoteURLCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"/gh/owner/repo", "--cluster", "wrongcluster"})

	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	require.Contains(t, err.Error(), "no such host", "the host the user named did not answer")
	require.Contains(t, err.Error(), "active login has expired", "and the fallback says why it could not answer either")
	require.NotContains(t, out.String(), entireCloneURLScheme)
}

// TestRepoRemoteURL_UnreachableClusterIsNotTreatedAsAbsent covers the failure
// that looks like a typo and is not one. A cluster in another federation that
// times out is unreachable, but nothing about that says the repo is not
// mirrored there — and the active context cannot see that federation, so its
// placement list is not evidence of absence. Answering from it would resurrect
// the bug the cluster-addressed dial exists to fix: "not mirrored on
// <host>" for a mirror that is really there, or, with nothing mirrored in the
// active context, an instruction to onboard a repo that is already onboarded.
//
// Not parallel: swaps the package-global activeCoreClient/clusterCoreClient.
func TestRepoRemoteURL_UnreachableClusterIsNotTreatedAsAbsent(t *testing.T) {
	const elsewhere = "royalcanin.partial.to"

	for _, tc := range []struct {
		name     string
		fallback []coreapi.ResolvedPlacement
	}{
		// The active context answers, but about its own federation only.
		{"fallback lists other clusters", []coreapi.ResolvedPlacement{{ClusterHost: defaultClusterHost}}},
		// And when it holds nothing, silence is not proof either.
		{"fallback lists nothing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, printJSON(w, &coreapi.ResolvePlacementsOutputBody{Placements: tc.fallback}))
			}))
			t.Cleanup(srv.Close)

			prev := clusterCoreClient
			clusterCoreClient = func(context.Context, string) (*coreapi.Client, error) {
				return nil, fmt.Errorf("%w: dial tcp: %w", clusterdiscovery.ErrUnreachable,
					&net.DNSError{Err: "i/o timeout", Name: elsewhere, IsTimeout: true})
			}
			t.Cleanup(func() { clusterCoreClient = prev })

			_, _, err := runCoreCmd(t, newRepoRemoteURLCmd, srv.URL, "/gh/owner/repo", "--cluster", elsewhere)
			require.ErrorContains(t, err, "i/o timeout", "the connectivity failure is the answer")
			require.NotContains(t, err.Error(), "not mirrored", "we never reached the federation that would know")
			require.NotContains(t, err.Error(), "mirror add", "and must not tell the user to onboard what may already exist")
		})
	}
}

// TestRepoRemoteURL_PickerKeepsStdoutClean is the regression test for the
// command's one hard contract: `git remote add entire "$(entire repo
// remote url …)"` must capture the URL and nothing else, even when the mirror
// is on several clusters and the picker runs.
//
// Without a terminal seam this path is untestable — CanPromptInteractively()
// is false under go test, so the form is never constructed and the routing in
// selectPlacement could be deleted with every other test still green. So it
// forces interactivity, hands the picker a fake terminal, and asserts the
// prompt landed there rather than on stdout.
//
// Not parallel: sets process-global env and replaces two package-level seams.
func TestRepoRemoteURL_PickerKeepsStdoutClean(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1") // make CanPromptInteractively() true
	t.Setenv("ACCESSIBLE", "1")      // line-based form, so input can be scripted

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The picker labels clusters by slug, so it reads the catalog.
		if r.URL.Path == testClustersPath {
			assert.NoError(t, printJSON(w, &coreapi.ListClustersOutputBody{Clusters: []coreapi.Cluster{
				{Slug: "aws-us-east-2", PublicUrl: "https://aws-us-east-2.entire.io"},
				{Slug: "aws-eu-west-1", PublicUrl: "https://aws-eu-west-1.entire.io"},
			}}))
			return
		}
		assert.Equal(t, "/api/v1/mirrors/placements", r.URL.Path)
		assert.NoError(t, printJSON(w, &coreapi.ResolvePlacementsOutputBody{Placements: []coreapi.ResolvedPlacement{
			{ClusterHost: "aws-us-east-2.entire.io"},
			{ClusterHost: "aws-eu-west-1.entire.io"},
		}}))
	}))
	t.Cleanup(srv.Close)

	// Hosts are offered case-folded and sorted, so 1 = eu-west, 2 = us-east.
	var terminal bytes.Buffer
	prevTerm := openPlacementPromptTerminal
	openPlacementPromptTerminal = func() (placementPromptTerminal, error) {
		return placementPromptTerminal{in: strings.NewReader("2\n"), out: &terminal}, nil
	}
	t.Cleanup(func() { openPlacementPromptTerminal = prevTerm })

	stdout, stderr, err := runCoreCmd(t, newRepoRemoteURLCmd, srv.URL, "/gh/owner/repo")
	require.NoError(t, err)

	// The whole point: stdout is exactly the URL, byte for byte.
	require.Equal(t, "entire://aws-us-east-2.entire.io/gh/owner/repo\n", stdout)
	require.Empty(t, stderr)

	// And the prompt really did render — on the terminal, not into the capture.
	// Clusters are offered by slug, which is what --cluster takes.
	require.Contains(t, terminal.String(), "pick a remote")
	require.Contains(t, terminal.String(), "aws-eu-west-1")
}

// TestValidateEntireURLForPrinting_OffsetMatchesTheQuotedString calls the
// validator directly with untrimmed input, which the CLI cannot currently
// produce — resolveRepoRemoteURL trims before calling it. The function trims
// for itself rather than relying on that, so the offset it reports and the
// string it quotes have to agree without the caller's help.
func TestValidateEntireURLForPrinting_OffsetMatchesTheQuotedString(t *testing.T) {
	t.Parallel()

	err := validateEntireURLForPrinting("   entire://host.entire.io/gh/o/r\nfoo   ")
	require.Error(t, err)

	// The quoted string is the trimmed one, and the reported offset indexes it.
	const want = "entire://host.entire.io/gh/o/r"
	require.Contains(t, err.Error(), `"`+want+`\nfoo"`)
	require.Contains(t, err.Error(), "at offset "+strconv.Itoa(len(want)))
}
