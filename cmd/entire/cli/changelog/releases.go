package changelog

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

var releaseHeading = regexp.MustCompile(`^\[([^\]]+)\] - (\d{4}-\d{2}-\d{2})$`)

// parseReleases splits top-level release sections, preserving their Markdown bodies.
// Parsing headings as Markdown avoids splitting on examples inside fenced code.
func parseReleases(source []byte, markdownURL string) ([]Entry, error) {
	if !utf8.Valid(source) {
		return nil, errors.New("invalid Markdown UTF-8")
	}
	doc := goldmark.New().Parser().Parse(text.NewReader(source))
	first, ok := doc.FirstChild().(*ast.Heading)
	if !ok || first.Level != 1 || !strings.EqualFold(strings.TrimSpace(nodeText(first, source)), changelogCategory) {
		return nil, errors.New("missing Changelog heading")
	}
	entries := make([]Entry, 0)
	seen := make(map[string]bool)
	var current *Entry
	bodyStart := 0
	flush := func(end int) error {
		if current == nil {
			return nil
		}
		current.Content = strings.TrimSpace(string(source[bodyStart:end]))
		if current.Content == "" {
			return fmt.Errorf("empty release %s", current.Title)
		}
		entries = append(entries, *current)
		current = nil
		return nil
	}
	sections := 0
	for block := first.NextSibling(); block != nil; block = block.NextSibling() {
		heading, ok := block.(*ast.Heading)
		if !ok || heading.Level != 2 {
			continue
		}
		segment := heading.Lines().At(0)
		start := bytes.LastIndexByte(source[:segment.Start], '\n') + 1
		if err := flush(start); err != nil {
			return nil, err
		}
		sections++
		title := strings.TrimSpace(nodeText(heading, source))
		if strings.EqualFold(strings.Trim(title, "[]"), "Unreleased") {
			continue
		}
		match := releaseHeading.FindStringSubmatch(title)
		if match == nil {
			return nil, fmt.Errorf("invalid release heading %q", title)
		}
		version, date := match[1], match[2]
		if _, err := time.Parse(time.DateOnly, date); err != nil {
			return nil, fmt.Errorf("invalid release date: %w", err)
		}
		if seen[version] {
			return nil, fmt.Errorf("duplicate release %q", version)
		}
		seen[version] = true
		current = &Entry{
			Slug: "cli-" + version, Title: "Entire CLI " + version, Date: date,
			Category: changelogCategory, MarkdownURL: markdownURL,
			URL: "https://github.com/entireio/cli/blob/main/CHANGELOG.md#" + strings.ReplaceAll(version, ".", "") + "---" + date,
		}
		bodyStart = segment.Stop
	}
	if err := flush(len(source)); err != nil {
		return nil, err
	}
	if sections == 0 {
		return nil, errors.New("missing release sections")
	}
	return entries, nil
}
