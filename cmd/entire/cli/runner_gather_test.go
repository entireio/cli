package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	testutil.WriteFile(t, dir, "CLAUDE.md", body)

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
	testutil.WriteFile(t, dir, "README.md", "héllo")

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

// TestReadCapped_KeepsContentOfANonUTF8File is the other half of the boundary
// fix. Scanning back for a valid prefix unconditionally walks to 0 on a doc that
// is not UTF-8 at all — every prefix is invalid and "" is not — so the caller
// got the truncation marker and none of the content. Invalidity our cut did not
// cause is the file's own, and the under-cap path passes those bytes through
// too.
func TestReadCapped_KeepsContentOfANonUTF8File(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// 0xE9 is "é" in latin-1 and invalid on its own in UTF-8.
	// 0xE9 is "é" in latin-1 and invalid on its own in UTF-8. Go strings hold
	// arbitrary bytes, so testutil.WriteFile carries it fine.
	body := string(append([]byte{0xE9}, []byte(strings.Repeat("resume of the project. ", 20))...))
	testutil.WriteFile(t, dir, "CLAUDE.md", body)

	got, ok := readCapped(dir, "CLAUDE.md", 40)
	if !ok {
		t.Fatal("readCapped() not ok")
	}
	if !strings.Contains(got, "resume of the project") {
		t.Errorf("readCapped() dropped the file's content: %q", got)
	}
}

// TestReadCapped_FileStartingMidRune is the floor on the boundary backup. The
// loop walks back at most UTFMax-1 continuation bytes, so a file that opens
// with them cannot drive the cut to zero and lose its content.
func TestReadCapped_FileStartingMidRune(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Nothing but continuation bytes: every position looks mid-rune.
	body := strings.Repeat("\x80", 200)
	testutil.WriteFile(t, dir, "CLAUDE.md", body)

	got, ok := readCapped(dir, "CLAUDE.md", 40)
	if !ok {
		t.Fatal("readCapped() not ok")
	}
	content := strings.TrimSuffix(got, "\n…(truncated)…")
	if len(content) < 40-(utf8.UTFMax-1) {
		t.Errorf("readCapped() kept only %d bytes; the backup must be bounded", len(content))
	}
}
