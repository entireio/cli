package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
		// A printed URL has no `git clone` behind it to reject a malformed one,
		// so the passthrough is validated here and nowhere else — `repo clone`
		// still forwards both of these verbatim.
		{name: "bare scheme rejected", args: []string{"entire://"}, wantErr: "invalid entire URL"},
		{name: "embedded newline rejected", args: []string{"entire://example.com/et/p/r\nfoo"}, wantErr: "whitespace or a control character"},
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
			cmd.SetArgs([]string{"remote", "url", "/et/paul/dogbark.git"})
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
		assert.Equal(t, "/api/v1/mirrors/placements", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
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
	require.Contains(t, terminal.String(), "pick a remote")
	require.Contains(t, terminal.String(), "aws-eu-west-1.entire.io")
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
