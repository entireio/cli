package changelog

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testOrigin = "https://entire.io"
const indexDescription = "Published Entire blog posts in Changelog, newest first."

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return string(data)
}

func indexFixture(origin string, count int) string {
	var b strings.Builder
	b.WriteString("# Blog\n\n" + indexDescription + "\n\n")
	// Reverse input order deliberately; every date ties so slugs decide.
	for i := count - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "- [Title %d](%s/blog/post-%02d.md) (2026-09-01): Description %d\n", i, origin, i, i)
	}
	return b.String()
}

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient(server.Client(), server.URL)
	require.NoError(t, err)
	return client
}

func TestParseIndex(t *testing.T) {
	t.Parallel()
	client, err := NewClient(nil, "")
	require.NoError(t, err)
	published := strings.ReplaceAll(fixture(t, "index.md"), "ORIGIN", testOrigin)
	entries, err := client.parseIndex([]byte(published))
	require.NoError(t, err)
	require.Len(t, entries, 4)
	require.Equal(t, "2026-09-09", entries[0].Date)
	require.Equal(t, testOrigin+"/blog/entire-dispatch-0x001d", entries[0].URL)
	for _, tc := range []struct {
		name, source string
		count        int
		title        string
		bad          bool
	}{
		{name: "empty", source: indexFixture(testOrigin, 0)},
		{name: "punctuation drift", source: strings.Replace(indexFixture(testOrigin, 1), "newest first.", "newest first", 1), count: 1, title: "Title 0"},
		{name: "description drift", source: strings.Replace(indexFixture(testOrigin, 1), indexDescription, "Latest Entire product updates in **Changelog**.", 1), count: 1, title: "Title 0"},
		{name: "heading drift", source: strings.Replace(indexFixture(testOrigin, 1), "# Blog", "## Product updates", 1), count: 1, title: "Title 0"},
		{name: "category in heading", source: strings.Replace(indexFixture(testOrigin, 1), "# Blog\n\n"+indexDescription, "# Product changelog", 1), count: 1, title: "Title 0"},
		{name: "extra prose", source: indexFixture(testOrigin, 1) + "\n## About these updates\n\nRead more on the website.\n", count: 1, title: "Title 0"},
		{name: "empty copy drift", source: "# Product updates\n\nNo posts in changelog yet.\n"},
		{name: "ties", source: indexFixture(testOrigin, 3), count: 3, title: "Title 0"},
		{name: "CRLF", source: strings.ReplaceAll(indexFixture(testOrigin, 1), "\n", "\r\n"), count: 1, title: "Title 0"},
		{name: "formatting", source: strings.ReplaceAll(indexFixture(testOrigin, 1), "Title 0", "**Title [日本語]** and `code`"), count: 1, title: "Title [日本語] and code"},
		{name: "escaped title", source: strings.ReplaceAll(indexFixture(testOrigin, 1), "Title 0", `Title \[one\] &amp; two`), count: 1, title: "Title [one] & two"},
		{name: "duplicate", source: indexFixture(testOrigin, 1) + "- [Duplicate](https://entire.io/blog/post-00.md) (2026-09-01): Duplicate description\n", count: 1, title: "Title 0"},
		{name: "HTML", source: "<html>Oops</html>", bad: true},
		{name: "blank", bad: true},
		{name: "category", source: strings.ReplaceAll(published, "in Changelog", "in Company"), bad: true},
		{name: "unfiltered", source: strings.ReplaceAll(published, " in Changelog", ""), bad: true},
		{name: "category substring", source: strings.ReplaceAll(published, "Changelog", "ChangelogArchive"), bad: true},
		{name: "error page", source: "# Service unavailable\n\nPlease try again later.\n", bad: true},
		{name: "date", source: strings.ReplaceAll(published, "2026-09-09", "2026-02-30"), bad: true},
		{name: "description", source: strings.ReplaceAll(indexFixture(testOrigin, 1), "Description 0", ""), bad: true},
		{name: "link", source: strings.ReplaceAll(indexFixture(testOrigin, 1), "[Title 0]", "Title 0"), bad: true},
		{name: "missing date", source: strings.ReplaceAll(indexFixture(testOrigin, 1), " (2026-09-01)", ""), bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := client.parseIndex([]byte(tc.source))
			if tc.bad {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, tc.count)
			if tc.count > 0 {
				require.Equal(t, tc.title, got[0].Title)
			}
		})
	}
}

func TestParsePost(t *testing.T) {
	t.Parallel()
	source := fixture(t, "post.md")
	want := strings.SplitN(source, "---\n", 3)[2]
	body, err := parsePost([]byte(source))
	require.NoError(t, err)
	require.Equal(t, want, body)
	body, err = parsePost([]byte(strings.ReplaceAll(source, "\n", "\r\n")))
	require.NoError(t, err)
	require.Equal(t, strings.ReplaceAll(want, "\n", "\r\n"), body)
	for _, source := range []string{"<html>error</html>", "---\ncategory: Changelog\n", "---\ncategory: [\n---\nbody", "---\ncategory: Company\n---\nbody", "---\ncategory: Changelog\n---\n"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			_, err := parsePost([]byte(source))
			require.Error(t, err)
		})
	}
}

func TestReadSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, query           string
		limit, want, requests int
	}{
		{"default", "", 5, 5, 5}, {"one", "", 1, 1, 1}, {"all", "", 50, 9, 9},
		{"title", "TITLE 7", 5, 1, 9}, {"description", "description 6", 1, 1, 0},
		{"body beyond five", "git network", 2, 2, 9}, {"phrase", "network git", 5, 0, 9},
		{"literal", "git.*network", 5, 0, 9}, {"no matches", "absent", 5, 0, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			counts := map[string]int{}
			var active, maxActive atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Empty(t, r.Header.Get("Authorization"))
				assert.Empty(t, r.Header.Get("Cookie"))
				if r.URL.Path == "/blog.md" {
					assert.Equal(t, "category=Changelog", r.URL.RawQuery)
					fmt.Fprint(w, indexFixture("http://"+r.Host, 9))
					return
				}
				n := active.Add(1)
				defer active.Add(-1)
				for old := maxActive.Load(); n > old; old = maxActive.Load() {
					if maxActive.CompareAndSwap(old, n) {
						break
					}
				}
				mu.Lock()
				counts[r.URL.Path]++
				mu.Unlock()
				if strings.HasSuffix(r.URL.Path, "00.md") {
					time.Sleep(10 * time.Millisecond)
				}
				body := "ordinary body"
				if strings.HasSuffix(r.URL.Path, "07.md") || strings.HasSuffix(r.URL.Path, "08.md") {
					body = "A Git Network update"
				}
				fmt.Fprint(w, "---\ncategory: Changelog\n---\n"+body)
			})
			entries, err := client.Read(t.Context(), tc.limit, tc.query)
			require.NoError(t, err)
			require.Len(t, entries, tc.want)
			for i, e := range entries {
				require.NotEmpty(t, e.Content)
				if i > 0 {
					require.Less(t, entries[i-1].Slug, e.Slug)
				}
			}
			require.LessOrEqual(t, maxActive.Load(), int32(fetchConcurrency))
			mu.Lock()
			defer mu.Unlock()
			if tc.requests > 0 {
				require.Len(t, counts, tc.requests)
			}
			for _, n := range counts {
				require.Equal(t, 1, n)
			}
		})
	}
}

func TestReadFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		status            int
		contentType, body string
		post              bool
	}{
		{name: "status", status: 503},
		{name: "HTML content type", contentType: "text/html", body: "# Blog"},
		{name: "HTML body", body: "<html>bad gateway</html>"},
		{name: "oversize index", body: strings.Repeat("x", maxResponseBytes+1)},
		{name: "oversize post", body: strings.Repeat("x", maxResponseBytes+1), post: true},
		{name: "post failure", status: 500, post: true},
		{name: "wrong category", body: "---\ncategory: Company\n---\nbody", post: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.post && r.URL.Path == "/blog.md" {
					fmt.Fprint(w, indexFixture("http://"+r.Host, 2))
					return
				}
				if tc.post && r.URL.Path == "/blog/post-00.md" {
					fmt.Fprint(w, "---\ncategory: Changelog\n---\nmatch")
					return
				}
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				fmt.Fprint(w, tc.body)
			})
			entries, err := client.Read(t.Context(), 5, "match")
			require.Error(t, err)
			require.Nil(t, entries, "required failures must discard earlier matches")
		})
	}
}

func TestURLRestrictions(t *testing.T) {
	t.Parallel()
	client, err := NewClient(nil, "")
	require.NoError(t, err)
	for _, address := range []string{"http://entire.io/blog/a.md", "https://evil.test/blog/a.md", "https://entire.io.evil.test/blog/a.md", "https://entire.io:444/blog/a.md", "https://user:secret@entire.io/blog/a.md", "https://entire.io/blog/a", "https://entire.io/about/a.md", "https://entire.io/blog/../a.md", "https://entire.io/blog/%2e%2e/a.md", "https://entire.io/blog/a.md?x=1", "https://entire.io/blog/a.md#x"} {
		t.Run(address, func(t *testing.T) {
			t.Parallel()
			_, err := client.parseIndex([]byte(strings.ReplaceAll(indexFixture(testOrigin, 1), testOrigin+"/blog/post-00.md", address)))
			require.Error(t, err)
		})
	}
}

func TestRedirectRestrictions(t *testing.T) {
	t.Parallel()
	var received atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received.Add(1) }))
	t.Cleanup(other.Close)
	for _, index := range []bool{true, false} {
		t.Run(strconv.FormatBool(index), func(t *testing.T) {
			t.Parallel()
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if !index && r.URL.Path == "/blog.md" {
					fmt.Fprint(w, indexFixture("http://"+r.Host, 1))
					return
				}
				http.Redirect(w, r, other.URL+"/blog/redirect.md", http.StatusFound)
			})
			_, err := client.Read(t.Context(), 1, "")
			require.ErrorContains(t, err, "disallowed changelog URL")
			require.Zero(t, received.Load())
		})
	}
}

func TestReadCancellationAndTimeout(t *testing.T) {
	t.Parallel()
	for _, timeout := range []bool{true, false} {
		t.Run(strconv.FormatBool(timeout), func(t *testing.T) {
			t.Parallel()
			started := make(chan struct{})
			client := testClient(t, func(_ http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if timeout {
				client.http.Timeout = 20 * time.Millisecond
			} else {
				go func() { <-started; cancel() }()
			}
			_, err := client.Read(ctx, 1, "")
			require.Error(t, err)
			if timeout {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				require.ErrorIs(t, err, context.Canceled)
			}
		})
	}
}

func TestReadStopsOutstandingWork(t *testing.T) {
	t.Parallel()
	var started atomic.Int32
	allStarted := make(chan struct{})
	var stopped atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/blog.md" {
			fmt.Fprint(w, indexFixture("http://"+r.Host, 12))
			return
		}
		if started.Add(1) == fetchConcurrency {
			close(allStarted)
		}
		<-allStarted
		if r.URL.Path == "/blog/post-00.md" {
			fmt.Fprint(w, "---\ncategory: Changelog\n---\nmatch")
			return
		}
		<-r.Context().Done()
		stopped.Add(1)
	})
	entries, err := client.Read(t.Context(), 1, "match")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, int32(fetchConcurrency), started.Load())
	require.Eventually(t, func() bool { return stopped.Load() == fetchConcurrency-1 }, time.Second, time.Millisecond)
}

func TestReadRejectsLimitBeforeNetwork(t *testing.T) {
	t.Parallel()
	client := testClient(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") })
	for _, limit := range []int{0, -1} {
		_, err := client.Read(t.Context(), limit, "")
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := client.Read(ctx, 1, "")
	require.ErrorIs(t, err, context.Canceled)
}

func TestSameOriginRedirects(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"/blog/renamed.md", "/about.md", "/blog/renamed.md?extra=1", "/blog/../about.md"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/blog.md":
					fmt.Fprint(w, indexFixture("http://"+r.Host, 1))
				case "/blog/post-00.md":
					http.Redirect(w, r, target, http.StatusFound)
				case "/blog/renamed.md":
					fmt.Fprint(w, "---\ncategory: Changelog\n---\nrenamed body")
				default:
					t.Error("followed a disallowed redirect")
				}
			})
			entries, err := client.Read(t.Context(), 1, "")
			if target != "/blog/renamed.md" {
				require.ErrorContains(t, err, "disallowed changelog URL")
				return
			}
			require.NoError(t, err)
			require.Equal(t, "renamed body", entries[0].Content)
		})
	}
}
