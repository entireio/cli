//go:build e2e

package controlplane

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestControlPlane_CreateCloneDelete creates an org, a project in it, and a
// repo in that project, clones the repo, checks its git remote, and deletes
// the three in reverse. Not parallel: the suite shares one test account.
func TestControlPlane_CreateCloneDelete(t *testing.T) {
	dir := t.TempDir()
	sweepLeaked(t, dir)
	// Lowercase and 3–32 chars: valid as an org, project, and repo name.
	name := fmt.Sprintf("%s%d", namePrefix, time.Now().Unix())

	// Each cleanup is registered as soon as the create returns, by name, so a
	// create that succeeds but prints unusable JSON still gets deleted. The ref
	// switches to the ULID once known: a name that no longer resolves is an
	// error, while deleting an already-deleted ULID exits 0.
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
	repoRef := "/et/" + name + "/" + name
	t.Cleanup(func() { deleteResource(t, dir, "repo", repoRef) })
	created := decodeJSON[repoJSON](t, stdout)
	require.NotEmpty(t, created.ID)
	repoRef = created.ID
	repo := waitForRepoClonable(t, dir, "/et/"+name+"/"+name)
	require.Equal(t, created.ID, repo.ID)
	require.Equal(t, "/et/"+name+"/"+name, repo.Path)

	mustRunEntire(t, dir, "repo", "clone", "/et/"+name+"/"+name, name)
	remote := "entire://" + repo.ClusterHost + repo.Path
	remotes := testutil.GitOutput(t, filepath.Join(dir, name), "remote", "-v")
	assert.Contains(t, remotes, "origin\t"+remote+" (fetch)")
	assert.Contains(t, remotes, "origin\t"+remote+" (push)")

	// The server refuses to delete a project with repos or an org with projects.
	assertDeleted(t, dir, "repo", repo.ID)
	assertDeleted(t, dir, "project", project.ID)
	assertDeleted(t, dir, "org", org.ID)
}

type repoJSON struct {
	ID          string `json:"id"`
	ClusterHost string `json:"clusterHost"`
	Path        string `json:"path"`
	State       string `json:"state"`
}

// waitForRepoClonable reads the repo by its /et/<project>/<repo> path, the
// same lookup `repo clone` performs, until that read reports it active with
// clone coordinates. The create response already carries them, but the read
// path can lag behind it for a few seconds, either as a repo that is not yet
// active or as a by-name lookup that does not find the project or repo yet.
func waitForRepoClonable(t *testing.T, dir, ref string) repoJSON {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		stdout, stderr, err := runEntire(t, dir, "repo", "view", ref, "--json")
		var pending string
		if err != nil {
			require.True(t, strings.Contains(stderr, "no repo named") || strings.Contains(stderr, "no project named"),
				"entire repo view %s --json: %v\nstdout:\n%s\nstderr:\n%s", ref, err, stdout, stderr)
			pending = strings.TrimSpace(stderr)
		} else {
			repo := decodeJSON[repoJSON](t, stdout)
			require.NotEqual(t, "failed", repo.State, "repo %s failed to provision", ref)
			if repo.State == "active" && repo.ClusterHost != "" && repo.Path != "" {
				return repo
			}
			pending = fmt.Sprintf("state %q, clusterHost %q, path %q", repo.State, repo.ClusterHost, repo.Path)
		}
		require.True(t, time.Now().Before(deadline), "repo %s not clonable after 2 minutes: %s", ref, pending)
		time.Sleep(3 * time.Second)
	}
}

func assertDeleted(t *testing.T, dir, noun, id string) {
	t.Helper()
	stdout, _ := mustRunEntire(t, dir, noun, "delete", id, "--force")
	assert.Contains(t, stdout, "✓ Deleted "+noun+" "+id)
}

// deleteResource is the cleanup for a resource the test body normally deletes
// itself; the CLI exits 0 on an already-deleted ID.
func deleteResource(t *testing.T, dir, noun, id string) {
	t.Helper()
	if _, stderr, err := runEntireWithTimeout(t, dir, 30*time.Second, noun, "delete", id, "--force"); err != nil {
		t.Errorf("cleanup: entire %s delete %s: %v\n%s", noun, id, err, stderr)
	}
}
