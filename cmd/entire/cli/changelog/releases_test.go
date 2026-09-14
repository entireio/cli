package changelog

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseReleases(t *testing.T) {
	t.Parallel()
	source := "# Changelog\n\nPreamble\n\n## [Unreleased]\n\nFuture feature\n\n## [1.2.0] - 2026-09-02\n\n### Added\n\n- Released feature\n\n```md\n## Example heading\n```\n\n## [1.1.0] - 2026-08-01\n\nOlder fix\n"
	for _, newline := range []string{"\n", "\r\n"} {
		t.Run(fmt.Sprintf("newline %q", newline), func(t *testing.T) {
			t.Parallel()
			entries, err := parseReleases([]byte(strings.ReplaceAll(source, "\n", newline)), cliChangelogURL)
			require.NoError(t, err)
			require.Len(t, entries, 2)
			require.Equal(t, "Entire CLI 1.2.0", entries[0].Title)
			require.Equal(t, "2026-09-02", entries[0].Date)
			require.Contains(t, entries[0].Content, "## Example heading")
			require.NotContains(t, entries[0].Content, "Older fix")
			require.NotContains(t, entries[0].Content, "Future feature")
			require.Equal(t, "Older fix", entries[1].Content)
			require.Equal(t, cliChangelogURL, entries[0].MarkdownURL)
		})
	}
	for _, source := range []string{"", "<html>failure</html>", "# Changelog\n", "# Changelog\n\n## [1] - 2026-02-30\nbody", "# Changelog\n\n## [1] - 2026-01-01\n", "# Changelog\n\n## [1] - yesterday\nbody", "# Changelog\n\n## [1] - 2026-01-01\na\n## [1] - 2026-01-02\nb", "# Changelog\n\xff"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			_, err := parseReleases([]byte(source), cliChangelogURL)
			require.Error(t, err)
		})
	}
}

func TestReadMergedReleases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query string
		limit int
		slugs []string
	}{
		{"", 3, []string{"cli-2.0.0", "post-00", "cli-1.0.0"}},
		{"", 1, []string{"cli-2.0.0"}},
		{"", 20, []string{"cli-2.0.0", "post-00", "cli-1.0.0"}},
		{"shared phrase", 2, []string{"post-00", "cli-1.0.0"}},
		{"CLI 2.0.0", 1, []string{"cli-2.0.0"}},
		{"absent", 5, []string{}},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.query, tc.limit), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Empty(t, r.Header.Get("Authorization"))
				assert.Empty(t, r.Header.Get("Cookie"))
				switch r.URL.Path {
				case "/blog.md":
					fmt.Fprint(w, indexFixture("http://"+r.Host, 1))
				case "/CHANGELOG.md":
					fmt.Fprint(w, "# Changelog\n\n## [1.0.0] - 2026-08-31\n\nShared phrase\n\n## [2.0.0] - 2026-09-02\n\nLatest fix\n")
				case "/blog/post-00.md":
					fmt.Fprint(w, "---\ncategory: Changelog\n---\nShared phrase")
				default:
					t.Errorf("unexpected request %s", r.URL)
				}
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.Client(), server.URL)
			require.NoError(t, err)
			entries, err := client.Read(t.Context(), tc.limit, tc.query, false)
			require.NoError(t, err)
			slugs := make([]string, 0, len(entries))
			for _, entry := range entries {
				slugs = append(slugs, entry.Slug)
			}
			require.Equal(t, tc.slugs, slugs)
		})
	}
}

func TestReleaseFetchFailures(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"status", "malformed", "oversize", "redirect"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/blog.md" {
					fmt.Fprint(w, indexFixture("http://"+r.Host, 1))
					return
				}
				assert.Equal(t, "/CHANGELOG.md", r.URL.Path)
				switch failure {
				case "status":
					w.WriteHeader(http.StatusBadGateway)
				case "malformed":
					fmt.Fprint(w, "not a changelog")
				case "oversize":
					fmt.Fprint(w, strings.Repeat("x", maxResponseBytes+1))
				case "redirect":
					http.Redirect(w, r, "/blog/redirect.md", http.StatusFound)
				}
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.Client(), server.URL)
			require.NoError(t, err)
			entries, err := client.Read(t.Context(), 1, "", false)
			require.Error(t, err)
			require.Nil(t, entries)
		})
	}
}

func TestSortMergedEntriesSameDate(t *testing.T) {
	t.Parallel()
	entries := []Entry{
		{Slug: "product", Date: "2026-09-02"},
		{Slug: "cli-1.0.0", Date: "2026-09-02"},
		{Slug: "older", Date: "2026-09-01"},
		{Slug: "newest", Date: "2026-09-03"},
	}
	sortEntries(entries)
	require.Equal(t, []Entry{
		{Slug: "newest", Date: "2026-09-03"},
		{Slug: "cli-1.0.0", Date: "2026-09-02"},
		{Slug: "product", Date: "2026-09-02"},
		{Slug: "older", Date: "2026-09-01"},
	}, entries)
}
