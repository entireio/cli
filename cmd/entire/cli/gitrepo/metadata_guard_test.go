package gitrepo

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestGitMetadataTraversalHasCanonicalOwner(t *testing.T) {
	t.Parallel()

	legacyTraversalOwners := map[string]string{
		"gitrepo/repository.go:resolveDotGitPath":    "Codex still uses the exported legacy resolver until the remaining-consumer split",
		"gitrepo/repository.go:resolveCommonGitPath": "Codex still uses the exported legacy resolver until the remaining-consumer split",
		"paths/worktree.go:GetWorktreeID":            "worktree-ID consumers migrate with session and the remaining consumers",
		"status.go:resolveWorktreeBranch":            "status migrates with the remaining consumers",
	}
	policyDotGitInspections := map[string]string{
		"agent/codex/hook_root.go:hasDotGitEntry":   "Codex policy checks whether a candidate checkout owns a .git entry",
		"dispatch_wizard.go:discoverLocalRepoRoots": "dispatch discovery filters sibling repository candidates",
		"gitrepo/status.go:insideNestedCheckout":    "status walking stops at nested checkout boundaries",
		"plugin_index.go:SyncPluginIndex":           "plugin index sync checks whether Entire's cache directory contains its clone",
	}
	allowedMetadataQueries := map[guardMetadataQuery]string{
		{source: "dispatch/mode_local.go:resolveRepoRoots", flag: "--show-toplevel"}:                    "local dispatch resolves explicit repository candidates",
		{source: "dispatch_wizard.go:resolveGitTopLevel", flag: "--show-toplevel"}:                      "dispatch discovery resolves explicit repository candidates",
		{source: "gitdir/gitdir.go:CommonDir", flag: "--git-common-dir"}:                                "session removes the current-worktree resolver in the session split",
		{source: "gitdir/gitdir.go:CommonDirForWorktree", flag: "--git-common-dir"}:                     "session removes the explicit-worktree resolver in the session split",
		{source: "paths/paths.go:resolveWorktreeRoot", flag: "--show-toplevel"}:                         "worktree-root discovery remains separate from explicit-root metadata resolution",
		{source: "session_adopt.go:stateStoreForWorktree", flag: "--git-common-dir"}:                    "adoption validates an arbitrary source repository in the session split",
		{source: "session_adopt.go:stateStoreForWorktree", flag: "--show-toplevel"}:                     "adoption validates an arbitrary source repository in the session split",
		{source: "settings/settings.go:clonePreferencesPathForWorktreeRoot", flag: "--git-common-dir"}:  "settings migrates with the remaining consumers",
		{source: "strategy/common.go:GetGitCommonDir", flag: "--git-common-dir"}:                        "strategy migrates in the strategy-and-hooks split",
		{source: "strategy/hooks.go:getGitDirInPath", flag: "--git-dir"}:                                "hook directory discovery migrates in the strategy-and-hooks split",
		{source: "strategy/manual_commit_session.go:gitCommonDirForWorktree", flag: "--git-common-dir"}: "session routing migrates in the strategy-and-hooks split",
		{source: "strategy/metadata_reconcile.go:loadShallowHashes", flag: "--git-common-dir"}:          "shallow metadata access migrates in the strategy-and-hooks split",
		{source: "trail_checkout_worktree.go:gitCommonDirForTrailWorktree", flag: "--git-common-dir"}:   "trail storage paths migrate with the remaining consumers",
		{source: "trail_checkout_worktree.go:validateTrailWorktreeReuse", flag: "--git-common-dir"}:     "trail reuse validation migrates with the remaining consumers",
		{source: "trail_checkout_worktree.go:validateTrailWorktreeReuse", flag: "--show-toplevel"}:      "trail reuse validation migrates with the remaining consumers",
	}
	allowedLegacyResolverCalls := map[string]string{
		"agent/codex/hook_root.go:resolveHookDiscovery": "Codex discovery migrates with the remaining consumers",
		"agent/codex/hook_root.go:rootOwnsGitDir":       "Codex ownership policy migrates with the remaining consumers",
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve guard source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".."))
	policy := gitMetadataGuardPolicy{
		legacyTraversalOwners:   legacyTraversalOwners,
		policyDotGitInspections: policyDotGitInspections,
		allowedMetadataQueries:  allowedMetadataQueries,
		allowedLegacyCalls:      allowedLegacyResolverCalls,
	}
	result, err := scanGitMetadataSources(repoRoot, policy)
	if err != nil {
		t.Fatalf("scan Go sources: %v", err)
	}
	for _, violation := range result.violations {
		t.Error(violation)
	}
	if result.canonicalTokens < 2 {
		t.Fatal("guard found no canonical gitdir/commondir parser tokens")
	}
	assertGuardLedgerSeen(t, "legacy traversal owner", legacyTraversalOwners, result.legacyOwnersSeen)
	assertGuardLedgerSeen(t, ".git policy inspection", policyDotGitInspections, result.policyInspectionsSeen)
	assertMetadataQueryLedgerSeen(t, allowedMetadataQueries, result.metadataQueriesSeen)
	assertGuardLedgerSeen(t, "legacy resolver call", allowedLegacyResolverCalls, result.legacyResolverCallsSeen)
}

type guardMetadataQuery struct {
	source string
	flag   string
}

type gitMetadataGuardPolicy struct {
	legacyTraversalOwners   map[string]string
	policyDotGitInspections map[string]string
	allowedMetadataQueries  map[guardMetadataQuery]string
	allowedLegacyCalls      map[string]string
}

type gitMetadataGuardResult struct {
	canonicalTokens         int
	legacyOwnersSeen        map[string]bool
	policyInspectionsSeen   map[string]bool
	metadataQueriesSeen     map[guardMetadataQuery]bool
	legacyResolverCallsSeen map[string]bool
	violations              []string
}

func scanGitMetadataSources(repoRoot string, policy gitMetadataGuardPolicy) (gitMetadataGuardResult, error) {
	fset := token.NewFileSet()
	packages, err := collectGuardPackages(fset, repoRoot)
	if err != nil {
		return gitMetadataGuardResult{}, fmt.Errorf("collect guard packages: %w", err)
	}
	result := gitMetadataGuardResult{
		legacyOwnersSeen:        map[string]bool{},
		policyInspectionsSeen:   map[string]bool{},
		metadataQueriesSeen:     map[guardMetadataQuery]bool{},
		legacyResolverCallsSeen: map[string]bool{},
	}

	for _, pkg := range packages {
		valueResolver := newGuardValueResolver(fset, pkg)
		for _, source := range pkg.files {
			rel, file := source.rel, source.file
			gitrepoImports, dotImportedGitrepo := gitrepoImportNames(file)
			samePackage := filepath.ToSlash(filepath.Dir(rel)) == "cmd/entire/cli/gitrepo"
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				key := guardSourceKey(rel, fn.Name.Name)
				canonicalOwner := guardSourcePath(rel) == "gitrepo/metadata.go"
				_, legacyOwner := policy.legacyTraversalOwners[key]
				_, policyInspection := policy.policyDotGitInspections[key]

				for _, value := range []string{"gitdir: ", "gitdir:", "commondir"} {
					if !valueResolver.contains(fn.Body, value) {
						continue
					}
					if canonicalOwner {
						result.canonicalTokens++
						continue
					}
					if legacyOwner {
						result.legacyOwnersSeen[key] = true
						continue
					}
					result.violations = append(result.violations, fmt.Sprintf("%s independently parses Git metadata token %q; use gitrepo.ResolveWorktreeMetadata", fset.Position(fn.Pos()), value))
				}
				if valueResolver.contains(fn.Body, "rev-parse") {
					for _, flag := range []string{"--absolute-git-dir", "--git-common-dir", "--git-dir", "--show-toplevel"} {
						if !valueResolver.contains(fn.Body, flag) {
							continue
						}
						query := guardMetadataQuery{source: key, flag: flag}
						if _, allowed := policy.allowedMetadataQueries[query]; allowed {
							result.metadataQueriesSeen[query] = true
						} else {
							result.violations = append(result.violations, fmt.Sprintf("%s runs an unaudited git rev-parse %s query; use gitrepo.ResolveWorktreeMetadata or document a semantic-query exception", fset.Position(fn.Pos()), flag))
						}
					}
				}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					if isLegacyMetadataResolverCall(call, gitrepoImports, dotImportedGitrepo, samePackage) {
						if _, allowed := policy.allowedLegacyCalls[key]; allowed {
							result.legacyResolverCallsSeen[key] = true
						} else {
							result.violations = append(result.violations, fmt.Sprintf("%s calls a legacy Git metadata resolver; use gitrepo.ResolveWorktreeMetadata or document the migration exception", fset.Position(call.Pos())))
						}
					}
					if !valueResolver.contains(call, ".git") || !isFilesystemMetadataInspection(call) || canonicalOwner {
						return true
					}
					if legacyOwner {
						result.legacyOwnersSeen[key] = true
						return true
					}
					if policyInspection {
						result.policyInspectionsSeen[key] = true
						return true
					}
					result.violations = append(result.violations, fmt.Sprintf("%s independently inspects a .git entry; use gitrepo.ResolveWorktreeMetadata or document a narrow policy exception", fset.Position(call.Pos())))
					return true
				})
			}
		}
	}
	return result, nil
}

func TestGitMetadataGuardRecognizesBypassForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		source      string
		kind        string
		value       string
		samePackage bool
		want        bool
	}{
		{
			name:   "rooted filesystem with local alias",
			source: `package probe; import "os"; func inspect(root *os.Root) { name := ".git"; _, _ = root.Lstat(name) }`,
			kind:   "filesystem",
			want:   true,
		},
		{
			name:   "package constant",
			source: `package probe; import "os"; const dotGitName = ".git"; func inspect() { _, _ = os.Stat(dotGitName) }`,
			kind:   "filesystem",
			want:   true,
		},
		{
			name:   "open root",
			source: `package probe; import "os"; func inspect() { _, _ = os.OpenRoot(".git") }`,
			kind:   "filesystem",
			want:   true,
		},
		{
			name:   "readlink",
			source: `package probe; import "os"; func inspect() { _, _ = os.Readlink(".git") }`,
			kind:   "filesystem",
			want:   true,
		},
		{
			name:   "package parser token",
			source: `package probe; import "strings"; const prefix = "gitdir: "; func inspect(data string) { _, _ = strings.CutPrefix(data, prefix) }`,
			kind:   "token",
			value:  "gitdir: ",
			want:   true,
		},
		{
			name:   "absolute git directory query",
			source: `package probe; import "os/exec"; const flag = "--absolute-git-dir"; func inspect() { _ = exec.Command("git", "rev-parse", flag) }`,
			kind:   "query",
			value:  "--absolute-git-dir",
			want:   true,
		},
		{
			name:   "aliased legacy resolver import",
			source: `package probe; import repo "github.com/entireio/cli/cmd/entire/cli/gitrepo"; func inspect() { _, _ = repo.ResolveDotGitPath(".") }`,
			kind:   "legacy",
			want:   true,
		},
		{
			name:        "same-package legacy resolver",
			source:      `package gitrepo; func inspect() { _, _ = ResolveDotGitPath(".") }`,
			kind:        "legacy",
			samePackage: true,
			want:        true,
		},
		{
			name:   "git directory parameter is not metadata traversal",
			source: `package probe; import "os"; func inspect(gitDir string) { _, _ = os.Stat(gitDir) }`,
			kind:   "filesystem",
			want:   false,
		},
		{
			name:   "parameter shadowing a package constant is not metadata traversal",
			source: `package probe; import "os"; const gitDir = ".git"; func inspect(gitDir string) { _, _ = os.Stat(gitDir) }`,
			kind:   "filesystem",
			want:   false,
		},
		{
			name:   "struct field named after a package constant is not metadata traversal",
			source: `package probe; import "os"; const gitDir = ".git"; type layout struct{ gitDir string }; func inspect(l layout) { _, _ = os.Stat(l.gitDir) }`,
			kind:   "filesystem",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "probe.go", tt.source, 0)
			if err != nil {
				t.Fatalf("parse probe: %v", err)
			}
			pkg := guardPackage{key: "probe", files: []guardSourceFile{{rel: "probe.go", file: file}}}
			valueResolver := newGuardValueResolver(fset, pkg)
			imports, dotImported := gitrepoImportNames(file)
			found := false
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				switch tt.kind {
				case "token":
					found = valueResolver.contains(fn.Body, tt.value)
				case "query":
					found = valueResolver.contains(fn.Body, "rev-parse") &&
						valueResolver.contains(fn.Body, tt.value)
				}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch tt.kind {
					case "legacy":
						found = found || isLegacyMetadataResolverCall(call, imports, dotImported, tt.samePackage)
					case "filesystem":
						found = found || valueResolver.contains(call, ".git") && isFilesystemMetadataInspection(call)
					}
					return true
				})
			}
			if found != tt.want {
				t.Fatalf("guard recognition = %t, want %t", found, tt.want)
			}
		})
	}
}

func TestGitMetadataGuardScansCompleteRepository(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeGuardSource(t, repoRoot, "probe/tokens.go", `package probe; const dotGitName = ".git"; const gitDir = ".git"`)
	writeGuardSource(t, repoRoot, "probe/use.go", `package probe; import "os"; func crossFileConstant() { _, _ = os.Stat(dotGitName) }`)
	writeGuardSource(t, repoRoot, "internal/probe/probe.go", `package probe; import "os"; func internalReadlink() { _, _ = os.Readlink(".git") }`)
	writeGuardSource(t, repoRoot, "probe/shadow.go", `package probe; import "os"; func shadowed(gitDir string) { _, _ = os.Stat(gitDir) }`)
	writeGuardSource(t, repoRoot, "probe/query.go", `package probe; import "os/exec"; func existingException() { _ = exec.Command("git", "rev-parse", "--show-toplevel", "--git-dir") }`)
	// The field is declared in one file and read in another, while the package
	// also declares a gitDir constant. A per-file check cannot resolve the
	// selector, and resolving it by name instead reported this as a traversal.
	writeGuardSource(t, repoRoot, "probe/layout.go", `package probe; type layout struct{ gitDir string }`)
	writeGuardSource(t, repoRoot, "probe/method.go", `package probe; import "os"; func (l layout) crossFileField() { _, _ = os.Stat(l.gitDir) }`)

	allowedQuery := guardMetadataQuery{source: "probe/query.go:existingException", flag: "--show-toplevel"}
	result, err := scanGitMetadataSources(repoRoot, gitMetadataGuardPolicy{
		allowedMetadataQueries: map[guardMetadataQuery]string{allowedQuery: "fixture exception"},
	})
	if err != nil {
		t.Fatalf("scan fixture repository: %v", err)
	}
	if !result.metadataQueriesSeen[allowedQuery] {
		t.Fatal("scanner did not observe the exact allowed metadata query")
	}
	if len(result.violations) != 3 {
		t.Fatalf("violations = %d, want 3:\n%s", len(result.violations), strings.Join(result.violations, "\n"))
	}
	violations := strings.Join(result.violations, "\n")
	for _, want := range []string{"probe/use.go", "internal/probe/probe.go", "--git-dir"} {
		if !strings.Contains(violations, want) {
			t.Errorf("violations do not contain %q:\n%s", want, violations)
		}
	}
	for _, notWant := range []string{"probe/shadow.go", "probe/method.go", "--show-toplevel"} {
		if strings.Contains(violations, notWant) {
			t.Errorf("violations unexpectedly contain %q:\n%s", notWant, violations)
		}
	}
}

func writeGuardSource(t *testing.T, repoRoot, rel, source string) {
	t.Helper()
	path := filepath.Join(repoRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("write fixture source: %v", err)
	}
}

func isFilesystemMetadataInspection(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch selector.Sel.Name {
	case "DirFS", "Lstat", "Open", "OpenFile", "OpenInRoot", "OpenRoot", "ReadDir", "ReadFile", "Readlink", "Stat":
		return true
	default:
		return false
	}
}

type guardSourceFile struct {
	rel  string
	file *ast.File
}

type guardPackage struct {
	key   string
	files []guardSourceFile
}

// collectGuardPackages groups every scanned file by the package it belongs to,
// so identifiers can later be resolved against the whole package rather than
// against one file in isolation. Packages come back in key order so a scan
// reports violations deterministically.
func collectGuardPackages(fset *token.FileSet, repoRoot string) ([]guardPackage, error) {
	byKey := map[string][]guardSourceFile{}
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !isGuardSourceFile(rel) {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		key := guardPackageKey(rel, file)
		byKey[key] = append(byKey[key], guardSourceFile{rel: rel, file: file})
		return nil
	})
	if err != nil {
		return nil, err
	}

	packages := make([]guardPackage, 0, len(byKey))
	for key, files := range byKey {
		sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
		packages = append(packages, guardPackage{key: key, files: files})
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].key < packages[j].key })
	return packages, nil
}

func gitrepoImportNames(file *ast.File) (map[string]bool, bool) {
	names := map[string]bool{}
	dotImported := false
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != "github.com/entireio/cli/cmd/entire/cli/gitrepo" {
			continue
		}
		switch {
		case spec.Name == nil:
			names["gitrepo"] = true
		case spec.Name.Name == ".":
			dotImported = true
		case spec.Name.Name != "_":
			names[spec.Name.Name] = true
		}
	}
	return names, dotImported
}

func isLegacyMetadataResolverCall(call *ast.CallExpr, importNames map[string]bool, dotImported, samePackage bool) bool {
	legacyName := func(name string) bool {
		return name == "ResolveDotGitPath" || name == "ResolveCommonGitPath"
	}
	if ident, ok := call.Fun.(*ast.Ident); ok {
		return (dotImported || samePackage) && legacyName(ident.Name)
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !legacyName(selector.Sel.Name) {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && importNames[pkg.Name]
}

func assertGuardLedgerSeen(t *testing.T, label string, ledger map[string]string, seen map[string]bool) {
	t.Helper()
	for key, reason := range ledger {
		if !seen[key] {
			t.Errorf("documented %s %s (%s) no longer exists; remove or update the exception", label, key, reason)
		}
	}
}

func assertMetadataQueryLedgerSeen(t *testing.T, ledger map[guardMetadataQuery]string, seen map[guardMetadataQuery]bool) {
	t.Helper()
	for query, reason := range ledger {
		if !seen[query] {
			t.Errorf("documented git metadata query %s %s (%s) no longer exists; remove or update the exception", query.source, query.flag, reason)
		}
	}
}

type guardValueResolver struct {
	typeInfo *types.Info
	bindings map[types.Object]ast.Expr
}

// newGuardValueResolver type-checks a whole package at once, so an identifier
// declared in one file resolves from another, and a local name that shadows a
// package-level constant resolves to the local declaration.
//
// Checking one file at a time cannot do either. The fallback that compensated
// for it matched an unresolved identifier against package constants by name, so
// any parameter, field, or local sharing a name with a constant elsewhere in the
// package was reported as a violation. That fires on gitDir, which strategy
// declares as a constant and several functions take as a parameter, failing the
// build for code the guard is not looking for while naming neither cause nor
// fix. An identifier that still does not resolve now matches nothing.
func newGuardValueResolver(fset *token.FileSet, pkg guardPackage) guardValueResolver {
	typeInfo := &types.Info{
		Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{},
	}
	files := make([]*ast.File, 0, len(pkg.files))
	for _, source := range pkg.files {
		files = append(files, source.file)
	}
	name := "guard"
	if len(files) > 0 {
		name = files[0].Name.Name
	}
	// Imports are not resolved, so type errors are the expected outcome; the
	// identifiers declared inside the package are recorded regardless, and
	// those are all the resolver reads.
	config := types.Config{Error: func(error) {}}
	config.Check(name, fset, files, typeInfo) //nolint:errcheck // unresolved imports always error here.
	return guardValueResolver{
		typeInfo: typeInfo,
		bindings: guardValueBindings(files, typeInfo),
	}
}

func guardValueBindings(files []*ast.File, typeInfo *types.Info) map[types.Object]ast.Expr {
	bindings := map[types.Object]ast.Expr{}
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch decl := node.(type) {
			case *ast.ValueSpec:
				for i, name := range decl.Names {
					if object := typeInfo.Defs[name]; object != nil {
						bindings[object] = expressionAt(decl.Values, i)
					}
				}
			case *ast.AssignStmt:
				if decl.Tok != token.DEFINE {
					return true
				}
				for i, lhs := range decl.Lhs {
					name, ok := lhs.(*ast.Ident)
					if !ok {
						continue
					}
					if object := typeInfo.Defs[name]; object != nil {
						bindings[object] = expressionAt(decl.Rhs, i)
					}
				}
			}
			return true
		})
	}
	return bindings
}

func expressionAt(expressions []ast.Expr, index int) ast.Expr {
	if index < len(expressions) {
		return expressions[index]
	}
	if len(expressions) == 1 {
		return expressions[0]
	}
	return nil
}

func (r guardValueResolver) contains(node ast.Node, want string) bool {
	return r.containsSeen(node, want, map[types.Object]bool{})
}

func (r guardValueResolver) containsSeen(node ast.Node, want string, seen map[types.Object]bool) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		switch value := n.(type) {
		case *ast.BasicLit:
			if value.Kind != token.STRING {
				return true
			}
			unquoted, err := strconv.Unquote(value.Value)
			if err == nil && unquoted == want {
				found = true
				return false
			}
		case *ast.Ident:
			object := r.typeInfo.Uses[value]
			if object == nil {
				object = r.typeInfo.Defs[value]
			}
			if object == nil {
				return false
			}
			if seen[object] {
				return false
			}
			expr := r.bindings[object]
			if expr == nil {
				return false
			}
			seen[object] = true
			if r.containsSeen(expr, want, seen) {
				found = true
			}
			delete(seen, object)
			return false
		}
		return !found
	})
	return found
}

func guardPackageKey(rel string, file *ast.File) string {
	return filepath.ToSlash(filepath.Dir(rel)) + ":" + file.Name.Name
}

func guardSourceKey(rel, function string) string {
	return guardSourcePath(rel) + ":" + function
}

func guardSourcePath(rel string) string {
	return strings.TrimPrefix(filepath.ToSlash(rel), "cmd/entire/cli/")
}

func isGuardSourceFile(rel string) bool {
	rel = filepath.ToSlash(rel)
	if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
		return false
	}
	for _, prefix := range []string{
		"cmd/entire/cli/agent/testutil/",
		"cmd/entire/cli/benchutil/",
		"cmd/entire/cli/integration_test/",
		"e2e/",
	} {
		if strings.HasPrefix(rel, prefix) {
			return false
		}
	}
	return true
}
