// dupl rolls a golangci-lint dupl JSON report up by feature, so the pipeline
// step the README describes actually exists in the tree.
//
//	golangci-lint run -c dupl-config.yaml --new=false --output.json.path=dupl.json ./...
//	dupl -in dupl.json -features features.json -root . -out dupl-by-feature.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"cxtool/internal/cx"
)

// golangciReport is the slice of golangci-lint's JSON output this tool reads.
type golangciReport struct {
	Issues []struct {
		Text string `json:"Text"`
		Pos  struct {
			Filename string `json:"Filename"`
			Line     int    `json:"Line"`
		} `json:"Pos"`
	} `json:"Issues"`
}

type pair struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Other     string `json:"other"`
	OtherLine int    `json:"other_line"`
	Span      int    `json:"span"`
}

type crossPair struct {
	A string `json:"a"`
	B string `json:"b"`
	N int    `json:"n"`
}

var (
	otherRe = regexp.MustCompile("`([^`:]+):(\\d+)-(\\d+)`")
	spanRe  = regexp.MustCompile(`^(\d+)-(\d+) lines`)
)

func main() {
	in := flag.String("in", "dupl.json", "golangci-lint JSON report (dupl linter)")
	featuresPath := flag.String("features", "features.json", "feature mapping")
	rootDir := flag.String("root", ".", "repository root, for normalizing the report's relative paths")
	baseDir := flag.String("base", ".", "directory the report's relative paths resolve against (golangci-lint reports them relative to its config file's directory, not to the repo root)")
	out := flag.String("out", "dupl-by-feature.json", "output file")
	flag.Parse()

	cfg, err := cx.LoadConfig(*featuresPath)
	cx.Check(err)
	absRoot, err := filepath.Abs(*rootDir)
	cx.Check(err)
	absBase, err := filepath.Abs(*baseDir)
	cx.Check(err)

	b, err := os.ReadFile(*in)
	cx.Check(err)
	var rep golangciReport
	cx.Check(json.Unmarshal(b, &rep))

	// One report, two bases. golangci reports Pos.Filename relative to its
	// config file's directory, so it arrives as a chain of ../.., while the
	// path quoted inside the issue text is already repo-relative. Resolving
	// everything against the repo root sent every Pos.Filename out of the repo
	// and so to _unmapped; resolving everything against the config dir does the
	// same to the quoted paths. Try each base and keep the one that lands
	// inside the repo.
	inRepo := func(p string) (string, bool) {
		rel, err := cx.RelSlash(absRoot, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return "", false
		}
		return rel, true
	}
	norm := func(fn string) string {
		if filepath.IsAbs(fn) {
			if rel, ok := inRepo(fn); ok {
				return rel
			}
			return filepath.ToSlash(fn)
		}
		bases := []string{absBase, absRoot}
		// Containment alone cannot choose between the two: the config dir is
		// itself inside the repo, so a repo-relative path joined onto it yields
		// tools/complexity/cmd/... — nominally contained, but naming no file.
		// Existence is the disambiguator.
		for _, base := range bases {
			full := filepath.Join(base, fn)
			if _, err := os.Stat(full); err != nil {
				continue
			}
			if rel, ok := inRepo(full); ok {
				return rel
			}
		}
		for _, base := range bases {
			if rel, ok := inRepo(filepath.Join(base, fn)); ok {
				return rel
			}
		}
		return filepath.ToSlash(fn)
	}

	prod := map[string]int{}
	lines := map[string]int{}
	test := map[string]int{}
	norank := map[string]int{}
	cross := map[[2]string]int{}
	var pairs []pair
	for _, it := range rep.Issues {
		fn := norm(it.Pos.Filename)
		f := cfg.For(fn)
		feat := f.Name
		span := 0
		if m := spanRe.FindStringSubmatch(it.Text); m != nil {
			a, _ := strconv.Atoi(m[1])
			b, _ := strconv.Atoi(m[2])
			span = b - a + 1
		}
		if strings.HasSuffix(fn, "_test.go") {
			test[feat]++
			continue
		}
		// A norank feature is generated or test-infrastructure code, which the
		// rest of the pipeline already keeps out of rankings. Counting its
		// duplication as ordinary production duplication would put generated
		// client boilerplate next to hand-written code in the one report whose
		// whole job is pointing at duplication worth removing.
		if !f.Ranked {
			norank[feat]++
			continue
		}
		prod[feat]++
		lines[feat] += span
		if m := otherRe.FindStringSubmatch(it.Text); m != nil {
			other := norm(m[1])
			otherLine, _ := strconv.Atoi(m[2])
			pairs = append(pairs, pair{File: fn, Line: it.Pos.Line, Other: other, OtherLine: otherLine, Span: span})
			if of := cfg.For(other).Name; of != feat {
				cross[sortedPair(feat, of)]++
			}
		}
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Span > pairs[j].Span })
	if len(pairs) > 40 {
		pairs = pairs[:40]
	}
	var crossOut []crossPair
	for k, n := range cross {
		crossOut = append(crossOut, crossPair{A: k[0], B: k[1], N: n})
	}
	sort.Slice(crossOut, func(i, j int) bool {
		if crossOut[i].N != crossOut[j].N {
			return crossOut[i].N > crossOut[j].N
		}
		return crossOut[i].A < crossOut[j].A
	})

	res := map[string]any{
		"total":             len(rep.Issues),
		"prod_by_feature":   prod,
		"lines_by_feature":  lines,
		"test_by_feature":   test,
		"norank_by_feature": norank,
		"cross":             crossOut,
		"largest":           pairs,
	}
	cx.Check(cx.WriteJSON(*out, res))
	fmt.Fprintf(os.Stderr, "%d issues (%d prod, %d test, %d norank) -> %s\n", len(rep.Issues), sum(prod), sum(test), sum(norank), *out)
}

// sortedPair canonicalises an unordered feature pair so A<->B and B<->A land in
// the same bucket.
func sortedPair(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

func sum(m map[string]int) int {
	t := 0
	for _, v := range m {
		t += v
	}
	return t
}
