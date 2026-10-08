// reach computes, for every cobra command constructor in the module, the set of
// module functions reachable from it (call graph + closures + function values),
// and splits that into EXCLUSIVE (reached by no other root) vs SHARED.
//
//	reach -root . -features features.json -out out/reach [-algo vta|cha] [-who substr]
//
// Roots:
//   - every function whose result type is *cobra.Command (one root each);
//   - one pseudo-root "<init>" for all package initializers (var initializers,
//     init() functions) — code that runs at program start belongs to no command;
//   - one pseudo-root "<main:pkg>" per main package.
//
// Traversal stops at other cobra roots, so a group constructor's set is its own
// glue only, and a leaf's set is its own work. Metrics are attributed to
// top-level FuncDecls (closures roll up to their enclosing declaration, which is
// also how gocognit counts them; generic instantiations roll up to their
// origin). Test files are excluded, so "unreached" means "nothing in the
// shipped binaries references this" (modulo reflection).
//
// Precision policies, in one place because they are the tool:
//   - VTA falls back to "every function with this signature" for dynamic calls
//     it cannot resolve; call sites resolving to more than -maxfanout callees
//     are treated as unresolved noise and dropped.
//   - A closure can only run if its creator ran: an anonymous function is never
//     entered through a call edge unless its declaration was already visited
//     (its parent's expansion enqueues it legitimately).
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"cxtool/internal/cx"

	"github.com/uudashr/gocognit"
	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

type decl struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	LOC    int    `json:"loc"`
	Cognit int    `json:"cognit"`
	Feat   string `json:"feature"`
	Area   string `json:"area"`
	Ranked bool   `json:"ranked"`
}

type cmdResult struct {
	Root          string         `json:"root"`
	Kind          string         `json:"kind"` // cobra | init | main
	File          string         `json:"file"`
	Feature       string         `json:"feature"`
	ReachDecls    int            `json:"reach_decls"`
	ReachLOC      int            `json:"reach_loc"`
	ReachCognit   int            `json:"reach_cognit"`
	ExclDecls     int            `json:"excl_decls"`
	ExclLOC       int            `json:"excl_loc"`
	ExclCognit    int            `json:"excl_cognit"`
	ExclFeatures  map[string]int `json:"excl_features_loc"`
	ReachFeatures map[string]int `json:"reach_features_loc"`
}

type root struct {
	label string
	kind  string
	fns   []*ssa.Function // entry functions (several for <init>)
	decl  *decl           // for cobra roots
}

// analysis holds the immutable per-program state the traversal reads.
type analysis struct {
	cg      *callgraph.Graph
	declOf  map[*ssa.Function]*decl
	topDecl map[*ssa.Function]*ssa.Function // parents + generic origins, memoized
	succs   map[*ssa.Function][]*ssa.Function
	siteFan map[ssa.CallInstruction]int // call-site fan-out, shared by the cutoff and -who
}

func main() {
	rootDir := flag.String("root", ".", "module root")
	featuresPath := flag.String("features", "features.json", "feature mapping")
	outDir := flag.String("out", "out/reach", "output dir")
	algo := flag.String("algo", "vta", "callgraph algorithm: vta|cha")
	who := flag.String("who", "", "debug: print owners and callers of decls whose key contains this substring")
	maxFan := flag.Int("maxfanout", 60, "drop dynamic call sites resolving to more callees than this (VTA fallback noise)")
	trims := flag.String("trimprefix", "cmd/entire/cli.", "comma-separated module-relative prefixes trimmed from displayed symbol keys")
	flag.Parse()

	absRoot, err := filepath.Abs(*rootDir)
	cx.Check(err)
	cfg, err := cx.LoadConfig(*featuresPath)
	cx.Check(err)
	modPath, err := cx.ReadModulePath(absRoot)
	cx.Check(err)
	short := shortener(modPath, cx.SplitList(*trims))

	t0 := time.Now()
	pkgs, err := packages.Load(&packages.Config{Mode: packages.LoadAllSyntax, Dir: absRoot}, "./...")
	cx.Check(err)
	if packages.PrintErrors(pkgs) > 0 {
		fmt.Fprintln(os.Stderr, "warning: some packages had errors; continuing")
	}
	fmt.Fprintf(os.Stderr, "loaded %d packages in %s\n", len(pkgs), time.Since(t0).Round(time.Millisecond))

	t0 = time.Now()
	prog, ssaPkgs := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	fmt.Fprintf(os.Stderr, "built SSA in %s\n", time.Since(t0).Round(time.Millisecond))

	allFuncs := ssautil.AllFunctions(prog)
	t0 = time.Now()
	var cg *callgraph.Graph
	if *algo == "cha" {
		cg = cha.CallGraph(prog)
	} else {
		cg = vta.CallGraph(allFuncs, nil)
	}
	fmt.Fprintf(os.Stderr, "callgraph (%s) with %d nodes in %s\n", *algo, len(cg.Nodes), time.Since(t0).Round(time.Millisecond))

	a := &analysis{cg: cg, declOf: map[*ssa.Function]*decl{}, topDecl: map[*ssa.Function]*ssa.Function{}, succs: map[*ssa.Function][]*ssa.Function{}}
	fset := prog.Fset
	isModule := func(fn *ssa.Function) bool {
		return fn.Pkg != nil && strings.HasPrefix(fn.Pkg.Pkg.Path(), modPath)
	}

	// Index top-level, non-test decls with metrics.
	for fn := range allFuncs {
		if !isModule(fn) || fn.Parent() != nil || fn.Synthetic != "" {
			continue
		}
		fd, ok := fn.Syntax().(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		pos := fset.Position(fd.Pos())
		rel, _ := cx.RelSlash(absRoot, pos.Filename)
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		end := fset.Position(fd.End()).Line
		feat := cfg.For(rel)
		a.declOf[fn] = &decl{
			Key: short(fn.String()), Name: fd.Name.Name, File: rel, Line: pos.Line,
			LOC: end - pos.Line + 1, Cognit: gocognit.Complexity(fd),
			Feat: feat.Name, Area: feat.Area, Ranked: feat.Ranked,
		}
	}

	// Roots.
	var roots []*root
	cobraRoot := map[*ssa.Function]bool{}
	for fn, d := range a.declOf {
		sig := fn.Signature
		if sig.Results().Len() == 1 && isCobraCmdPtr(sig.Results().At(0).Type()) {
			cobraRoot[fn] = true
			roots = append(roots, &root{label: d.Key, kind: "cobra", fns: []*ssa.Function{fn}, decl: d})
		}
	}
	initRoot := &root{label: "<init>", kind: "init"}
	for _, p := range ssaPkgs {
		if p == nil || !strings.HasPrefix(p.Pkg.Path(), modPath) {
			continue
		}
		if f := p.Func("init"); f != nil {
			initRoot.fns = append(initRoot.fns, f)
		}
		if p.Pkg.Name() == "main" {
			if f := p.Func("main"); f != nil {
				roots = append(roots, &root{label: "<main:" + strings.TrimPrefix(p.Pkg.Path(), modPath+"/") + ">", kind: "main", fns: []*ssa.Function{f}})
			}
		}
	}
	roots = append(roots, initRoot)
	fmt.Fprintf(os.Stderr, "%d module decls, %d roots (%d cobra)\n", len(a.declOf), len(roots), len(cobraRoot))

	a.buildSuccs(allFuncs, *maxFan)

	// BFS per root over the precomputed graph. Never enter another cobra root
	// (or its closures) — its cost is its own.
	reach := map[*root]map[*ssa.Function]bool{}
	for _, r := range roots {
		seen := map[*ssa.Function]bool{}
		queue := []*ssa.Function{}
		for _, f := range r.fns {
			seen[f] = true
			queue = append(queue, f)
		}
		decls := map[*ssa.Function]bool{}
		for len(queue) > 0 {
			f := queue[0]
			queue = queue[1:]
			if td := a.top(f); a.declOf[td] != nil {
				decls[td] = true
				seen[td] = true // lets closures of generic instances be admitted via their origin
			}
			for _, s := range a.succs[f] {
				if seen[s] {
					continue
				}
				td := a.top(s)
				if cobraRoot[td] && (r.kind != "cobra" || td != r.fns[0]) {
					continue
				}
				// A closure can only run if its creator ran.
				if s.Parent() != nil && !seen[td] {
					continue
				}
				seen[s] = true
				queue = append(queue, s)
			}
		}
		reach[r] = decls
	}

	owners := map[*ssa.Function][]*root{}
	for r, set := range reach {
		for d := range set {
			owners[d] = append(owners[d], r)
		}
	}

	if *who != "" {
		a.explainWho(*who, owners, fset, absRoot)
	}

	var results []*cmdResult
	for r, set := range reach {
		res := &cmdResult{Root: r.label, Kind: r.kind, ExclFeatures: map[string]int{}, ReachFeatures: map[string]int{}}
		if r.decl != nil {
			res.File = r.decl.File
			res.Feature = r.decl.Feat
		}
		for d := range set {
			m := a.declOf[d]
			res.ReachDecls++
			res.ReachLOC += m.LOC
			res.ReachCognit += m.Cognit
			res.ReachFeatures[m.Feat] += m.LOC
			if len(owners[d]) == 1 {
				res.ExclDecls++
				res.ExclLOC += m.LOC
				res.ExclCognit += m.Cognit
				res.ExclFeatures[m.Feat] += m.LOC
			}
		}
		results = append(results, res)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].ExclLOC != results[j].ExclLOC {
			return results[i].ExclLOC > results[j].ExclLOC
		}
		return results[i].Root < results[j].Root
	})

	var unreached []*decl
	for fn, d := range a.declOf {
		if len(owners[fn]) == 0 && !cobraRoot[fn] {
			unreached = append(unreached, d)
		}
	}
	sort.Slice(unreached, func(i, j int) bool {
		if unreached[i].File != unreached[j].File {
			return unreached[i].File < unreached[j].File
		}
		return unreached[i].Line < unreached[j].Line
	})

	hist := map[int]int{}
	histLOC := map[int]int{}
	for fn, d := range a.declOf {
		if cobraRoot[fn] {
			continue
		}
		hist[len(owners[fn])]++
		histLOC[len(owners[fn])] += d.LOC
	}

	cx.Check(os.MkdirAll(*outDir, 0o755))
	rows := make([][]string, 0, len(results))
	for _, r := range results {
		rows = append(rows, []string{r.Root, r.Kind, r.File, r.Feature, itoa(r.ReachDecls), itoa(r.ReachLOC), itoa(r.ReachCognit), itoa(r.ExclDecls), itoa(r.ExclLOC), itoa(r.ExclCognit), topFeatures(r.ExclFeatures, 3)})
	}
	cx.Check(cx.WriteCSV(filepath.Join(*outDir, "commands.csv"),
		[]string{"root", "kind", "file", "feature", "reach_decls", "reach_loc", "reach_cognit", "excl_decls", "excl_loc", "excl_cognit", "top_excl_features"}, rows))

	out := map[string]any{
		"module": modPath, "algo": *algo,
		"meta":  map[string]any{"maxfanout": *maxFan, "trimprefix": *trims},
		"roots": len(roots), "cobra_roots": len(cobraRoot), "decls": len(a.declOf),
		"commands": results, "owner_hist": hist, "owner_histloc": histLOC, "unreached": unreached,
	}
	cx.Check(cx.WriteJSON(filepath.Join(*outDir, "reach.json"), out))

	writeMarkdown(filepath.Join(*outDir, "reach.md"), *algo, len(roots), len(cobraRoot), len(a.declOf), hist, histLOC, results, unreached)
	fmt.Fprintf(os.Stderr, "wrote %s/{reach.md,reach.json,commands.csv}\n", *outDir)
}

// top returns the top-level declaration a function belongs to: closures roll
// up through Parent, generic instantiations to their Origin. Memoized — it is
// consulted for every edge of every root's traversal.
func (a *analysis) top(fn *ssa.Function) *ssa.Function {
	if td, ok := a.topDecl[fn]; ok {
		return td
	}
	td := fn
	for td.Parent() != nil {
		td = td.Parent()
	}
	if o := td.Origin(); o != nil {
		td = o
	}
	a.topDecl[fn] = td
	return td
}

// buildSuccs precomputes each function's successors once: callgraph callees
// (with the fan-out cutoff applied to dynamic call sites), *ssa.Function
// operands (function values stored into fields, slices, maps), and nested
// closures. The traversal itself then only filters per root.
func (a *analysis) buildSuccs(allFuncs map[*ssa.Function]bool, maxFan int) {
	a.siteFan = map[ssa.CallInstruction]int{}
	for _, n := range a.cg.Nodes {
		for _, e := range n.Out {
			if e.Site != nil {
				a.siteFan[e.Site]++
			}
		}
	}
	var ops []*ssa.Value
	for fn := range allFuncs {
		var out []*ssa.Function
		if n := a.cg.Nodes[fn]; n != nil {
			for _, e := range n.Out {
				if e.Site != nil && a.siteFan[e.Site] > maxFan {
					if cc := e.Site.Common(); cc != nil && cc.StaticCallee() == nil {
						continue
					}
				}
				out = append(out, e.Callee.Func)
			}
		}
		for _, b := range fn.Blocks {
			for _, ins := range b.Instrs {
				ops = ins.Operands(ops[:0])
				for _, op := range ops {
					if op == nil {
						continue
					}
					if f, ok := (*op).(*ssa.Function); ok {
						out = append(out, f)
					}
				}
			}
		}
		out = append(out, fn.AnonFuncs...)
		if len(out) > 0 {
			a.succs[fn] = out
		}
	}
}

// explainWho prints, for every decl whose key contains substr, its owners and
// its incoming call edges, reading each site's fan-out from a.siteFan — the
// same map the cutoff uses, so the diagnostic cannot describe a different
// filter than the one that ran.
func (a *analysis) explainWho(substr string, owners map[*ssa.Function][]*root, fset *token.FileSet, absRoot string) {
	for fn, d := range a.declOf {
		if !strings.Contains(d.Key, substr) {
			continue
		}
		var labels []string
		for _, r := range owners[fn] {
			labels = append(labels, r.label)
		}
		sort.Strings(labels)
		fmt.Fprintf(os.Stderr, "WHO %s:%d %s -> %d owners: %s\n", d.File, d.Line, d.Key, len(labels), strings.Join(labels, ", "))
		n := a.cg.Nodes[fn]
		if n == nil {
			continue
		}
		for _, e := range n.In {
			site, kind := "?", "static"
			if e.Site != nil {
				site = strings.TrimPrefix(fset.Position(e.Site.Pos()).String(), absRoot+"/")
				if cc := e.Site.Common(); cc != nil {
					if cc.IsInvoke() {
						kind = "invoke"
					} else if cc.StaticCallee() == nil {
						kind = "dynamic"
					}
				}
			}
			fmt.Fprintf(os.Stderr, "    <- %s [%s, site fan-out %d] at %s\n", e.Caller.Func.String(), kind, a.siteFan[e.Site], site)
		}
	}
}

func writeMarkdown(path, algo string, nRoots, nCobra, nDecls int, hist, histLOC map[int]int, results []*cmdResult, unreached []*decl) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Command reachability (%s)\n\n%d roots (%d cobra, plus <init> and one per main package), %d module decls.\n\n", algo, nRoots, nCobra, nDecls)
	fmt.Fprintf(&sb, "## How shared is the code? (decls by number of roots reaching them)\n\n| reached by N roots | decls | LOC |\n|---:|---:|---:|\n")
	var ks []int
	for k := range hist {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	for _, k := range ks {
		fmt.Fprintf(&sb, "| %d | %d | %d |\n", k, hist[k], histLOC[k])
	}
	fmt.Fprintf(&sb, "\n## Per root (sorted by exclusive LOC)\n\n| root | kind | reach LOC | reach cognit | excl LOC | excl cognit | top exclusive features |\n|---|---|---:|---:|---:|---:|---|\n")
	for _, r := range results {
		fmt.Fprintf(&sb, "| `%s` | %s | %d | %d | %d | %d | %s |\n", r.Root, r.Kind, r.ReachLOC, r.ReachCognit, r.ExclLOC, r.ExclCognit, topFeatures(r.ExclFeatures, 3))
	}
	fmt.Fprintf(&sb, "\n## Decls reached by no root (%d)\n\nNothing in the shipped binaries references these (tests excluded; reflection not modelled).\n\n| feature | unreached LOC |\n|---|---:|\n", len(unreached))
	byFeat := map[string]int{}
	for _, d := range unreached {
		byFeat[d.Feat] += d.LOC
	}
	for _, kv := range sortedKV(byFeat) {
		fmt.Fprintf(&sb, "| %s | %d |\n", kv.k, kv.v)
	}
	fmt.Fprintf(&sb, "\n<details><summary>full list</summary>\n\n")
	for _, d := range unreached {
		fmt.Fprintf(&sb, "- `%s:%d` %s (%d loc, cog %d)\n", d.File, d.Line, d.Key, d.LOC, d.Cognit)
	}
	fmt.Fprintf(&sb, "\n</details>\n")
	cx.Check(os.WriteFile(path, []byte(sb.String()), 0o644))
}

func isCobraCmdPtr(t types.Type) bool {
	p, ok := t.(*types.Pointer)
	if !ok {
		return false
	}
	n, ok := p.Elem().(*types.Named)
	if !ok {
		return false
	}
	return n.Obj().Name() == "Command" && n.Obj().Pkg() != nil && strings.HasSuffix(n.Obj().Pkg().Path(), "spf13/cobra")
}

type kv struct {
	k string
	v int
}

func sortedKV(m map[string]int) []kv {
	var out []kv
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].v != out[j].v {
			return out[i].v > out[j].v
		}
		return out[i].k < out[j].k
	})
	return out
}

func topFeatures(m map[string]int, n int) string {
	var parts []string
	for i, e := range sortedKV(m) {
		if i >= n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%d", e.k, e.v))
	}
	return strings.Join(parts, " ")
}

// shortener trims the module path (and the given module-relative prefixes)
// from displayed symbol keys. Presentation-only; applied where keys are
// emitted, never to the analysis itself. trims arrives already cleaned, so the
// returned closure does no per-call parsing — it runs once per emitted key.
func shortener(mod string, trims []string) func(string) string {
	full := make([]string, 0, len(trims))
	for _, tp := range trims {
		full = append(full, mod+"/"+tp)
	}
	return func(s string) string {
		for _, tp := range full {
			s = strings.ReplaceAll(s, tp, "")
		}
		return strings.ReplaceAll(s, mod+"/", "")
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
