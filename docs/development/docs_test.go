package development_test

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// Deliberately scoped to the instruction entry points and extracted references,
// not a network crawler or a general Markdown validator. These docs use inline
// links and ATX headings (including inline code), not reference links or HTML IDs.
var (
	inlineLink = regexp.MustCompile(`\[[^\]\n]*\]\(([^\s)]+)(?:\s+"[^"]*")?\)`)
	heading    = regexp.MustCompile(`^#{1,6}\s+(.+?)\s*#*\s*$`)
)

func TestInstructionDocs(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..") // go test runs in this package's directory.
	data, err := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	const budget = 20 * 1024
	if len(data) > budget {
		t.Errorf("CLAUDE.md is %d bytes (budget %d); move specialized guidance to a reference", len(data), budget)
	}

	references, err := filepath.Glob("*.md") // Includes new, unstaged references.
	if err != nil {
		t.Fatal(err)
	}
	if len(references) == 0 {
		t.Fatal("no development references found")
	}
	references = append(references, filepath.Join(root, "CLAUDE.md"), filepath.Join(root, "CONTRIBUTING.md"))
	for _, file := range references {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			links := inlineLink.FindAllStringSubmatch(withoutFences(string(data)), -1)
			for _, link := range links {
				if err := checkLocalLink(file, link[1]); err != nil {
					t.Error(err)
				}
			}
			if file == filepath.Join(root, "CLAUDE.md") && len(links) == 0 {
				t.Fatal("instruction routing table has no links")
			}
		})
	}
}

func checkLocalLink(source, target string) error {
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("%s: invalid link %q: %w", source, target, err)
	}
	if u.Scheme != "" || u.Host != "" {
		return nil // External links are deliberately not fetched.
	}
	destination := source
	if u.Path != "" {
		destination = filepath.Join(filepath.Dir(source), filepath.FromSlash(u.Path))
	}
	if _, err := os.Stat(destination); err != nil {
		return fmt.Errorf("%s: broken link %q: %w", source, target, err)
	}
	if u.Fragment == "" || filepath.Ext(destination) != ".md" {
		return nil
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		return fmt.Errorf("%s: reading link %q: %w", source, target, err)
	}
	if !markdownAnchors(string(data))[u.Fragment] {
		return fmt.Errorf("%s: link %q names a missing heading", source, target)
	}
	return nil
}

func markdownAnchors(text string) map[string]bool {
	anchors := make(map[string]bool)
	for _, line := range strings.Split(withoutFences(text), "\n") {
		match := heading.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		// GitHub-style slugs for these docs: lowercase, drop punctuation except
		// hyphens/underscores, replace spaces, and suffix duplicate headings.
		base := strings.Map(func(r rune) rune {
			switch {
			case r == ' ':
				return '-'
			case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r):
				return unicode.ToLower(r)
			default:
				return -1
			}
		}, match[1])
		slug := base
		for n := 1; anchors[slug]; n++ {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		anchors[slug] = true
	}
	return anchors
}

func withoutFences(text string) string {
	lines := strings.Split(text, "\n")
	var fence string
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			lines[i] = ""
			if strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]) == "" {
				fence = ""
			}
		} else if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			n := len(trimmed) - len(strings.TrimLeft(trimmed, trimmed[:1]))
			fence = trimmed[:n]
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

func TestCheckLocalLink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "source.md")
	content := "# Local\n## `Some_API` (details)\n## Repeat\n## Repeat\n" +
		"```md\n# Fake\n[bad](missing.md)\n```\n~~~md\n# Also fake\n~~~\n"
	for _, name := range []string{"source.md", "other file.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		target string
		valid  bool
	}{
		{"#local", true},
		{"other%20file.md#some_api-details", true},
		{"other%20file.md#repeat-1", true},
		{"source.md", true},
		{"missing.md", false},
		{"source.md#missing", false},
		{"#fake", false},
		{"#also-fake", false},
		{"https://example.invalid/missing#heading", true},
		{"mailto:someone@example.invalid", true},
	} {
		t.Run(tt.target, func(t *testing.T) {
			t.Parallel()
			err := checkLocalLink(source, tt.target)
			if (err == nil) != tt.valid {
				t.Fatalf("valid = %v; error = %v", tt.valid, err)
			}
		})
	}
	if links := inlineLink.FindAllString(withoutFences(content), -1); len(links) != 0 {
		t.Errorf("links inside code fences must be ignored: %v", links)
	}
}
