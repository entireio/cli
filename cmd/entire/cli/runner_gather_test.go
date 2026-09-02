package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Not parallel: swaps newTrailAPIClient and changes the process working directory.
func TestGatherTrailsUsesNativeRepoBaseForFindings(t *testing.T) {
	const basePath = "/api/v1/repos/native-repo-id/trails"

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.RunGit(t, repoDir, "remote", "add", "origin", "entire://aws-us-east-2.entire.io/et/entirehq/marvin")
	t.Chdir(repoDir)

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case basePath:
			if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{{ID: "native-trail", Number: 3}}}); err != nil {
				t.Errorf("encode trail list response: %v", err)
			}
		case basePath + "/3/reviews/comments":
			if err := json.NewEncoder(w).Encode(api.TrailReviewCommentsResponse{Comments: []api.TrailReviewComment{{ID: "finding-1", Status: trailReviewStatusOpen}}}); err != nil {
				t.Errorf("encode findings response: %v", err)
			}
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	previous := newTrailAPIClient
	newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("token", srv.URL), "native-repo-id", nil
	}
	t.Cleanup(func() { newTrailAPIClient = previous })

	got := gatherTrails(t.Context(), io.Discard, 10, false)
	if !strings.Contains(got, "1 past review findings") {
		t.Fatalf("gatherTrails output = %q, want one finding", got)
	}
	if len(paths) != 2 || paths[0] != basePath || paths[1] != basePath+"/3/reviews/comments" {
		t.Fatalf("request paths = %v, want native list and findings routes", paths)
	}
}

// TestReadCapped_TruncatesOnARuneBoundary pins that the byte budget does not cut
// a multi-byte rune in half. The output is embedded in the prompt the runner
// sends to a model, so an invalid UTF-8 sequence there is a defect the caller
// cannot see; the callers' constants are labelled "max chars" while the cap is
// applied in bytes, which is what made this easy to miss.
func TestReadCapped_TruncatesOnARuneBoundary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// "é" is two bytes, so an odd cap always lands mid-rune.
	body := strings.Repeat("é", 100)
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, cap := range []int{1, 7, 21, 99} {
		got, ok := readCapped(dir, "CLAUDE.md", cap)
		if !ok {
			t.Fatalf("readCapped(cap=%d) not ok", cap)
		}
		if !utf8.ValidString(got) {
			t.Errorf("readCapped(cap=%d) produced invalid UTF-8: %q", cap, got)
		}
		if !strings.Contains(got, "truncated") {
			t.Errorf("readCapped(cap=%d) should mark the truncation, got %q", cap, got)
		}
	}
}

// A file inside the cap comes back whole, with no truncation marker.
func TestReadCapped_ReturnsShortFilesWhole(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("héllo"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := readCapped(dir, "README.md", 100)
	if !ok {
		t.Fatal("readCapped() not ok")
	}
	if got != "héllo" {
		t.Errorf("readCapped() = %q, want héllo", got)
	}
}

// A missing file is not an error, just absent.
func TestReadCapped_MissingFile(t *testing.T) {
	t.Parallel()

	if _, ok := readCapped(t.TempDir(), "nope.md", 10); ok {
		t.Error("readCapped() on a missing file should report not-ok")
	}
}
