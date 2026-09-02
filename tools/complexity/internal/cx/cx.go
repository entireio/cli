// Package cx holds what the cxtool binaries (., ./reach, ./dupl) must agree
// on: the feature-mapping rules, the rank policy, and a few I/O helpers.
// There is exactly one definition of "which feature does this file belong to" —
// keep it that way.
package cx

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
)

// FeatureRule maps path patterns to a feature. A pattern ending in "/" is a
// directory-prefix match; otherwise the directory must match exactly and the
// basename is a glob. A pattern naming one exact path beats every glob and
// directory rule regardless of where its feature sits in the list (see
// matchPass). NoRank marks features that are excluded from rankings, hotspot
// tables and headline totals (test infrastructure, generated code, test-only
// agents) while still being measured.
type FeatureRule struct {
	Name   string   `json:"name"`
	Area   string   `json:"area"`
	Paths  []string `json:"paths"`
	NoRank bool     `json:"norank,omitempty"`
	Note   string   `json:"note,omitempty"`
}

type Config struct {
	Doc      string        `json:"_doc"`
	Features []FeatureRule `json:"features"`
}

// LoadConfig reads the feature mapping. A missing file is an error: every
// binary in this module is useless without the mapping.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("features config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("features config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("features config: %w", err)
	}
	return &c, nil
}

// validate rejects a pattern path.Match cannot parse. matchRule discards that
// error, so an unparseable glob (an unclosed "[", say) would otherwise match
// nothing at all: its files drift into _unmapped while the rule that was meant
// to claim them looks perfectly fine in the config.
func (c *Config) validate() error {
	for i := range c.Features {
		r := &c.Features[i]
		for _, p := range r.Paths {
			if _, ok := dirPrefix(p); ok {
				continue
			}
			_, pbase := path.Split(p)
			if _, err := path.Match(pbase, "probe"); err != nil {
				return fmt.Errorf("feature %q: bad pattern %q: %w", r.Name, p, err)
			}
		}
	}
	return nil
}

func matchRule(pattern, rel string) bool {
	if dir, ok := dirPrefix(pattern); ok {
		return strings.HasPrefix(rel, dir)
	}
	pdir, pbase := path.Split(pattern)
	rdir, rbase := path.Split(rel)
	if pdir != rdir {
		return false
	}
	ok, _ := path.Match(pbase, rbase)
	return ok
}

// Feature is the resolved mapping for one file.
type Feature struct {
	Name   string
	Area   string
	Ranked bool
}

// Unmapped is the feature a path falls into when no rule claims it. Exported
// because callers report the gap (a count, and the paths on stderr) and must
// not have to re-spell the name to recognise it.
var Unmapped = Feature{Name: "_unmapped", Area: "_unmapped", Ranked: false}

// For maps a repo-relative file path to its feature, in three ordered steps,
// first match winning:
//
//  1. a rule naming this exact path, so an explicit _test.go rule always wins;
//  2. for a test file, its source name (foo_test.go -> foo.go), resolved the
//     same way, so a test follows its source wherever the source went;
//  3. glob and directory-prefix rules against the path itself.
//
// Step 2 sits above step 3 on purpose. A glob written for sources usually
// matches their tests too ("setup*.go" catches setup_import_test.go), so
// matching the test's own path first would strand tests in the glob's feature
// while their sources resolved elsewhere — splitting a pair across two
// features and quietly wrecking the test-LOC and ratio columns for both.
func (c *Config) For(rel string) Feature {
	if f, ok := c.matchPass(rel, true); ok {
		return f
	}
	if strings.HasSuffix(rel, "_test.go") {
		src := strings.TrimSuffix(rel, "_test.go") + ".go"
		if f, ok := c.matchPass(src, true); ok {
			return f
		}
		if f, ok := c.matchPass(src, false); ok {
			return f
		}
	}
	if f, ok := c.matchPass(rel, false); ok {
		return f
	}
	return Unmapped
}

// literalPattern reports whether p names one exact path: no glob metacharacter
// and not a directory prefix.
func literalPattern(p string) bool {
	_, isDir := dirPrefix(p)
	return !isDir && !strings.ContainsAny(p, "*?[")
}

// matchPass scans the rules in config order, considering only exact-path rules
// when literals is true and only globs and directory prefixes when it is false.
//
// Separating the two is what keeps a broad earlier glob from silently
// swallowing a later rule that names one file exactly —
// "cmd/entire/cli/setup*.go" absorbing "cmd/entire/cli/setup_import.go". That
// shape is invisible in the output, which is what makes it worth designing
// against rather than just fixing once: the file lands in a plausible wrong
// feature instead of in _unmapped, so one feature over-counts and another
// under-counts with nothing reported. It also makes the config
// order-independent for exact paths, so a one-file rule works wherever its
// feature happens to sit in the list.
func (c *Config) matchPass(rel string, literals bool) (Feature, bool) {
	best, bestSpec, found := Feature{}, -1, false
	for i := range c.Features {
		r := &c.Features[i]
		for _, p := range r.Paths {
			if literalPattern(p) != literals {
				continue
			}
			if !matchRule(p, rel) {
				continue
			}
			if spec := specificity(p); !found || spec > bestSpec {
				best, bestSpec, found = Feature{Name: r.Name, Area: r.Area, Ranked: !r.NoRank}, spec, true
			}
		}
	}
	return best, found
}

// specificity ranks two matching non-literal patterns so the narrower one wins
// regardless of list order. A basename glob names one directory exactly and so
// beats any prefix rule containing it; between two prefix rules the longer one
// wins.
//
// Without this, two overlapping directory rules are resolved by position alone:
// features.json has "e2e/" (test-infra) and "e2e/vogon/" (agent:vogon), and
// today the narrower one happens to sit earlier. Alphabetising the file, or
// inserting a broad rule above a narrow one, would silently move a subtree's
// LOC and coverage to the wrong feature with nothing reported — the same
// invisible misattribution the literal/glob split exists to prevent, left open
// for the one case that split does not cover.
func specificity(pattern string) int {
	if dir, ok := dirPrefix(pattern); ok {
		return len(dir)
	}
	// A glob is scoped to its own directory, so it is narrower than every
	// prefix rule that could match the same path; +1 outranks a prefix of the
	// same length.
	pdir, _ := path.Split(pattern)
	return len(pdir) + 1
}

// dirPrefix reports whether pattern is a directory-prefix rule, and the prefix.
func dirPrefix(pattern string) (string, bool) {
	if strings.HasSuffix(pattern, "/") {
		return pattern, true
	}
	return "", false
}

// ReadModulePath returns the module path of the Go module rooted at dir.
func ReadModulePath(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", err
	}
	mp := modfile.ModulePath(b)
	if mp == "" {
		return "", fmt.Errorf("no module path in %s/go.mod", dir)
	}
	return mp, nil
}

// Check exits with the error on stderr; these are batch CLIs, not libraries.
func Check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// WriteCSV writes one CSV file whole; writers stay pure row-builders.
func WriteCSV(path string, header []string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		f.Close()
		return err
	}
	if err := w.WriteAll(rows); err != nil {
		f.Close()
		return err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// WriteJSON writes one indented JSON file whole, the counterpart to WriteCSV.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// RelSlash returns path relative to root with forward slashes: the form every
// rule in the mapping is written in, and so the only form For accepts.
func RelSlash(root, p string) (string, error) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// SplitList parses a comma-separated flag value, trimming and dropping empties.
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
