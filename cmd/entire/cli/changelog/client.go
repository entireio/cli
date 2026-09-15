// Package changelog reads the public Entire product changelog without authentication.
package changelog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	changelogCategory = "Changelog"
	productionOrigin  = "https://entire.io"
	cliChangelogURL   = "https://raw.githubusercontent.com/entireio/cli/refs/heads/main/CHANGELOG.md"
	maxResponseBytes  = 2 << 20
	requestTimeout    = 20 * time.Second
	fetchConcurrency  = 4
)

// Entry is a published product update. Content preserves the source Markdown/MDX.
type Entry struct {
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Date        string `json:"date"`
	Description string `json:"description"`
	Category    string `json:"category"`
	URL         string `json:"url"`
	MarkdownURL string `json:"markdown_url"`
	Content     string `json:"content"`
}

// Client owns an unauthenticated HTTP client and its permitted origin.
type Client struct {
	http       http.Client
	origin     *url.URL
	releaseURL string
}

// NewClient defaults to the public product and CLI feeds. A custom origin
// serves the product index, posts, and /CHANGELOG.md for tests.
func NewClient(httpClient *http.Client, origin string) (*Client, error) {
	if origin == "" {
		origin = productionOrigin
	}
	u, err := url.Parse(origin)
	if err != nil {
		return nil, fmt.Errorf("changelog origin: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid changelog origin")
	}
	releaseURL := cliChangelogURL
	if origin != productionOrigin {
		releaseURL = strings.TrimRight(origin, "/") + "/CHANGELOG.md"
	}
	c := &Client{origin: u, releaseURL: releaseURL}
	if httpClient != nil {
		c.http = *httpClient
	}
	c.http.Jar = nil // Never attach cookies, including on redirects.
	c.http.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many changelog redirects")
		}
		if via[0].URL.String() == c.releaseURL {
			if req.URL.String() != c.releaseURL {
				return fmt.Errorf("disallowed changelog URL: %s", req.URL.Redacted())
			}
			return nil
		}
		return c.validateURL(req.URL, via[0].URL.Path == "/blog.md")
	}
	return c, nil
}

func (c *Client) validateURL(u *url.URL, index bool) error {
	validPath := strings.HasPrefix(u.Path, "/blog/") && strings.HasSuffix(u.Path, ".md") && path.Clean(u.Path) == u.Path && u.RawQuery == ""
	if index {
		validPath = u.Path == "/blog.md" && u.RawQuery == "category=Changelog"
	}
	if u.Scheme != c.origin.Scheme || u.Host != c.origin.Host || u.User != nil || u.Fragment != "" || u.RawPath != "" || strings.Contains(u.Path, "\\") || !validPath {
		return fmt.Errorf("disallowed changelog URL: %s", u.Redacted())
	}
	return nil
}

func (c *Client) fetch(ctx context.Context, address string, index bool) ([]byte, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("parse changelog URL: %w", err)
	}
	if address != c.releaseURL || index {
		if err := c.validateURL(u, index); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("create changelog request: %w", err)
	}
	req.Header.Set("Accept", "text/markdown")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch changelog %s: %w", address, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch changelog %s: HTTP %d", address, resp.StatusCode)
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "html") {
		return nil, fmt.Errorf("fetch changelog %s: expected Markdown, received HTML", address)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read changelog %s: %w", address, err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("changelog response exceeds %d bytes: %s", maxResponseBytes, address)
	}
	return body, nil
}

// Read returns up to limit posts matching a case-insensitive literal substring,
// newest first. An empty query lists entries without text filtering.
// onlyCLI skips the product feed and returns only CLI releases.
func (c *Client) Read(ctx context.Context, limit int, query string, onlyCLI bool) ([]Entry, error) {
	if limit <= 0 {
		return nil, errors.New("--limit must be positive")
	}
	var entries []Entry
	if !onlyCLI {
		index, err := c.fetch(ctx, strings.TrimRight(c.origin.String(), "/")+"/blog.md?category=Changelog", true)
		if err != nil {
			return nil, err
		}
		entries, err = c.parseIndex(index)
		if err != nil {
			return nil, err
		}
	}
	releases, err := c.fetch(ctx, c.releaseURL, false)
	if err != nil {
		return nil, err
	}
	cliEntries, err := parseReleases(releases, c.releaseURL)
	if err != nil {
		return nil, fmt.Errorf("CLI changelog: %w", err)
	}
	entries = append(entries, cliEntries...)
	sortEntries(entries)
	if query == "" {
		entries = entries[:min(limit, len(entries))]
	}
	return c.selectEntries(ctx, entries, limit, strings.ToLower(query))
}

type fetchResult struct {
	entry Entry
	err   error
}

func (c *Client) selectEntries(ctx context.Context, entries []Entry, limit int, query string) ([]Entry, error) {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	// Fetch ahead by at most fetchConcurrency posts and consume results in order.
	// This keeps a slow newer post from losing its place to a faster older one.
	pending := make([]chan fetchResult, len(entries))
	start := func(i int) {
		pending[i] = make(chan fetchResult, 1)
		workers.Go(func() {
			entry := entries[i]
			var err error
			if entry.Content == "" {
				var body []byte
				body, err = c.fetch(ctx, entry.MarkdownURL, false)
				if err == nil {
					entry.Content, err = parsePost(body)
				}
			}
			if err != nil {
				err = fmt.Errorf("changelog post %s: %w", entry.Slug, err)
			}
			pending[i] <- fetchResult{entry, err}
		})
	}
	next := min(fetchConcurrency, len(entries))
	for i := range next {
		start(i)
	}
	results := make([]Entry, 0, min(limit, len(entries)))
	for i := range entries {
		var result fetchResult
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("read changelog: %w", ctx.Err())
		case result = <-pending[i]:
		}
		if result.err != nil {
			return nil, result.err
		}
		e := result.entry
		if query == "" || strings.Contains(strings.ToLower(e.Title), query) || strings.Contains(strings.ToLower(e.Description), query) || strings.Contains(strings.ToLower(e.Content), query) {
			results = append(results, e)
			if len(results) == limit {
				break
			}
		}
		if next < len(entries) {
			start(next)
			next++
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read changelog: %w", err)
	}
	return results, nil
}
