package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/changelog"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func changelogTestClient(t *testing.T, fail bool) (*changelog.Client, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/CHANGELOG.md" {
			fmt.Fprint(w, "# Changelog\n\n## [Unreleased]\n")
			return
		}
		if r.URL.Path == "/blog.md" {
			fmt.Fprint(w, "# Blog\n\nPublished Entire blog posts in Changelog, newest first.\n\n")
			for i := range 7 {
				fmt.Fprintf(w, "- [Update %d](http://%s/blog/post-%d.md) (2026-09-01): Description %d\n", i, r.Host, i, i)
			}
			return
		}
		if fail && r.URL.Path == "/blog/post-1.md" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, "---\ncategory: Changelog\n---\n\nFull **body** with <Example />.\n")
	}))
	t.Cleanup(server.Close)
	client, err := changelog.NewClient(server.Client(), server.URL)
	require.NoError(t, err)
	return client, &requests
}

func TestChangelogCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		args  []string
		count int
		bad   bool
	}{
		{name: "default", args: []string{"--json"}, count: 5},
		{name: "explicit", args: []string{"--limit", "2", "--json"}, count: 2},
		{name: "above corpus", args: []string{"--limit", "50", "--json"}, count: 7},
		{name: "search", args: []string{"search", "body", "--limit", "2", "--json"}, count: 2},
		{name: "empty", args: []string{"search", "absent", "--json"}},
		{name: "zero", args: []string{"--limit", "0"}, bad: true},
		{name: "negative", args: []string{"search", "body", "--limit", "-1"}, bad: true},
		{name: "bad integer", args: []string{"--limit", "abc"}, bad: true},
		{name: "extra arg", args: []string{"extra"}, bad: true},
		{name: "blank", args: []string{"search", " \t"}, bad: true},
		{name: "missing query", args: []string{"search"}, bad: true},
		{name: "extra query", args: []string{"search", "one", "two"}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, requests := changelogTestClient(t, false)
			cmd := newChangelogCmdWithClient(client)
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			cmd.SetArgs(tc.args)
			err := cmd.ExecuteContext(t.Context())
			if tc.bad {
				require.Error(t, err)
				require.Zero(t, requests.Load())
				return
			}
			require.NoError(t, err)
			require.Empty(t, stderr.String())
			require.NotContains(t, out.String(), "\x1b")
			var entries []map[string]any
			require.NoError(t, json.Unmarshal(out.Bytes(), &entries))
			require.Len(t, entries, tc.count)
			if tc.count == 0 {
				require.Equal(t, "[]\n", out.String())
				return
			}
			require.ElementsMatch(t, []string{"slug", "title", "date", "description", "category", "url", "markdown_url", "content"}, slices.Collect(maps.Keys(entries[0])))
			require.Equal(t, "\nFull **body** with <Example />.\n", entries[0]["content"])
		})
	}
}

func TestChangelogRootIndependentOfRepository(t *testing.T) {
	// CWD and environment are process-global; these cases cannot run in parallel.
	for _, kind := range []string{"outside", "disabled", "broken"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if kind != "outside" {
				testutil.InitRepo(t, dir)
			}
			if kind == "broken" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".entire"), []byte("keep"), 0o600))
			}
			t.Chdir(dir)
			t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			client, _ := changelogTestClient(t, false)
			for _, args := range [][]string{{"changelog", "--json", "--limit", "1"}, {"changelog", "search", "absent", "--json"}} {
				root := NewRootCmd()
				old, _, err := root.Find([]string{"changelog"})
				require.NoError(t, err)
				root.RemoveCommand(old)
				root.AddCommand(exemptFromEntireDirCheck(newChangelogCmdWithClient(client)))
				var out, stderr bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&stderr)
				root.SetArgs(args)
				executed, err := root.ExecuteContextC(t.Context())
				require.NoError(t, err)
				require.False(t, ShouldCheckCheckpointPolicyWarning(executed))
				require.True(t, json.Valid(out.Bytes()), out.String())
				require.Empty(t, stderr.String())
			}
			if kind == "broken" {
				data, err := os.ReadFile(filepath.Join(dir, ".entire"))
				require.NoError(t, err)
				require.Equal(t, "keep", string(data))
			} else {
				_, err := os.Stat(filepath.Join(dir, ".entire"))
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func TestChangelogFetchFailureHasNoOutput(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--json"}, {"search", "body", "--json"}, {"search", "body"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			client, _ := changelogTestClient(t, true)
			cmd := newChangelogCmdWithClient(client)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(args)
			require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "HTTP 502")
			require.Empty(t, out.String())
		})
	}
}

type changelogBrokenWriter struct{ err error }

func (w changelogBrokenWriter) Write([]byte) (int, error) { return 0, w.err }

func TestChangelogOutput(t *testing.T) {
	t.Parallel()
	entry := changelog.Entry{Title: "Update", Date: "2026-09-01", URL: "https://entire.io/blog/update", Content: "Full **body**\n<Example />"}
	var out bytes.Buffer
	require.NoError(t, writeChangelog(&out, []changelog.Entry{entry}, false))
	for _, value := range []string{entry.Title, entry.Date, entry.URL, entry.Content} {
		require.Contains(t, out.String(), value)
	}
	require.NotContains(t, out.String(), "\x1b")
	var empty bytes.Buffer
	require.NoError(t, writeChangelog(&empty, nil, false))
	require.Equal(t, "No changelog entries found.\n", empty.String())
	want := errors.New("broken writer")
	for _, asJSON := range []bool{false, true} {
		require.ErrorIs(t, writeChangelog(changelogBrokenWriter{want}, []changelog.Entry{entry}, asJSON), want)
	}
}

func TestChangelogAgentHelp(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"changelog", "--json"}, {"changelog", "search", "--json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			root := NewRootCmd()
			cmd := newAgentHelpCmd(root)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(args)
			require.NoError(t, cmd.ExecuteContext(t.Context()))
			require.True(t, json.Valid(out.Bytes()))
			require.Contains(t, out.String(), "--limit")
			require.Contains(t, out.String(), "--json")
			name := strings.Join(args[:len(args)-1], " ")
			require.Equal(t, agentHelpAudienceReadOnly, agentHelpFactsFor(name).audience)
		})
	}
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	require.NoError(t, root.ExecuteContext(t.Context()))
	require.Contains(t, out.String(), "changelog")
}

func TestChangelogOnlyCLI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		args  []string
		slugs []string
	}{
		{"list", []string{"--only-cli", "--limit", "2", "--json"}, []string{"cli-2.0.0", "cli-1.0.0"}},
		{"limit", []string{"--only-cli", "--limit", "1", "--json"}, []string{"cli-2.0.0"}},
		{"search", []string{"search", "older FIX", "--only-cli", "--limit", "1", "--json"}, []string{"cli-1.0.0"}},
		{"inherited", []string{"--only-cli", "search", "older fix", "--json"}, []string{"cli-1.0.0"}},
		{"no match", []string{"search", "absent", "--only-cli", "--json"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/CHANGELOG.md" {
					t.Errorf("CLI-only request fetched product feed: %s", r.URL.Path)
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				fmt.Fprint(w, "# Changelog\n\n## [1.0.0] - 2026-08-01\n\nOlder fix\n\n## [2.0.0] - 2026-09-01\n\nNew feature\n")
			}))
			t.Cleanup(server.Close)
			client, err := changelog.NewClient(server.Client(), server.URL)
			require.NoError(t, err)
			cmd := newChangelogCmdWithClient(client)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			require.NoError(t, cmd.ExecuteContext(t.Context()))
			var entries []changelog.Entry
			require.NoError(t, json.Unmarshal(out.Bytes(), &entries))
			slugs := make([]string, 0, len(entries))
			for _, entry := range entries {
				slugs = append(slugs, entry.Slug)
			}
			require.Equal(t, tc.slugs, slugs)
			require.Equal(t, int32(1), requests.Load())
		})
	}
}
