package cli

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

func TestRunMirrorCreateWizard_RequiresTTY(t *testing.T) {
	t.Parallel()
	// In-process tests are non-interactive, so the wizard must refuse before
	// touching auth or the network, pointing at the non-interactive form.
	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetContext(context.Background())

	err := runMirrorCreateWizard(cmd, false, time.Minute)

	var silent *SilentError
	require.ErrorAs(t, err, &silent)
	require.Empty(t, out.String(), "stdout must stay clean")
	require.Contains(t, errOut.String(), "interactive terminal")
	require.Contains(t, errOut.String(), "entire repo mirror create <github-url>")
}

func TestSelectableAvailableRepos(t *testing.T) {
	t.Parallel()
	in := []coreapi.AvailableMirror{
		{Owner: "octocat", Repo: "zeta", Access: coreapi.AvailableMirrorAccessAdmin, Status: coreapi.AvailableMirrorStatusAvailable},
		{Owner: "octocat", Repo: "alpha", Access: coreapi.AvailableMirrorAccessWrite, Status: coreapi.AvailableMirrorStatusAvailable},
		// dropped: read-only access can't onboard
		{Owner: "octocat", Repo: "readonly", Access: coreapi.AvailableMirrorAccessRead, Status: coreapi.AvailableMirrorStatusAvailable},
		// dropped: already mirrored
		{Owner: "octocat", Repo: "done", Access: coreapi.AvailableMirrorAccessWrite, Status: coreapi.AvailableMirrorStatusMirrored},
		// dropped: owner-only
		{Owner: "someone", Repo: "private", Access: coreapi.AvailableMirrorAccessAdmin, Status: coreapi.AvailableMirrorStatusOwnerOnly},
		// kept, sorts before octocat
		{Owner: "acme", Repo: "thing", Access: coreapi.AvailableMirrorAccessWrite, Status: coreapi.AvailableMirrorStatusAvailable},
	}

	got := selectableAvailableRepos(in)

	var keys []string
	for _, m := range got {
		keys = append(keys, m.Owner+"/"+m.Repo)
	}
	require.Equal(t, []string{"acme/thing", "octocat/alpha", "octocat/zeta"}, keys)
}

func TestHostFromPublicURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "https url", in: "https://aws-us-east-2.entire.io", want: "aws-us-east-2.entire.io"},
		{name: "bare host", in: "eu-west-1.entire.io", want: "eu-west-1.entire.io"},
		{name: "host with port", in: "https://localhost:8080", want: "localhost:8080"},
		{name: "trims space", in: "  https://aws-us-east-2.entire.io  ", want: "aws-us-east-2.entire.io"},
		// A trailing slash is a benign catalog shape, not a path injection.
		{name: "trailing slash", in: "https://aws-us-east-2.entire.io/", want: "aws-us-east-2.entire.io"},
		{name: "empty", in: "", wantErr: true},
		// userinfo trick rejected by validateClusterHost
		{name: "userinfo injection", in: "https://aws-us-east-2.entire.io@evil.com", wantErr: true},
		{name: "url with path", in: "https://aws-us-east-2.entire.io/sneaky", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := hostFromPublicURL(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestClusterChoices(t *testing.T) {
	t.Parallel()
	regions := []regionChoice{
		// Both us-east and eu-west are defaults — for their own jurisdictions.
		{slug: "us-east", jurisdiction: "us", host: "aws-us-east-2.entire.io", isDefault: true},
		{slug: "eu-west", jurisdiction: "eu", host: "eu-west-1.entire.io", isDefault: true},
		{host: "bare.entire.io"}, // no slug/jurisdiction
	}

	// Caller is in "eu": every cluster is listed, the eu cluster is ordered
	// first (so it's visible+checked on a short terminal), and only the eu
	// default pre-selects (is_default is per-jurisdiction, so us must not).
	opts, defaults := clusterChoices(regions, "eu")

	require.Len(t, opts, 3)
	require.Equal(t, "eu-west (eu)", opts[0].Key, "caller's jurisdiction listed first")
	require.Equal(t, "eu-west-1.entire.io", opts[0].Value)
	require.Equal(t, []string{"eu-west-1.entire.io"}, defaults)
	// The other jurisdictions are still present, in their original relative order.
	var keys []string
	for _, o := range opts {
		keys = append(keys, o.Key)
	}
	require.ElementsMatch(t, []string{"us-east (us)", "eu-west (eu)", "bare.entire.io"}, keys)

	// Unknown jurisdiction: all still listed, original order, nothing pre-selected.
	noOpts, noneDefault := clusterChoices(regions, "")
	require.Empty(t, noneDefault)
	require.Equal(t, "us-east (us)", noOpts[0].Key)
}

func TestRegionLabel(t *testing.T) {
	t.Parallel()
	require.Equal(t, "us-east (us)", regionLabel(regionChoice{slug: "us-east", jurisdiction: "us", host: "h"}))
	require.Equal(t, "us-east", regionLabel(regionChoice{slug: "us-east", host: "h"}))
	require.Equal(t, "h", regionLabel(regionChoice{host: "h"}))
}

func TestMirrorTargets(t *testing.T) {
	t.Parallel()
	repos := []coreapi.AvailableMirror{
		{Owner: "a", Repo: "x"},
		{Owner: "b", Repo: "y"},
	}
	regions := []regionChoice{
		{host: "r1.entire.io"},
		{host: "r2.entire.io"},
	}

	targets := mirrorTargets(repos, regions)

	// Cross-product: 2 repos × 2 regions = 4 pairs, repo-major order.
	require.Len(t, targets, 4)
	require.Equal(t, mirrorTarget{owner: "a", repo: "x", region: regions[0]}, targets[0])
	require.Equal(t, mirrorTarget{owner: "a", repo: "x", region: regions[1]}, targets[1])
	require.Equal(t, mirrorTarget{owner: "b", repo: "y", region: regions[0]}, targets[2])
	require.Equal(t, mirrorTarget{owner: "b", repo: "y", region: regions[1]}, targets[3])
}

func TestMirrorCreateResultRow(t *testing.T) {
	t.Parallel()
	require.Equal(t,
		[]string{"octocat/hello", "us-east (us)", "ready", "entire://h/gh/octocat/hello"},
		mirrorCreateResultRow(mirrorResult{owner: "octocat", repo: "hello", regionLabel: "us-east (us)", status: "ready", cloneURL: "entire://h/gh/octocat/hello"}),
	)
	// No clone URL (e.g. error/empty) renders a dash.
	require.Equal(t,
		[]string{"octocat/hello", "us-east", "error", placeholderDash},
		mirrorCreateResultRow(mirrorResult{owner: "octocat", repo: "hello", regionLabel: "us-east", status: "error"}),
	)
}

func TestMirrorProgress_NonTTY(t *testing.T) {
	t.Parallel()
	// A bytes.Buffer is non-interactive, so the progress degrades to one printed
	// line per mirror as it reaches a terminal state — no cursor escapes, and
	// non-final updates print nothing.
	var buf bytes.Buffer
	p := newMirrorProgress(&buf, []string{"a/x @ aws-eu-central-1.entire.io", "b/y @ aws-us-east-2.entire.io"})
	p.start()
	p.set(0, "processing", false, false) // in-flight: prints nothing
	require.Empty(t, buf.String())
	p.set(0, "ready", true, true)
	p.set(1, "failed", true, false)
	p.stop()

	out := buf.String()
	require.Contains(t, out, "✓ a/x @ aws-eu-central-1.entire.io ready")
	require.Contains(t, out, "✗ b/y @ aws-us-east-2.entire.io failed")
	require.NotContains(t, out, "\033[", "non-tty output must not emit cursor escapes")
}

func TestClustersToRegions(t *testing.T) {
	t.Parallel()
	in := []coreapi.Cluster{
		{Slug: "us-east", Jurisdiction: "us", PublicUrl: "https://aws-us-east-2.entire.io", IsDefault: true},
		{Slug: "eu-west", Jurisdiction: "eu", PublicUrl: "eu-west-1.entire.io"},
		// dropped: public_url can't reduce to a bare host (userinfo trick)
		{Slug: "bad", Jurisdiction: "us", PublicUrl: "https://aws-us-east-2.entire.io@evil.com"},
	}

	got := clustersToRegions(in)

	require.Equal(t, []regionChoice{
		{slug: "us-east", jurisdiction: "us", host: "aws-us-east-2.entire.io", isDefault: true},
		{slug: "eu-west", jurisdiction: "eu", host: "eu-west-1.entire.io"},
	}, got)
}
