//go:build e2e

package controlplane

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	// namePrefix marks every resource this suite creates.
	namePrefix = "e2e-cp-"
	// leakedAfter is how old a suite org must be before the sweep treats it
	// as left behind rather than owned by a run still in progress elsewhere.
	leakedAfter = 30 * time.Minute
)

// sweepLeaked deletes the orgs, and everything under them, that earlier runs
// left behind by ending without cleanup: a killed process, a cancelled job, a
// lost runner. The test account may own three orgs, so without this one such
// run would block every later one until someone cleaned up by hand.
//
// The sweep is best effort: a leftover it cannot list or delete is logged and
// skipped rather than failing this run, whose own resources are still deleted
// strictly by its t.Cleanup. A quota it fails to clear surfaces at org create.
func sweepLeaked(t *testing.T, dir string) {
	t.Helper()
	var orgs []struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		CreatedAt time.Time `json:"createdAt"`
	}
	if !sweepList(t, dir, &orgs, "org", "list", "--json") {
		return
	}
	for _, org := range orgs {
		if !strings.HasPrefix(org.Name, namePrefix) || time.Since(org.CreatedAt) < leakedAfter {
			continue
		}
		var projects []struct {
			ID string `json:"id"`
		}
		if !sweepList(t, dir, &projects, "project", "list", "--org", org.ID, "--json") {
			continue
		}
		for _, project := range projects {
			var repos []struct {
				ID string `json:"id"`
			}
			if !sweepList(t, dir, &repos, "repo", "list", project.ID, "--json") {
				continue
			}
			for _, repo := range repos {
				sweepDelete(t, dir, "repo", repo.ID)
			}
			sweepDelete(t, dir, "project", project.ID)
		}
		if sweepDelete(t, dir, "org", org.ID) {
			t.Logf("swept leaked org %s", org.Name)
		}
	}
}

// sweepList runs a list command and decodes its JSON into out, logging and
// reporting false on any failure.
func sweepList(t *testing.T, dir string, out any, args ...string) bool {
	t.Helper()
	stdout, stderr, err := runEntire(t, dir, args...)
	if err != nil {
		t.Logf("sweep: entire %s: %v\n%s", strings.Join(args, " "), err, stderr)
		return false
	}
	if err := json.Unmarshal([]byte(stdout), out); err != nil {
		t.Logf("sweep: entire %s: stdout is not JSON: %v\n%s", strings.Join(args, " "), err, stdout)
		return false
	}
	return true
}

// sweepDelete force-deletes one leftover, logging and reporting false on
// failure.
func sweepDelete(t *testing.T, dir, noun, id string) bool {
	t.Helper()
	if _, stderr, err := runEntireWithTimeout(t, dir, 30*time.Second, noun, "delete", id, "--force"); err != nil {
		t.Logf("sweep: entire %s delete %s: %v\n%s", noun, id, err, stderr)
		return false
	}
	return true
}
