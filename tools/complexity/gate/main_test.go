package main

import (
	"strconv"
	"strings"
	"testing"
)

func fn(file, name string, start, cognit int) FuncMetric {
	return FuncMetric{File: file, Name: name, Start: start, Cognit: cognit}
}

// TestFindOffenders pins the whole decision table: what the gate fires on, and
// — the half that decides whether anyone leaves it switched on — what it lets
// through.
func TestFindOffenders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  []FuncMetric
		head  []FuncMetric
		limit int
		// want is "file:func=headCognit/base", base being "new" for a
		// function absent from the base tree.
		want []string
	}{
		{
			name:  "new function over the limit fails",
			base:  nil,
			head:  []FuncMetric{fn("a.go", "F", 10, 31)},
			limit: 30,
			want:  []string{"a.go:F=31/new"},
		},
		{
			name:  "new function at the limit passes",
			head:  []FuncMetric{fn("a.go", "F", 10, 30)},
			limit: 30,
		},
		{
			name:  "pre-existing violation left alone passes",
			base:  []FuncMetric{fn("a.go", "F", 10, 80)},
			head:  []FuncMetric{fn("a.go", "F", 40, 80)},
			limit: 30,
		},
		{
			name:  "pre-existing violation that grew fails",
			base:  []FuncMetric{fn("a.go", "F", 10, 80)},
			head:  []FuncMetric{fn("a.go", "F", 10, 81)},
			limit: 30,
			want:  []string{"a.go:F=81/80"},
		},
		{
			name:  "pre-existing violation that shrank but stayed over passes",
			base:  []FuncMetric{fn("a.go", "F", 10, 80)},
			head:  []FuncMetric{fn("a.go", "F", 10, 79)},
			limit: 30,
		},
		{
			name:  "function pushed over the limit fails",
			base:  []FuncMetric{fn("a.go", "F", 10, 30)},
			head:  []FuncMetric{fn("a.go", "F", 10, 31)},
			limit: 30,
			want:  []string{"a.go:F=31/30"},
		},
		{
			name:  "growth that stays within the limit passes",
			base:  []FuncMetric{fn("a.go", "F", 10, 3)},
			head:  []FuncMetric{fn("a.go", "F", 10, 29)},
			limit: 30,
		},
		{
			name:  "same name in another file is a different function",
			base:  []FuncMetric{fn("a.go", "F", 10, 80)},
			head:  []FuncMetric{fn("b.go", "F", 10, 80)},
			limit: 30,
			want:  []string{"b.go:F=80/new"},
		},
		{
			name:  "methods are distinguished by receiver",
			base:  []FuncMetric{fn("a.go", "T.F", 10, 80), fn("a.go", "U.F", 90, 5)},
			head:  []FuncMetric{fn("a.go", "T.F", 10, 80), fn("a.go", "U.F", 90, 40)},
			limit: 30,
			want:  []string{"a.go:U.F=40/5"},
		},
		{
			name:  "a raised limit forgives what a lower one would catch",
			base:  []FuncMetric{fn("a.go", "F", 10, 30)},
			head:  []FuncMetric{fn("a.go", "F", 10, 45)},
			limit: 50,
		},
		{
			name: "worst first, then file, then line",
			head: []FuncMetric{
				fn("b.go", "Mid", 5, 40),
				fn("a.go", "Tie2", 90, 35),
				fn("a.go", "Tie1", 10, 35),
				fn("c.go", "Worst", 1, 99),
			},
			limit: 30,
			want:  []string{"c.go:Worst=99/new", "b.go:Mid=40/new", "a.go:Tie1=35/new", "a.go:Tie2=35/new"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := offenderKeys(findOffenders(tc.base, tc.head, tc.limit))
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("findOffenders = %v, want %v", got, tc.want)
			}
		})
	}
}

func offenderKeys(offs []Offender) []string {
	out := make([]string, 0, len(offs))
	for _, o := range offs {
		was := "new"
		if !o.IsNew {
			was = strconv.Itoa(o.Base)
		}
		out = append(out, o.File+":"+o.Name+"="+strconv.Itoa(o.Cognit)+"/"+was)
	}
	return out
}

func feat(name string, ranked bool, stmts int, cov float64) FeatureCoverage {
	return FeatureCoverage{Name: name, Ranked: ranked, Stmts: stmts, CovPct: cov}
}

// TestFindCovRegressions pins the advisory's skip rules, which are the whole
// reason it is advisory: everything it cannot compare fairly it declines to
// compare at all.
func TestFindCovRegressions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		baseline  []FeatureCoverage
		head      []FeatureCoverage
		drop      float64
		minStmts  int
		wantFeats []string
	}{
		{
			name:      "drop past the tolerance is reported",
			baseline:  []FeatureCoverage{feat("f", true, 500, 80)},
			head:      []FeatureCoverage{feat("f", true, 500, 77.9)},
			drop:      2,
			minStmts:  300,
			wantFeats: []string{"f"},
		},
		{
			name:     "drop inside the tolerance is not",
			baseline: []FeatureCoverage{feat("f", true, 500, 80)},
			head:     []FeatureCoverage{feat("f", true, 500, 78)},
			drop:     2,
			minStmts: 300,
		},
		{
			name:     "a gain is not",
			baseline: []FeatureCoverage{feat("f", true, 500, 80)},
			head:     []FeatureCoverage{feat("f", true, 500, 91)},
			drop:     2,
			minStmts: 300,
		},
		{
			name:     "unranked features are skipped",
			baseline: []FeatureCoverage{feat("test-infra", false, 500, 80)},
			head:     []FeatureCoverage{feat("test-infra", false, 500, 10)},
			drop:     2,
			minStmts: 300,
		},
		{
			name:     "features under min-stmts are skipped",
			baseline: []FeatureCoverage{feat("f", true, 500, 80)},
			head:     []FeatureCoverage{feat("f", true, 299, 10)},
			drop:     2,
			minStmts: 300,
		},
		{
			name:     "a feature absent from the baseline is skipped",
			baseline: nil,
			head:     []FeatureCoverage{feat("f", true, 500, 10)},
			drop:     2,
			minStmts: 300,
		},
		{
			name:     "a feature dropped from head is skipped",
			baseline: []FeatureCoverage{feat("f", true, 500, 80)},
			head:     nil,
			drop:     2,
			minStmts: 300,
		},
		{
			name:     "n/a on either side is skipped",
			baseline: []FeatureCoverage{feat("a", true, 500, -1), feat("b", true, 500, 80)},
			head:     []FeatureCoverage{feat("a", true, 500, 10), feat("b", true, 500, -1)},
			drop:     2,
			minStmts: 300,
		},
		{
			// b and d tie on a 10-point drop and are ordered by name; c's
			// 25 beats a's 20, so the head of the list is not alphabetical.
			name:      "biggest drop first, then by name",
			baseline:  []FeatureCoverage{feat("a", true, 500, 80), feat("b", true, 500, 80), feat("c", true, 500, 95), feat("d", true, 500, 80)},
			head:      []FeatureCoverage{feat("a", true, 500, 60), feat("b", true, 500, 70), feat("c", true, 500, 70), feat("d", true, 500, 70)},
			drop:      2,
			minStmts:  300,
			wantFeats: []string{"c", "a", "b", "d"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, r := range findCovRegressions(tc.baseline, tc.head, tc.drop, tc.minStmts) {
				got = append(got, r.Feature)
			}
			if strings.Join(got, " ") != strings.Join(tc.wantFeats, " ") {
				t.Fatalf("findCovRegressions = %v, want %v", got, tc.wantFeats)
			}
		})
	}
}

func TestParsePct(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]float64{"77.3": 77.3, "0.0": 0, "n/a": -1, "": -1, " 12.5 ": 12.5} {
		if got := parsePct(in); got != want {
			t.Errorf("parsePct(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestEscapeAnnotation guards the two encodings a workflow command needs; a
// raw newline in a message would otherwise end the annotation early and let
// the rest of the text be read as a fresh command.
func TestEscapeAnnotation(t *testing.T) {
	t.Parallel()
	if got, want := escapeData("100% done\nnext"), "100%25 done%0Anext"; got != want {
		t.Errorf("escapeData = %q, want %q", got, want)
	}
	if got, want := escapeProperty("a:b,c.go"), "a%3Ab%2Cc.go"; got != want {
		t.Errorf("escapeProperty = %q, want %q", got, want)
	}
}
