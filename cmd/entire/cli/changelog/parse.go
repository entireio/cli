package changelog

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"gopkg.in/yaml.v3"
)

func (c *Client) parseIndex(source []byte) ([]Entry, error) {
	if !utf8.Valid(source) {
		return nil, errors.New("malformed changelog index: invalid UTF-8")
	}
	doc := goldmark.New().Parser().Parse(text.NewReader(source))
	heading := doc.FirstChild()
	if heading == nil || heading.Kind() != ast.KindHeading || strings.TrimSpace(nodeText(heading, source)) == "" {
		return nil, errors.New("malformed changelog index: missing heading")
	}
	entries := make([]Entry, 0)
	seen := make(map[string]bool)
	categoryFound := false
	for block := heading; block != nil; block = block.NextSibling() {
		switch block.Kind() {
		case ast.KindHeading, ast.KindParagraph:
			// Identify the category before the posts without pinning surrounding copy.
			if len(entries) == 0 {
				words := strings.FieldsFunc(nodeText(block, source), func(r rune) bool {
					return !unicode.IsLetter(r) && !unicode.IsNumber(r)
				})
				for _, word := range words {
					categoryFound = categoryFound || strings.EqualFold(word, "Changelog")
				}
			}
			continue
		case ast.KindList:
			if !categoryFound {
				return nil, errors.New("malformed changelog index: expected Changelog category")
			}
		default:
			return nil, errors.New("malformed changelog index: expected post list")
		}
		for item := block.FirstChild(); item != nil; item = item.NextSibling() {
			entry, err := c.parseItem(item, source)
			if err != nil {
				return nil, fmt.Errorf("malformed changelog index: %w", err)
			}
			if !seen[entry.MarkdownURL] {
				entries = append(entries, entry)
				seen[entry.MarkdownURL] = true
			}
		}
	}
	if !categoryFound {
		return nil, errors.New("malformed changelog index: expected Changelog category")
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Date != entries[j].Date {
			return entries[i].Date > entries[j].Date
		}
		return entries[i].Slug < entries[j].Slug
	})
	return entries, nil
}

func (c *Client) parseItem(item ast.Node, source []byte) (Entry, error) {
	var entry Entry
	paragraph := item.FirstChild()
	if paragraph == nil || paragraph.NextSibling() != nil {
		return entry, errors.New("invalid post list item")
	}
	link, ok := paragraph.FirstChild().(*ast.Link)
	if !ok {
		return entry, errors.New("missing post link")
	}
	entry.Title = strings.TrimSpace(nodeText(link, source))
	if entry.Title == "" {
		return entry, errors.New("missing post title")
	}
	u, err := url.Parse(string(link.Destination))
	if err != nil {
		return entry, fmt.Errorf("post URL: %w", err)
	}
	if err := c.validateURL(u, false); err != nil {
		return entry, err
	}
	var suffix strings.Builder
	for n := link.NextSibling(); n != nil; n = n.NextSibling() {
		suffix.WriteString(nodeText(n, source))
	}
	metadata := strings.TrimSpace(suffix.String())
	if len(metadata) < 14 || metadata[0] != '(' || metadata[11:14] != "): " {
		return entry, errors.New("missing post date or description")
	}
	entry.Date = metadata[1:11]
	if _, err := time.Parse(time.DateOnly, entry.Date); err != nil {
		return entry, fmt.Errorf("invalid post date: %w", err)
	}
	entry.Description = strings.TrimSpace(metadata[14:])
	if entry.Description == "" {
		return entry, errors.New("missing post description")
	}
	entry.MarkdownURL = u.String()
	entry.URL = strings.TrimSuffix(entry.MarkdownURL, ".md")
	entry.Slug = strings.TrimSuffix(path.Base(u.Path), ".md")
	if entry.Slug == "" {
		return entry, errors.New("missing post slug")
	}
	entry.Category = "Changelog"
	return entry, nil
}

// nodeText extracts inline text without exposing emphasis or link syntax.
func nodeText(node ast.Node, source []byte) string {
	switch n := node.(type) {
	case *ast.Text:
		raw := n.Value(source)
		if !n.IsRaw() {
			raw = util.ResolveEntityNames(util.ResolveNumericReferences(util.UnescapePunctuations(raw)))
		}
		value := string(raw)
		if n.SoftLineBreak() || n.HardLineBreak() {
			value += "\n"
		}
		return value
	case *ast.String:
		return string(n.Value)
	default:
		var value strings.Builder
		for child := node.FirstChild(); child != nil; child = child.NextSibling() {
			value.WriteString(nodeText(child, source))
		}
		return value.String()
	}
}

// parsePost validates the frontmatter and returns the body byte-for-byte.
func parsePost(source []byte) (string, error) {
	if !utf8.Valid(source) {
		return "", errors.New("invalid Markdown UTF-8")
	}
	line, rest, ok := bytes.Cut(source, []byte("\n"))
	if !ok || string(bytes.TrimSuffix(line, []byte("\r"))) != "---" {
		return "", errors.New("malformed Markdown post: missing YAML frontmatter")
	}
	start := len(source) - len(rest)
	for len(rest) > 0 {
		line, remaining, _ := bytes.Cut(rest, []byte("\n"))
		if string(bytes.TrimSuffix(line, []byte("\r"))) == "---" {
			var metadata struct {
				Category string `yaml:"category"`
			}
			if err := yaml.Unmarshal(source[start:len(source)-len(rest)], &metadata); err != nil {
				return "", fmt.Errorf("invalid post frontmatter: %w", err)
			}
			if metadata.Category != "Changelog" {
				return "", fmt.Errorf("unexpected post category %q", metadata.Category)
			}
			if len(bytes.TrimSpace(remaining)) == 0 {
				return "", errors.New("empty Markdown post")
			}
			return string(remaining), nil
		}
		rest = remaining
	}
	return "", errors.New("malformed Markdown post: unclosed YAML frontmatter")
}
