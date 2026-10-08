// cxtool computes a deterministic complexity/coverage/churn baseline for a Go
// repository and rolls it up by "feature" using a file→feature mapping.
//
//	cxtool -root . -features features.json -cover a.out,b.out -churn-days 90,180 -out out/
//
// Metrics per function: LOC, cyclomatic (gocyclo), cognitive (gocognit),
// statements + covered statements (from cover profiles, merged per block with
// the maximum count seen). Metrics per file: the above summed, plus churn.
// Metrics per feature: the above summed over files per the mapping owned by
// internal/cx. Generated files (ast.IsGenerated) are counted in LOC but
// excluded from complexity. Features whose rule sets norank:true are measured
// but excluded from ranking tables.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cxtool/internal/cx"

	"github.com/fzipp/gocyclo"
	"github.com/uudashr/gocognit"
)

// Warning thresholds; flag-settable so retuning them never renames a column.
var (
	cycloWarn  = flag.Int("cyclo-warn", 15, "count functions with cyclomatic complexity above this")
	cognitWarn = flag.Int("cognit-warn", 20, "count functions with cognitive complexity above this")
	covWarn    = flag.Float64("cov-warn", 50, "a function below this coverage %% counts as under-covered")
)

// ---------- data model ----------

type Func struct {
	File    string `json:"file"`
	Pkg     string `json:"pkg"`
	Name    string `json:"name"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
	LOC     int    `json:"loc"`
	Cyclo   int    `json:"cyclo"`
	Cognit  int    `json:"cognit"`
	Stmts   int    `json:"stmts"`
	Covered int    `json:"covered"`
	Feature string `json:"feature"`
	Area    string `json:"area"`
	Ranked  bool   `json:"ranked"`
}

func (f *Func) CovPct() float64 {
	if f.Stmts == 0 {
		return -1
	}
	return 100 * float64(f.Covered) / float64(f.Stmts)
}

type Churn struct {
	Commits int `json:"commits"`
	Added   int `json:"added"`
	Deleted int `json:"deleted"`
}

type File struct {
	Rel       string           `json:"rel"`
	PkgDir    string           `json:"pkg_dir"`
	IsTest    bool             `json:"is_test"`
	Generated bool             `json:"generated"`
	Ranked    bool             `json:"ranked"`
	LOC       int              `json:"loc"` // physical lines
	SumCyclo  int              `json:"sum_cyclo"`
	SumCognit int              `json:"sum_cognit"`
	MaxCognit int              `json:"max_cognit"`
	Stmts     int              `json:"stmts"`
	Covered   int              `json:"covered"`
	Churn     map[string]Churn `json:"churn"` // key: "90d"
	Feature   string           `json:"feature"`
	Area      string           `json:"area"`
	Funcs     []*Func          `json:"-"`
	Imports   []string         `json:"-"`
}

func (f *File) CovPct() float64 {
	if f.Stmts == 0 {
		return -1
	}
	return 100 * float64(f.Covered) / float64(f.Stmts)
}

type Agg struct {
	Name        string           `json:"name"`
	Area        string           `json:"area,omitempty"`
	Ranked      bool             `json:"ranked"`
	Files       int              `json:"files"`
	TestFiles   int              `json:"test_files"`
	ProdLOC     int              `json:"prod_loc"`
	TestLOC     int              `json:"test_loc"`
	GenLOC      int              `json:"gen_loc"`
	Funcs       int              `json:"funcs"`
	SumCyclo    int              `json:"sum_cyclo"`
	MaxCyclo    int              `json:"max_cyclo"`
	SumCognit   int              `json:"sum_cognit"`
	MaxCognit   int              `json:"max_cognit"`
	OverCyclo   int              `json:"funcs_over_cyclo"`
	OverCognit  int              `json:"funcs_over_cognit"`
	Stmts       int              `json:"stmts"`
	Covered     int              `json:"covered"`
	UncovCognit int              `json:"uncovered_cognit"` // cognitive complexity in under-covered functions
	Churn       map[string]Churn `json:"churn"`
	FanIn       int              `json:"fan_in,omitempty"`
	FanOut      int              `json:"fan_out,omitempty"`
	cognitVals  []int
}

func (a *Agg) CovPct() float64 {
	if a.Stmts == 0 {
		return -1
	}
	return 100 * float64(a.Covered) / float64(a.Stmts)
}

func (a *Agg) TestRatio() float64 {
	if a.ProdLOC == 0 {
		return 0
	}
	return float64(a.TestLOC) / float64(a.ProdLOC)
}

func (a *Agg) Density() float64 { // cognitive complexity per 100 prod LOC
	if a.ProdLOC == 0 {
		return 0
	}
	return 100 * float64(a.SumCognit) / float64(a.ProdLOC)
}

func (a *Agg) MedianCognit() float64 {
	if len(a.cognitVals) == 0 {
		return 0
	}
	v := append([]int(nil), a.cognitVals...)
	sort.Ints(v)
	n := len(v)
	if n%2 == 1 {
		return float64(v[n/2])
	}
	return float64(v[n/2-1]+v[n/2]) / 2
}

func (a *Agg) add(f *File, windows []string) {
	a.Ranked = a.Ranked || f.Ranked
	for _, w := range windows {
		c := a.Churn[w]
		fc := f.Churn[w]
		c.Commits += fc.Commits
		c.Added += fc.Added
		c.Deleted += fc.Deleted
		a.Churn[w] = c
	}
	if f.IsTest {
		a.TestFiles++
		a.TestLOC += f.LOC
		return
	}
	a.Files++
	if f.Generated {
		a.GenLOC += f.LOC
		return
	}
	a.ProdLOC += f.LOC
	a.Stmts += f.Stmts
	a.Covered += f.Covered
	for _, fn := range f.Funcs {
		a.Funcs++
		a.SumCyclo += fn.Cyclo
		a.SumCognit += fn.Cognit
		a.cognitVals = append(a.cognitVals, fn.Cognit)
		a.MaxCyclo = max(a.MaxCyclo, fn.Cyclo)
		a.MaxCognit = max(a.MaxCognit, fn.Cognit)
		if fn.Cyclo > *cycloWarn {
			a.OverCyclo++
		}
		if fn.Cognit > *cognitWarn {
			a.OverCognit++
		}
		if fn.Stmts > 0 && fn.CovPct() < *covWarn {
			a.UncovCognit += fn.Cognit
		}
	}
}

// ---------- main ----------

func main() {
	root := flag.String("root", ".", "repository root")
	featuresPath := flag.String("features", "features.json", "feature mapping JSON")
	coverList := flag.String("cover", "", "comma-separated cover profiles (merged per block, max count)")
	churnDays := flag.String("churn-days", "90,180", "comma-separated churn windows in days")
	outDir := flag.String("out", "out", "output directory")
	skipDirs := flag.String("skip", "node_modules", "extra dir names to skip (dot-dirs are always skipped)")
	flag.Parse()

	absRoot, err := filepath.Abs(*root)
	cx.Check(err)
	modPath, err := cx.ReadModulePath(absRoot)
	cx.Check(err)
	cfg, err := cx.LoadConfig(*featuresPath)
	cx.Check(err)

	files := parseTree(absRoot, modPath, cfg, cx.SplitList(*skipDirs))

	// Coverage and churn both only read the parsed file set and write disjoint
	// fields on it, so they overlap; each takes seconds on this repo (a ~370MB
	// profile to fold, a `git log --numstat` over 180 days) and a person is
	// waiting on the total.
	var stages sync.WaitGroup
	if *coverList != "" {
		stages.Add(1)
		go func() {
			defer stages.Done()
			mergeCoverage(cx.SplitList(*coverList), modPath, files)
		}()
	}
	var windows []string
	stages.Add(1)
	go func() {
		defer stages.Done()
		windows = applyChurn(absRoot, cx.SplitList(*churnDays), files)
	}()
	stages.Wait()

	// Aggregate: features, areas, packages, fan-in/out.
	features := map[string]*Agg{}
	areas := map[string]*Agg{}
	pkgs := map[string]*Agg{}
	pkgImports := map[string]map[string]bool{} // pkgDir -> set of imported pkgDirs
	for _, f := range files {
		fa := get(features, f.Feature)
		fa.Area = f.Area
		fa.add(f, windows)
		get(areas, f.Area).add(f, windows)
		get(pkgs, f.PkgDir).add(f, windows)
		if f.IsTest {
			continue
		}
		if pkgImports[f.PkgDir] == nil {
			pkgImports[f.PkgDir] = map[string]bool{}
		}
		for _, ip := range f.Imports {
			dep := strings.TrimPrefix(ip, modPath+"/")
			if dep == modPath {
				dep = "."
			}
			if dep == f.PkgDir {
				continue
			}
			pkgImports[f.PkgDir][dep] = true
		}
	}
	for p, deps := range pkgImports {
		get(pkgs, p).FanOut = len(deps)
		for d := range deps {
			if a := pkgs[d]; a != nil {
				a.FanIn++
			}
		}
	}

	// Outputs.
	cx.Check(os.MkdirAll(*outDir, 0o755))
	var allFuncs []*Func
	var fileList []*File
	for _, f := range files {
		fileList = append(fileList, f)
		if !f.IsTest && !f.Generated {
			allFuncs = append(allFuncs, f.Funcs...)
		}
	}
	sort.Slice(fileList, func(i, j int) bool { return fileList[i].Rel < fileList[j].Rel })
	sort.Slice(allFuncs, func(i, j int) bool {
		if allFuncs[i].Cognit != allFuncs[j].Cognit {
			return allFuncs[i].Cognit > allFuncs[j].Cognit
		}
		return allFuncs[i].File+allFuncs[i].Name < allFuncs[j].File+allFuncs[j].Name
	})
	featureAggs, areaAggs, pkgAggs := sortedAggs(features), sortedAggs(areas), sortedAggs(pkgs)

	cx.Check(writeFuncsCSV(filepath.Join(*outDir, "functions.csv"), allFuncs))
	cx.Check(writeAggCSV(filepath.Join(*outDir, "features.csv"), featureAggs, windows))
	cx.Check(writeAggCSV(filepath.Join(*outDir, "areas.csv"), areaAggs, windows))
	cx.Check(writeAggCSV(filepath.Join(*outDir, "packages.csv"), pkgAggs, windows))
	cx.Check(writeFilesCSV(filepath.Join(*outDir, "files.csv"), fileList, windows))
	writeMarkdown(filepath.Join(*outDir, "report.md"), areaAggs, featureAggs, pkgAggs, allFuncs, fileList, windows)

	// Unmapped files are a mapping gap; count them into the report and list
	// them on stderr so the mapping can be iterated.
	unmappedFiles, unmappedLOC := 0, 0
	for _, f := range fileList {
		if f.Feature == cx.Unmapped.Name {
			unmappedFiles++
			unmappedLOC += f.LOC
			fmt.Fprintf(os.Stderr, "unmapped: %s\n", f.Rel)
		}
	}

	out := map[string]any{
		"module":  modPath,
		"windows": windows,
		"meta": map[string]any{
			"cyclo_warn":     *cycloWarn,
			"cognit_warn":    *cognitWarn,
			"cov_warn":       *covWarn,
			"unmapped_files": unmappedFiles,
			"unmapped_loc":   unmappedLOC,
		},
		"areas":    areaAggs,
		"features": featureAggs,
		"packages": pkgAggs,
		"files":    fileList,
		"funcs":    allFuncs,
	}
	cx.Check(cx.WriteJSON(filepath.Join(*outDir, "report.json"), out))
	fmt.Fprintf(os.Stderr, "wrote %s/{report.md,report.json,functions.csv,features.csv,areas.csv,packages.csv,files.csv}; %d unmapped files\n", *outDir, unmappedFiles)
}

// ---------- parse ----------

func parseTree(absRoot, modPath string, cfg *cx.Config, skip []string) map[string]*File {
	skipSet := map[string]bool{}
	for _, s := range skip {
		skipSet[s] = true
	}
	var paths []string
	cx.Check(filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != absRoot {
				if skipSet[d.Name()] || strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				// A nested go.mod starts a different module; this analysis is
				// per-module (reach's packages.Load never descends either).
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") {
			paths = append(paths, p)
		}
		return nil
	}))

	// token.FileSet is documented mutex-guarded; parsing is CPU-bound and
	// embarrassingly parallel across files.
	fset := token.NewFileSet()
	files := make(map[string]*File, len(paths))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for _, p := range paths {
		wg.Add(1)
		sem <- struct{}{}
		go func(p string) {
			defer wg.Done()
			defer func() { <-sem }()
			f := parseFile(fset, absRoot, modPath, cfg, p)
			if f != nil {
				mu.Lock()
				files[f.Rel] = f
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	return files
}

func parseFile(fset *token.FileSet, absRoot, modPath string, cfg *cx.Config, p string) *File {
	rel, err := cx.RelSlash(absRoot, p)
	cx.Check(err)
	src, err := os.ReadFile(p)
	cx.Check(err)
	af, err := parser.ParseFile(fset, p, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", rel, err)
		return nil
	}
	feat := cfg.For(rel)
	f := &File{
		Rel:       rel,
		PkgDir:    path.Dir(rel),
		IsTest:    strings.HasSuffix(rel, "_test.go"),
		Generated: ast.IsGenerated(af),
		Ranked:    feat.Ranked,
		LOC:       bytes.Count(src, []byte("\n")),
		Churn:     map[string]Churn{},
		Feature:   feat.Name,
		Area:      feat.Area,
	}
	for _, imp := range af.Imports {
		ip, _ := strconv.Unquote(imp.Path.Value)
		if strings.HasPrefix(ip, modPath+"/") || ip == modPath {
			f.Imports = append(f.Imports, ip)
		}
	}
	if f.IsTest {
		return f
	}
	for _, decl := range af.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Pos()).Line
		end := fset.Position(fn.End()).Line
		f.Funcs = append(f.Funcs, &Func{
			File:    rel,
			Pkg:     af.Name.Name,
			Name:    funcName(fn),
			Start:   start,
			End:     end,
			LOC:     end - start + 1,
			Cyclo:   gocyclo.Complexity(fn),
			Cognit:  gocognit.Complexity(fn),
			Feature: f.Feature,
			Area:    f.Area,
			Ranked:  f.Ranked,
		})
	}
	sort.Slice(f.Funcs, func(i, j int) bool { return f.Funcs[i].Start < f.Funcs[j].Start })
	for _, fn := range f.Funcs {
		f.SumCyclo += fn.Cyclo
		f.SumCognit += fn.Cognit
		f.MaxCognit = max(f.MaxCognit, fn.Cognit)
	}
	return f
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := strings.TrimPrefix(types.ExprString(fn.Recv.List[0].Type), "*")
	return recv + "." + fn.Name.Name
}

// ---------- coverage ----------

type covKey struct{ sl, sc, el, ec, n int }

// mergeCoverage folds cover profiles straight into the files, line by line:
// no materialized block slice, no sort (x/tools/cover.ParseProfiles builds and
// sorts every block of every profile — ~4.8M with -coverpkg=./... — only for
// this caller to keep one int per unique block). Duplicate blocks within and
// across profiles keep the maximum count, which matches set-mode OR and is the
// deliberate cross-suite semantic for count mode.
func mergeCoverage(profiles []string, modPath string, files map[string]*File) {
	merged := map[*File]map[covKey]int{}
	missing := map[string]bool{}
	prefix := modPath + "/"
	for _, cp := range profiles {
		cp = strings.TrimSpace(cp)
		if cp == "" {
			continue
		}
		fh, err := os.Open(cp)
		cx.Check(err)
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		byName := map[string]*File{} // per-profile cache incl. misses
		first := true
		for sc.Scan() {
			line := sc.Bytes()
			if first {
				first = false
				if !bytes.HasPrefix(line, []byte("mode: ")) {
					cx.Check(fmt.Errorf("%s: missing mode line", cp))
				}
				continue
			}
			name, k, count, ok := parseCoverLine(line)
			if !ok {
				cx.Check(fmt.Errorf("%s: bad line %q", cp, line))
			}
			f, seen := byName[name]
			if !seen {
				f = files[strings.TrimPrefix(name, prefix)]
				byName[name] = f
				if f == nil {
					missing[name] = true
				}
			}
			if f == nil {
				continue
			}
			m := merged[f]
			if m == nil {
				m = map[covKey]int{}
				merged[f] = m
			}
			if old, ok := m[k]; !ok || count > old {
				m[k] = count
			}
		}
		cx.Check(sc.Err())
		cx.Check(fh.Close())
	}
	for f, m := range merged {
		for k, cnt := range m {
			f.Stmts += k.n
			if cnt > 0 {
				f.Covered += k.n
			}
			// Attribute to the enclosing function by start line.
			i := sort.Search(len(f.Funcs), func(i int) bool { return f.Funcs[i].Start > k.sl }) - 1
			if i >= 0 && f.Funcs[i].End >= k.sl {
				f.Funcs[i].Stmts += k.n
				if cnt > 0 {
					f.Funcs[i].Covered += k.n
				}
			}
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "coverage: %d profile files not found in tree\n", len(missing))
	}
}

// parseCoverLine parses "name.go:sl.sc,el.ec numStmts count".
func parseCoverLine(line []byte) (name string, k covKey, count int, ok bool) {
	sp2 := bytes.LastIndexByte(line, ' ')
	if sp2 < 0 {
		return
	}
	sp1 := bytes.LastIndexByte(line[:sp2], ' ')
	if sp1 < 0 {
		return
	}
	colon := bytes.LastIndexByte(line[:sp1], ':')
	if colon < 0 {
		return
	}
	var e1, e2, e3 bool
	count, e1 = atoi(line[sp2+1:])
	k.n, e2 = atoi(line[sp1+1 : sp2])
	pos := line[colon+1 : sp1]
	comma := bytes.IndexByte(pos, ',')
	if comma < 0 {
		return
	}
	k.sl, k.sc, e3 = dotPair(pos[:comma])
	el, ec, e4 := dotPair(pos[comma+1:])
	k.el, k.ec = el, ec
	return string(line[:colon]), k, count, e1 && e2 && e3 && e4
}

func dotPair(b []byte) (int, int, bool) {
	dot := bytes.IndexByte(b, '.')
	if dot < 0 {
		return 0, 0, false
	}
	a, ok1 := atoi(b[:dot])
	c, ok2 := atoi(b[dot+1:])
	return a, c, ok1 && ok2
}

func atoi(b []byte) (int, bool) {
	n := 0
	if len(b) == 0 {
		return 0, false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// ---------- churn ----------

// applyChurn runs one git log over the widest window and buckets each commit
// into every window it falls in (the windows are strictly nested; running git
// once per window recomputes the same diffs). Renames are not followed.
func applyChurn(root string, days []string, files map[string]*File) []string {
	type window struct {
		label  string
		cutoff time.Time
	}
	var windows []window
	maxDays := 0
	for _, d := range days {
		n, err := strconv.Atoi(d)
		cx.Check(err)
		windows = append(windows, window{d + "d", time.Now().AddDate(0, 0, -n)})
		maxDays = max(maxDays, n)
	}
	if maxDays == 0 {
		return nil
	}
	out, err := exec.Command("git", "-C", root, "log", fmt.Sprintf("--since=%d days ago", maxDays),
		"--numstat", "--no-renames", "--format=%H %ct", "--", "*.go").Output()
	cx.Check(err)

	var commitTime time.Time
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		l := sc.Text()
		if l == "" {
			continue
		}
		parts := strings.Split(l, "\t")
		if len(parts) == 1 { // "<hash> <unix-time>" commit line
			if _, ts, ok := strings.Cut(l, " "); ok {
				sec, err := strconv.ParseInt(ts, 10, 64)
				cx.Check(err)
				commitTime = time.Unix(sec, 0)
			}
			continue
		}
		if len(parts) != 3 {
			continue
		}
		f := files[parts[2]]
		if f == nil {
			continue // deleted, renamed away, or filtered
		}
		add, _ := strconv.Atoi(parts[0]) // "-" for binary → 0
		del, _ := strconv.Atoi(parts[1])
		for _, w := range windows {
			if commitTime.Before(w.cutoff) {
				continue
			}
			c := f.Churn[w.label]
			c.Commits++ // git lists each path at most once per commit
			c.Added += add
			c.Deleted += del
			f.Churn[w.label] = c
		}
	}
	labels := make([]string, len(windows))
	for i, w := range windows {
		labels[i] = w.label
	}
	return labels
}

// ---------- helpers ----------

func get(m map[string]*Agg, k string) *Agg {
	a := m[k]
	if a == nil {
		a = &Agg{Name: k, Churn: map[string]Churn{}}
		m[k] = a
	}
	return a
}

func sortedAggs(m map[string]*Agg) []*Agg {
	var out []*Agg
	for _, a := range m {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SumCognit != out[j].SumCognit {
			return out[i].SumCognit > out[j].SumCognit
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func pct(v float64) string {
	if v < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", v)
}

func itoa(i int) string { return strconv.Itoa(i) }

// ---------- writers ----------

func writeFuncsCSV(p string, fns []*Func) error {
	rows := make([][]string, 0, len(fns))
	for _, fn := range fns {
		rows = append(rows, []string{fn.Feature, fn.Area, fn.File, fn.Pkg, fn.Name, itoa(fn.Start), itoa(fn.LOC), itoa(fn.Cyclo), itoa(fn.Cognit), itoa(fn.Stmts), itoa(fn.Covered), pct(fn.CovPct())})
	}
	return cx.WriteCSV(p, []string{"feature", "area", "file", "pkg", "func", "start", "loc", "cyclo", "cognit", "stmts", "covered", "cov_pct"}, rows)
}

// churnHeader and churnCols keep the per-window column shape in one place; the
// agg and file writers must agree on it or their CSVs drift apart.
func churnHeader(windows []string) []string {
	hdr := make([]string, 0, 2*len(windows))
	for _, w := range windows {
		hdr = append(hdr, "commits_"+w, "lines_changed_"+w)
	}
	return hdr
}

func churnCols(churn map[string]Churn, windows []string) []string {
	cols := make([]string, 0, 2*len(windows))
	for _, w := range windows {
		c := churn[w]
		cols = append(cols, itoa(c.Commits), itoa(c.Added+c.Deleted))
	}
	return cols
}

func writeAggCSV(p string, aggs []*Agg, windows []string) error {
	hdr := []string{"name", "area", "ranked", "files", "test_files", "prod_loc", "test_loc", "gen_loc", "test_ratio", "funcs", "sum_cyclo", "max_cyclo", "sum_cognit", "max_cognit", "median_cognit", "cognit_per_100loc", "funcs_over_cyclo", "funcs_over_cognit", "stmts", "covered", "cov_pct", "uncovered_cognit"}
	hdr = append(hdr, churnHeader(windows)...)
	hdr = append(hdr, "fan_in", "fan_out")
	rows := make([][]string, 0, len(aggs))
	for _, a := range aggs {
		row := []string{a.Name, a.Area, strconv.FormatBool(a.Ranked), itoa(a.Files), itoa(a.TestFiles), itoa(a.ProdLOC), itoa(a.TestLOC), itoa(a.GenLOC), fmt.Sprintf("%.2f", a.TestRatio()), itoa(a.Funcs), itoa(a.SumCyclo), itoa(a.MaxCyclo), itoa(a.SumCognit), itoa(a.MaxCognit), fmt.Sprintf("%.1f", a.MedianCognit()), fmt.Sprintf("%.1f", a.Density()), itoa(a.OverCyclo), itoa(a.OverCognit), itoa(a.Stmts), itoa(a.Covered), pct(a.CovPct()), itoa(a.UncovCognit)}
		row = append(row, churnCols(a.Churn, windows)...)
		row = append(row, itoa(a.FanIn), itoa(a.FanOut))
		rows = append(rows, row)
	}
	return cx.WriteCSV(p, hdr, rows)
}

func writeFilesCSV(p string, files []*File, windows []string) error {
	hdr := []string{"feature", "area", "ranked", "file", "is_test", "generated", "loc", "funcs", "sum_cyclo", "sum_cognit", "max_cognit", "stmts", "covered", "cov_pct"}
	hdr = append(hdr, churnHeader(windows)...)
	rows := make([][]string, 0, len(files))
	for _, fl := range files {
		row := []string{fl.Feature, fl.Area, strconv.FormatBool(fl.Ranked), fl.Rel, strconv.FormatBool(fl.IsTest), strconv.FormatBool(fl.Generated), itoa(fl.LOC), itoa(len(fl.Funcs)), itoa(fl.SumCyclo), itoa(fl.SumCognit), itoa(fl.MaxCognit), itoa(fl.Stmts), itoa(fl.Covered), pct(fl.CovPct())}
		row = append(row, churnCols(fl.Churn, windows)...)
		rows = append(rows, row)
	}
	return cx.WriteCSV(p, hdr, rows)
}

func writeMarkdown(p string, areas, features, pkgs []*Agg, fns []*Func, files []*File, windows []string) {
	var b strings.Builder
	w0 := ""
	if len(windows) > 0 {
		w0 = windows[0]
	}
	fmt.Fprintf(&b, "# Complexity baseline\n\nRanking tables exclude features marked norank in the mapping (test infrastructure, generated code).\n\n")
	// The caption above is the contract, so the tables have to honour it rather
	// than filtering on a hardcoded area name and letting the rest through.
	// Excluded features are still measured, so they are named with their size
	// instead of vanishing from the report altogether.
	if excl := excludedNote(features); excl != "" {
		fmt.Fprintf(&b, "%s\n\n", excl)
	}
	fmt.Fprintf(&b, "## By area\n\n")
	aggTable(&b, rankedAggs(areas), w0, false)
	fmt.Fprintf(&b, "\n## By feature\n\n")
	aggTable(&b, rankedAggs(features), w0, false)
	fmt.Fprintf(&b, "\n## By package (top 40 by cognitive complexity)\n\n")
	if len(pkgs) > 40 {
		pkgs = pkgs[:40]
	}
	aggTable(&b, pkgs, w0, true)

	ranked := make([]*Func, 0, len(fns))
	for _, fn := range fns {
		if fn.Ranked {
			ranked = append(ranked, fn)
		}
	}
	funcTable(&b, "Top functions by cognitive complexity", ranked, func(*Func) bool { return true }, 40)
	funcTable(&b, fmt.Sprintf("Complex and under-covered (cognit ≥ %d, coverage < %.0f%%)", *cognitWarn, *covWarn), ranked,
		func(fn *Func) bool { return fn.Cognit >= *cognitWarn && fn.Stmts > 0 && fn.CovPct() < *covWarn }, 60)

	// File hotspots: churn × complexity.
	type hs struct {
		f *File
		c int
	}
	var hot []hs
	for _, f := range files {
		if f.IsTest || f.Generated || !f.Ranked {
			continue
		}
		if c := f.Churn[w0].Commits; c > 0 && f.SumCognit > 0 {
			hot = append(hot, hs{f, c})
		}
	}
	sort.Slice(hot, func(i, j int) bool { return hot[i].c*hot[i].f.SumCognit > hot[j].c*hot[j].f.SumCognit })
	fmt.Fprintf(&b, "\n## File hotspots: commits(%s) × cognitive complexity (top 40)\n\n", w0)
	fmt.Fprintf(&b, "| commits | sum cognit | loc | cov%% | feature | file |\n|---:|---:|---:|---:|---|---|\n")
	for i, h := range hot {
		if i >= 40 {
			break
		}
		fmt.Fprintf(&b, "| %d | %d | %d | %s | %s | `%s` |\n", h.c, h.f.SumCognit, h.f.LOC, pct(h.f.CovPct()), h.f.Feature, h.f.Rel)
	}
	cx.Check(os.WriteFile(p, []byte(b.String()), 0o644))
}

// rankedAggs drops the norank rows from a ranking table.
func rankedAggs(aggs []*Agg) []*Agg {
	out := make([]*Agg, 0, len(aggs))
	for _, a := range aggs {
		if a.Ranked {
			out = append(out, a)
		}
	}
	return out
}

// excludedNote names the norank features and their size, so excluding them from
// the rankings does not also hide that they exist.
func excludedNote(features []*Agg) string {
	var parts []string
	for _, a := range features {
		if !a.Ranked && a.ProdLOC > 0 {
			parts = append(parts, fmt.Sprintf("%s (%d prod LOC)", a.Name, a.ProdLOC))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return "Excluded (norank), still measured in the CSVs: " + strings.Join(parts, ", ") + "."
}

func funcTable(b *strings.Builder, title string, fns []*Func, keep func(*Func) bool, limit int) {
	fmt.Fprintf(b, "\n## %s\n\n| cognit | cyclo | loc | cov%% | feature | function |\n|---:|---:|---:|---:|---|---|\n", title)
	n := 0
	for _, fn := range fns {
		if !keep(fn) {
			continue
		}
		fmt.Fprintf(b, "| %d | %d | %d | %s | %s | `%s:%d` %s |\n", fn.Cognit, fn.Cyclo, fn.LOC, pct(fn.CovPct()), fn.Feature, fn.File, fn.Start, fn.Name)
		if n++; n >= limit {
			break
		}
	}
}

func aggTable(b *strings.Builder, aggs []*Agg, w0 string, withFan bool) {
	fmt.Fprintf(b, "| name | area | prod LOC | test LOC | ratio | funcs | Σcognit | cog/100loc | max cog | >%d cog | cov%% | uncov cog | commits %s", *cognitWarn, w0)
	if withFan {
		fmt.Fprintf(b, " | in | out")
	}
	fmt.Fprintf(b, " |\n|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:")
	if withFan {
		fmt.Fprintf(b, "|---:|---:")
	}
	fmt.Fprintf(b, "|\n")
	for _, a := range aggs {
		fmt.Fprintf(b, "| %s | %s | %d | %d | %.2f | %d | %d | %.1f | %d | %d | %s | %d | %d", a.Name, a.Area, a.ProdLOC, a.TestLOC, a.TestRatio(), a.Funcs, a.SumCognit, a.Density(), a.MaxCognit, a.OverCognit, pct(a.CovPct()), a.UncovCognit, a.Churn[w0].Commits)
		if withFan {
			fmt.Fprintf(b, " | %d | %d", a.FanIn, a.FanOut)
		}
		fmt.Fprintf(b, " |\n")
	}
}
