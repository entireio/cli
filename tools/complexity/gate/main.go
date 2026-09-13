// gate turns the cxtool tables into a CI check. It has two modes, and they
// are deliberately separate invocations because their failure semantics are
// opposites.
//
// Complexity diff (blocking):
//
//	gate -base out/base -head out/head [-cognit-warn 30]
//
// Each directory is a cxtool output directory (it needs only functions.csv).
// The gate fails when a function in head is over the threshold *and* is new or
// worse than it was at base. A function that is over the threshold and
// unchanged does not fail: this repo has a long tail of those, and a gate that
// fired on them would only ever be switched off.
//
// Coverage advisory (never blocking):
//
//	gate -features out/head/features.csv -baseline baseline/<date>/features.csv
//	     [-cov-drop 2.0] [-min-stmts 300]
//
// Compares each ranked feature's statement coverage against a committed
// snapshot and warns on a drop past the tolerance. The snapshot goes stale as
// the tree moves — features are renamed, files change hands, suites are
// resharded — so this half reports and never fails. It exits 0 even when it
// cannot read its inputs, emitting a warning annotation instead, so a stale
// path in the workflow can never redden a pull request.
package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"cxtool/internal/cx"
)

func main() {
	base := flag.String("base", "", "cxtool output directory for the base tree (holds functions.csv); with -head, runs the blocking complexity diff")
	head := flag.String("head", "", "cxtool output directory for the head tree (holds functions.csv); with -base, runs the blocking complexity diff")
	cognitWarn := flag.Int("cognit-warn", 30, "cognitive complexity a function may not exceed once it is new or worsened")
	features := flag.String("features", "", "features.csv from the head tree; with -baseline, runs the advisory coverage comparison")
	baseline := flag.String("baseline", "", "committed baseline features.csv to compare against; with -features, runs the advisory coverage comparison")
	covDrop := flag.Float64("cov-drop", 2.0, "coverage percentage points a feature may lose against the baseline before it is reported")
	minStmts := flag.Int("min-stmts", 300, "ignore features with fewer than this many statements in the head tree; small features swing on a handful of lines")
	flag.Usage = usage
	flag.Parse()

	diff := *base != "" || *head != ""
	cov := *features != "" || *baseline != ""
	switch {
	case diff && cov:
		cx.Check(errors.New("-base/-head and -features/-baseline are separate modes; run one at a time"))
	case diff:
		if *base == "" || *head == "" {
			cx.Check(errors.New("the complexity diff needs both -base and -head"))
		}
		os.Exit(complexityDiff(os.Stdout, *base, *head, *cognitWarn))
	case cov:
		if *features == "" || *baseline == "" {
			cx.Check(errors.New("the coverage advisory needs both -features and -baseline"))
		}
		coverageAdvisory(os.Stdout, *features, *baseline, *covDrop, *minStmts)
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintf(out, `gate — CI gates over the cxtool tables.

Complexity diff (exits 1 on a finding):
  gate -base <dir> -head <dir> [-cognit-warn N]

    Each <dir> is a cxtool output directory; only functions.csv is read. A
    function fails the gate when its head cognitive complexity is above
    -cognit-warn AND it is new (no function of that name in that file at base,
    so a moved or renamed file counts as entirely new), or its base complexity
    was within the threshold, or it grew. Pre-existing violations that did not
    move do not fail.

Coverage advisory (always exits 0):
  gate -features <features.csv> -baseline <features.csv> [-cov-drop F] [-min-stmts N]

    Reports ranked features whose statement coverage fell more than -cov-drop
    points below the committed baseline. Advisory because the baseline is a
    snapshot of one commit and drifts as the tree moves. Unreadable inputs are
    reported as warnings, not failures.

Flags:
`)
	flag.PrintDefaults()
}

// ---------- complexity diff ----------

// FuncMetric is one functions.csv row, reduced to what the diff compares.
type FuncMetric struct {
	File   string
	Name   string
	Start  int
	Cognit int
}

// key identifies a function across two trees. File plus name is the whole
// identity available in the CSVs: there are no stable symbol IDs, and line
// numbers move for reasons that have nothing to do with complexity. The cost
// is that renaming a file re-reports every over-threshold function in it as
// new, which is the safe direction for a gate to be wrong in.
func (f FuncMetric) key() string { return f.File + "\x00" + f.Name }

// Offender is a function the diff gate refuses.
type Offender struct {
	FuncMetric
	Base    int  // base complexity
	IsNew   bool // no counterpart at base, so Base is meaningless
	Comment string
}

// findOffenders returns the functions in head that are over the limit and
// either absent from base, within the limit at base, or worse than at base.
//
// Sorted worst-first, then by file and line, so the annotation order is
// stable across runs and platforms.
func findOffenders(base, head []FuncMetric, limit int) []Offender {
	prev := make(map[string]int, len(base))
	for _, f := range base {
		// Duplicate keys cannot arise from a compiling package, but if one
		// ever does, keeping the highest complexity makes the gate lenient
		// rather than firing on an ambiguity we cannot resolve.
		if old, ok := prev[f.key()]; !ok || f.Cognit > old {
			prev[f.key()] = f.Cognit
		}
	}
	var out []Offender
	for _, f := range head {
		if f.Cognit <= limit {
			continue
		}
		was, existed := prev[f.key()]
		switch {
		case !existed:
			out = append(out, Offender{FuncMetric: f, IsNew: true, Comment: "new function"})
		case was <= limit:
			out = append(out, Offender{FuncMetric: f, Base: was, Comment: fmt.Sprintf("was %d, within the limit", was)})
		case f.Cognit > was:
			out = append(out, Offender{FuncMetric: f, Base: was, Comment: fmt.Sprintf("grew from %d", was)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Cognit != b.Cognit {
			return a.Cognit > b.Cognit
		}
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		return a.Name < b.Name
	})
	return out
}

// complexityDiff reads both trees, reports, and returns the process exit code.
func complexityDiff(w io.Writer, baseDir, headDir string, limit int) int {
	base, err := readFuncs(baseDir)
	cx.Check(err)
	head, err := readFuncs(headDir)
	cx.Check(err)

	offenders := findOffenders(base, head, limit)
	if len(offenders) == 0 {
		fmt.Fprintf(w, "complexity gate: no new or worsened function over cognitive complexity %d (%d functions compared against %d)\n", limit, len(head), len(base))
		return 0
	}

	for _, o := range offenders {
		wasNote := strconv.Itoa(o.Base)
		if o.IsNew {
			wasNote = "new"
		}
		msg := fmt.Sprintf("function %s cognitive complexity %d (limit %d, base %s)", o.Name, o.Cognit, limit, wasNote)
		fmt.Fprintf(w, "::error file=%s,line=%d::%s\n", escapeProperty(o.File), o.Start, escapeData(msg))
	}

	fmt.Fprintf(w, "\ncomplexity gate: %d function(s) exceed cognitive complexity %d and are new or worsened:\n\n", len(offenders), limit)
	for _, o := range offenders {
		fmt.Fprintf(w, "  %3d  %s:%d  %s  (%s)\n", o.Cognit, o.File, o.Start, o.Name, o.Comment)
	}
	fmt.Fprintf(w, "\nSplit the function, or lower its nesting. Pre-existing violations are exempt;\nthis fires only on code this change added or made harder to read.\n")
	return 1
}

// ---------- coverage advisory ----------

// FeatureCoverage is one features.csv row, reduced to what the advisory reads.
type FeatureCoverage struct {
	Name   string
	Ranked bool
	Stmts  int
	CovPct float64 // -1 when the feature has no statements ("n/a" in the CSV)
}

// CovRegression is one feature that lost more coverage than the tolerance.
type CovRegression struct {
	Feature string
	Base    float64
	Head    float64
	Stmts   int
}

// findCovRegressions compares ranked features present in both tables. A
// feature is skipped when it is unranked (test infrastructure and generated
// code, which the mapping excludes from headline numbers), when either side
// has no coverage figure, or when the head side is smaller than minStmts —
// coverage on a small feature swings several points on a single helper.
func findCovRegressions(baseline, head []FeatureCoverage, drop float64, minStmts int) []CovRegression {
	was := make(map[string]FeatureCoverage, len(baseline))
	for _, f := range baseline {
		was[f.Name] = f
	}
	var out []CovRegression
	for _, f := range head {
		if !f.Ranked || f.Stmts < minStmts || f.CovPct < 0 {
			continue
		}
		b, ok := was[f.Name]
		if !ok || b.CovPct < 0 {
			continue
		}
		if f.CovPct < b.CovPct-drop {
			out = append(out, CovRegression{Feature: f.Name, Base: b.CovPct, Head: f.CovPct, Stmts: f.Stmts})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if la, lb := a.Base-a.Head, b.Base-b.Head; la != lb {
			return la > lb
		}
		return a.Feature < b.Feature
	})
	return out
}

// coverageAdvisory reports and returns nothing: this mode never fails a build,
// so even a read error is a warning annotation rather than an exit code.
func coverageAdvisory(w io.Writer, featuresCSV, baselineCSV string, drop float64, minStmts int) {
	head, err := readFeatures(featuresCSV)
	if err != nil {
		warnUnreadable(w, err)
		return
	}
	baseline, err := readFeatures(baselineCSV)
	if err != nil {
		warnUnreadable(w, err)
		return
	}

	regressions := findCovRegressions(baseline, head, drop, minStmts)
	if len(regressions) == 0 {
		fmt.Fprintf(w, "coverage advisory: no ranked feature lost more than %.1f points against %s\n", drop, baselineCSV)
		return
	}

	for _, r := range regressions {
		msg := fmt.Sprintf("feature %s statement coverage %.1f%%, down %.1f points from %.1f%% at the baseline (%d statements)", r.Feature, r.Head, r.Base-r.Head, r.Base, r.Stmts)
		fmt.Fprintf(w, "::warning::%s\n", escapeData(msg))
	}

	var md strings.Builder
	fmt.Fprintf(&md, "### Coverage advisory\n\n")
	fmt.Fprintf(&md, "%d ranked feature(s) lost more than %.1f points of statement coverage against `%s`. This does not block the build: the baseline is a snapshot of one commit and drifts as the tree moves.\n\n", len(regressions), drop, baselineCSV)
	fmt.Fprintf(&md, "| feature | baseline | head | delta | statements |\n| --- | --: | --: | --: | --: |\n")
	for _, r := range regressions {
		fmt.Fprintf(&md, "| %s | %.1f%% | %.1f%% | -%.1f | %d |\n", r.Feature, r.Base, r.Head, r.Base-r.Head, r.Stmts)
	}
	fmt.Fprint(w, md.String())
	if err := appendStepSummary(md.String()); err != nil {
		warnUnreadable(w, err)
	}
}

func warnUnreadable(w io.Writer, err error) {
	fmt.Fprintf(w, "::warning::coverage advisory skipped: %s\n", escapeData(err.Error()))
}

// appendStepSummary appends to the GitHub step summary when running under
// Actions, and does nothing anywhere else.
func appendStepSummary(md string) error {
	p := os.Getenv("GITHUB_STEP_SUMMARY")
	if p == "" {
		return nil
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("step summary: %w", err)
	}
	if _, err := io.WriteString(f, "\n"+md); err != nil {
		f.Close()
		return fmt.Errorf("step summary: %w", err)
	}
	return f.Close()
}

// ---------- CSV reading ----------

// readCSV returns the rows of a CSV keyed by header name, so a column added or
// reordered upstream in cxtool does not silently shift the values this gate
// reads.
func readCSV(path string, want ...string) ([]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s: empty", path)
	}
	idx := map[string]int{}
	for i, h := range rows[0] {
		idx[h] = i
	}
	for _, h := range want {
		if _, ok := idx[h]; !ok {
			return nil, fmt.Errorf("%s: no %q column", path, h)
		}
	}
	out := make([]map[string]string, 0, len(rows)-1)
	for _, row := range rows[1:] {
		m := make(map[string]string, len(want))
		for _, h := range want {
			if i := idx[h]; i < len(row) {
				m[h] = row[i]
			}
		}
		out = append(out, m)
	}
	return out, nil
}

func readFuncs(dir string) ([]FuncMetric, error) {
	path := filepath.Join(dir, "functions.csv")
	rows, err := readCSV(path, "file", "func", "start", "cognit")
	if err != nil {
		return nil, err
	}
	out := make([]FuncMetric, 0, len(rows))
	for _, m := range rows {
		start, err := strconv.Atoi(m["start"])
		if err != nil {
			return nil, fmt.Errorf("%s: bad start %q: %w", path, m["start"], err)
		}
		cognit, err := strconv.Atoi(m["cognit"])
		if err != nil {
			return nil, fmt.Errorf("%s: bad cognit %q: %w", path, m["cognit"], err)
		}
		out = append(out, FuncMetric{File: m["file"], Name: m["func"], Start: start, Cognit: cognit})
	}
	return out, nil
}

func readFeatures(path string) ([]FeatureCoverage, error) {
	rows, err := readCSV(path, "name", "ranked", "stmts", "cov_pct")
	if err != nil {
		return nil, err
	}
	out := make([]FeatureCoverage, 0, len(rows))
	for _, m := range rows {
		stmts, err := strconv.Atoi(m["stmts"])
		if err != nil {
			return nil, fmt.Errorf("%s: bad stmts %q: %w", path, m["stmts"], err)
		}
		out = append(out, FeatureCoverage{
			Name:   m["name"],
			Ranked: m["ranked"] == "true",
			Stmts:  stmts,
			CovPct: parsePct(m["cov_pct"]),
		})
	}
	return out, nil
}

// parsePct mirrors cxtool's pct(), which writes "n/a" for a feature with no
// statements. Anything unparseable is treated the same way — as no figure —
// because the advisory's only response to a missing number is to skip.
func parsePct(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return -1
	}
	return v
}

// ---------- annotations ----------

// escapeData and escapeProperty apply GitHub's workflow-command escaping. Our
// own paths and identifiers do not currently need it; the encoding is applied
// anyway so a message that one day carries a percent sign or a newline cannot
// truncate or forge an annotation.
func escapeData(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	s = strings.ReplaceAll(s, "\r", "%0D")
	return strings.ReplaceAll(s, "\n", "%0A")
}

func escapeProperty(s string) string {
	s = escapeData(s)
	s = strings.ReplaceAll(s, ":", "%3A")
	return strings.ReplaceAll(s, ",", "%2C")
}
