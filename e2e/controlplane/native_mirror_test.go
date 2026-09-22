//go:build e2e

package controlplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nativeMirrorSeedTimeout bounds the wait for a seed. Measured against
// production, placing a mirror of a fresh repo took 9 seconds end to end — so
// these are ceilings for a stall, not an expected duration, and they stay
// generous because the cost of being wrong is asymmetric: a run killed by the
// harness skips t.Cleanup and leaks resources on the shared account, while an
// unused ceiling costs nothing.
//
// The CLI is given less than the harness so a stalled seed surfaces as the
// command's own message ("still being created, check with mirror get") rather
// than as a killed process with no explanation.
const (
	nativeMirrorSeedTimeout = 5 * time.Minute
	nativeMirrorStepTimeout = 6 * time.Minute
)

type clusterJSON struct {
	Slug         string `json:"slug"`
	Jurisdiction string `json:"jurisdiction"`
	Host         string `json:"host"`
}

type placementJSON struct {
	Cluster  string `json:"cluster"`
	Status   string `json:"status"`
	Role     string `json:"role"`
	Stage    string `json:"stage"`
	Removing bool   `json:"removing"`
	CloneURL string `json:"cloneUrl"`
}

type repoDirJSON struct {
	Repo       string          `json:"repo"`
	Placements []placementJSON `json:"placements"`
}

// TestControlPlane_NativeMirrorLifecycle drives a native repo through the whole
// mirror surface: see where it lives, place a replica in another region, clone
// from it, push through it, repoint a git remote at it, and tear it back down.
//
// Phases are ordered subtests sharing one repo — the suite is serial against a
// single account, and a seed is too slow to pay for more than once. Each phase
// skips when an earlier one failed, so a single real failure does not read as
// six.
//
// Not parallel: the suite shares one test account.
func TestControlPlane_NativeMirrorLifecycle(t *testing.T) {
	dir := t.TempDir()
	sweepLeaked(t, dir)
	name := fmt.Sprintf("%s%d", namePrefix, time.Now().Unix())
	ref := "/et/" + name + "/" + name

	// Each cleanup is registered as soon as the create returns, by name, so a
	// create that succeeds but prints unusable JSON still gets deleted.
	stdout, _ := mustRunEntire(t, dir, "org", "create", name, "--json")
	orgRef := name
	t.Cleanup(func() { deleteResource(t, dir, "org", orgRef) })
	org := decodeJSON[struct {
		ID string `json:"id"`
	}](t, stdout)
	require.NotEmpty(t, org.ID)
	orgRef = org.ID

	stdout, _ = mustRunEntire(t, dir, "project", "create", name, "--owner", org.ID, "--json")
	projectRef := name
	t.Cleanup(func() { deleteResource(t, dir, "project", projectRef) })
	project := decodeJSON[struct {
		ID string `json:"id"`
	}](t, stdout)
	require.NotEmpty(t, project.ID)
	projectRef = project.ID

	stdout, _ = mustRunEntire(t, dir, "repo", "create", name, "--project", project.ID, "--json")
	repoRef := ref
	t.Cleanup(func() { deleteResource(t, dir, "repo", repoRef) })
	created := decodeJSON[repoJSON](t, stdout)
	require.NotEmpty(t, created.ID)
	repoRef = created.ID

	repo := waitForRepoClonable(t, dir, ref)
	require.NotEmpty(t, repo.ClusterSlug, "a provisioned repo names its primary cluster")

	// A native mirror goes in a region other than the repo's own, so the target
	// is read from the catalog rather than hardcoded: the account's home region
	// is not this test's to assume.
	home, target := pickClusters(t, dir, repo)
	t.Logf("repo %s is primary on %s (%s); mirroring to %s (%s)", ref, repo.ClusterSlug, repo.Jurisdiction, target.Slug, target.Jurisdiction)

	phase := func(label string, fn func(t *testing.T)) {
		if t.Failed() {
			t.Run(label, func(t *testing.T) { t.Skip("an earlier phase failed") })
			return
		}
		t.Run(label, fn)
	}

	phase("before: only the primary is listed", func(t *testing.T) {
		stdout, _ := mustRunEntire(t, dir, "repo", "mirror", "get", ref, "--json")
		row := decodeJSON[repoDirJSON](t, stdout)
		require.Equal(t, ref, row.Repo)
		require.Len(t, row.Placements, 1, "a fresh repo has only its primary")
		require.Equal(t, "primary", row.Placements[0].Role)
		require.Equal(t, repo.ClusterSlug, row.Placements[0].Cluster)
	})

	phase("the forge filter decides which directory the repo is in", func(t *testing.T) {
		native, _ := mustRunEntire(t, dir, "repo", "mirror", "list", "--forge", "et", "--all", "--json")
		require.Contains(t, native, ref, "--forge et lists native repos by their forge-qualified ref")

		gh, _ := mustRunEntire(t, dir, "repo", "mirror", "list", "--all", "--json")
		require.NotContains(t, gh, ref, "the default view is GitHub and must not change") //TODO: this should be addressed in a follow up PR.  this command should print both forges
	})

	phase("refusals cost no write", func(t *testing.T) {
		// Every case must fail before anything is created, which is why they run
		// against a repo whose placements are known to be exactly the primary.
		for _, tc := range []struct {
			name string
			args []string
			want string
		}{
			{"the repo's own cluster is its primary", []string{"mirror", "add", ref, "--cluster", home.Host}, "primary"},
			// --cluster takes the HOST column; the CLUSTER column is the
			// plausible mistake, and the answer names the hosts to use instead.
			{"a slug is not a cluster host", []string{"mirror", "add", ref, "--cluster", target.Slug}, "unknown cluster"},
			{"an unknown cluster names the known ones", []string{"mirror", "add", ref, "--cluster", "no-such-cluster.entire.io"}, "unknown cluster"},
			{"removing the primary is repo delete", []string{"mirror", "remove", ref, "--cluster", home.Host}, "entire repo delete"},
			{"a bare pair names no forge", []string{"mirror", "add", name + "/" + name}, "must name its forge"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, stderr, err := runEntire(t, dir, append([]string{"repo"}, tc.args...)...)
				require.Error(t, err)
				require.Contains(t, stderr, tc.want)
			})
		}
	})

	var cloneURL string
	phase("add places a replica and waits for it to be readable", func(t *testing.T) {
		stdout, stderr, err := runEntireWithTimeout(t, dir, nativeMirrorStepTimeout,
			"repo", "mirror", "add", ref, "--cluster", target.Host, "--timeout", nativeMirrorSeedTimeout.String())
		require.NoError(t, err, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
		// Registered right after the create, so it runs BEFORE the repo delete
		// that was registered earlier (cleanups are LIFO).
		t.Cleanup(func() {
			_, _, _ = runEntireWithTimeout(t, dir, nativeMirrorStepTimeout, "repo", "mirror", "remove", ref, "--cluster", target.Host)
		})
		cloneURL = "entire://" + target.Host + ref
		require.Contains(t, stdout, cloneURL, "a ready mirror prints the URL to clone it from")
	})

	phase("get shows the primary and the mirror, each by role", func(t *testing.T) {
		stdout, _ := mustRunEntire(t, dir, "repo", "mirror", "get", ref, "--json")
		row := decodeJSON[repoDirJSON](t, stdout)
		require.Len(t, row.Placements, 2)
		require.Equal(t, "primary", row.Placements[0].Role)
		byCluster := map[string]placementJSON{}
		for _, p := range row.Placements {
			byCluster[p.Cluster] = p
		}
		mirror, ok := byCluster[target.Slug]
		require.True(t, ok, "the mirror is listed under the cluster it was placed on")
		require.Equal(t, "native_mirror", mirror.Role)
		require.Equal(t, "ready", mirror.Status)
		require.False(t, mirror.Removing)
		require.Equal(t, cloneURL, mirror.CloneURL)
	})

	// Two placements and no terminal — the state every script and CI job is in,
	// and the one the unit tests can only simulate. What matters is that a
	// mirror appearing server-side does not change what an unflagged run
	// resolves to: the answer stays the repo's own primary, and is neither the
	// mirror nor a refusal demanding --cluster.
	phase("with no terminal a second placement still resolves the primary", func(t *testing.T) {
		stdout, _ := mustRunEntire(t, dir, "repo", "remote", "url", ref)
		require.Equal(t, "entire://"+home.Host+ref, strings.TrimSpace(stdout),
			"an unflagged run resolves the primary, not the mirror placed above")

		// A cluster the repo is not on reads the same here as on a /gh/ ref:
		// the clusters it IS on, rather than the failed dial to the named one.
		_, stderr, err := runEntire(t, dir, "repo", "remote", "url", ref, "--cluster", "no-such-cluster.entire.io")
		require.Error(t, err)
		require.Contains(t, stderr, "not mirrored on")
		require.Contains(t, stderr, home.Host)
		require.Contains(t, stderr, target.Host)
	})

	// Anchored on the STATUS token rather than a sentence: the status vocabulary
	// ("exists", "registered", "ready", "removed") is what the summary table and
	// the progress lines both carry, while prose moves whenever the reporting is
	// reshaped — which is exactly how the previous assertions here rotted
	// unnoticed, this suite not being part of CI.
	phase("add is idempotent and says so", func(t *testing.T) {
		stdout, _ := mustRunEntire(t, dir, "repo", "mirror", "add", ref, "--cluster", target.Host, "--no-wait")
		require.Contains(t, stdout, ref)
		require.Contains(t, stdout, "exists", "a second add must report the placement it found, not a fresh create")
		require.NotContains(t, stdout, "registered", "registered is for a placement this run created")
	})

	clone := filepath.Join(dir, "from-mirror")
	phase("the mirror clones from the active login", func(t *testing.T) {
		// Whether a cross-jurisdiction clone needs a login in the mirror's own
		// region is COR-1043; the note predates the 421-following transport, so
		// this asserts the behaviour rather than assuming either answer.
		_, stderr, err := runEntireWithTimeout(t, dir, nativeMirrorStepTimeout, "repo", "clone", cloneURL, clone)
		require.NoError(t, err, "cloning a mirror with the home-region login failed; if this is COR-1043, record it: %s", stderr)
	})

	// A placement serves pushes as well as fetches, so `remote use` writes ONE
	// URL and git needs no pushurl. This pins that: a remote pointed at a mirror
	// both fetches and pushes through it. If mirrors ever became read-only, the
	// push below is what would say so, rather than a user discovering it.
	phase("remote use points one URL at the mirror, and pushing through it works", func(t *testing.T) {
		_, stderr, err := runEntire(t, clone, "repo", "remote", "use", "--cluster", target.Host)
		require.NoError(t, err, stderr)

		remotes := testutil.GitOutput(t, clone, "remote", "-v")
		assert.Contains(t, remotes, "origin\t"+cloneURL+" (fetch)")
		assert.Contains(t, remotes, "origin\t"+cloneURL+" (push)",
			"one URL per remote: a mirror serves pushes too, so there is no split push target")

		require.NoError(t, os.WriteFile(filepath.Join(clone, "through-the-mirror.txt"), []byte("hello\n"), 0o644))
		testutil.CommitIfDirty(t, clone, "push through the mirror")
		out, perr := testutil.GitOutputErr(clone, "push", "origin", "HEAD")
		require.NoError(t, perr, "pushing through a mirror must work:\n%s", out)
	})

	phase("remove tears the replica down", func(t *testing.T) {
		stdout, stderr, err := runEntireWithTimeout(t, dir, nativeMirrorStepTimeout, "repo", "mirror", "remove", ref, "--cluster", target.Host)
		require.NoError(t, err, stderr)
		require.Contains(t, stdout, ref)
		require.Contains(t, stdout, "removed")

		after, _ := mustRunEntire(t, dir, "repo", "mirror", "get", ref, "--json")
		row := decodeJSON[repoDirJSON](t, after)
		require.Len(t, row.Placements, 1, "only the primary is left")
		require.Equal(t, "primary", row.Placements[0].Role)
	})

	// The server refuses to delete a project with repos or an org with projects.
	assertDeleted(t, dir, "repo", created.ID)
	assertDeleted(t, dir, "project", project.ID)
	assertDeleted(t, dir, "org", org.ID)
}

// pickClusters returns the repo's own cluster and one outside its region — the
// only kind a native mirror may be placed on. Both are read from the catalog
// rather than hardcoded, so the test keeps working wherever the account's repos
// land, and both are needed as HOSTS because that is what --cluster takes.
func pickClusters(t *testing.T, dir string, repo repoJSON) (home, foreign clusterJSON) {
	t.Helper()
	stdout, _ := mustRunEntire(t, dir, "cluster", "list", "--json")
	var clusters []clusterJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &clusters), "stdout is not JSON:\n%s", stdout)
	for _, cl := range clusters {
		switch {
		case cl.Slug == repo.ClusterSlug:
			home = cl
		case cl.Jurisdiction != repo.Jurisdiction && cl.Host != "" && foreign.Host == "":
			foreign = cl
		}
	}
	require.NotEmpty(t, home.Host, "the repo's own cluster %s is in the catalog: %s", repo.ClusterSlug, stdout)
	require.NotEmpty(t, foreign.Host, "a cluster outside %s to mirror into: %s", repo.Jurisdiction, stdout)
	return home, foreign
}

// TestControlPlane_RepoCreateRejectsClusterHost pins that a repo's home cluster
// is not the caller's to choose: it is the primary cell of the owning project's
// region. Flag parsing fails before any network call, so this creates nothing.
func TestControlPlane_RepoCreateRejectsClusterHost(t *testing.T) {
	_, stderr, err := runEntire(t, t.TempDir(), "repo", "create", "irrelevant",
		"--project", "irrelevant", "--cluster-host", "aws-us-east-2.entire.io")
	require.Error(t, err)
	require.True(t, strings.Contains(stderr, "unknown flag") && strings.Contains(stderr, "cluster-host"),
		"expected an unknown-flag error, got:\n%s", stderr)
}
