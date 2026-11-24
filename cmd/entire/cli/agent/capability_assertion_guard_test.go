package agent

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// nonCapabilityInterfaces are the interfaces in package agent that are not
// optional capabilities, so asserting to them directly is fine.
var nonCapabilityInterfaces = map[string]string{
	"Agent":              "the base contract every agent implements",
	"CapabilityDeclarer": "the resolvers' own declaration gate",
	"SessionLocator":     "the narrow slice of Agent a SessionStore holds, not an optional capability",
	"TestOnly":           "registry marker that hides test agents from selection, not a capability",
}

// TestCapabilitiesAreOnlyReachedThroughHelpers fails when code type-asserts an
// agent to an optional capability interface instead of calling its As* helper.
//
// An inline assertion skips the helper's DeclaredCaps gate, so an external
// agent that implements the method but did not declare the capability is used
// anyway. It also hides the capability from the DeclaredCaps doc list, which is
// derived from the helpers.
//
// The check parses every production Go file rather than grepping it, so an
// import under another name (agentpkg, cliagent) and a type-switch case list
// that wraps onto a second line are both seen. Naming the package-agent
// interface is not the only way to reach one, so an asserted type is also
// followed through aliases, defined types, interface literals, and interfaces
// declared in any repository package or inside the asserting function, through
// generic instantiations and dot imports, and matched by the capability methods
// or embedded capability interfaces it reaches.
func TestCapabilitiesAreOnlyReachedThroughHelpers(t *testing.T) {
	t.Parallel()

	repoRoot, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}

	interfaces, helpers := agentPackageDecls(t)
	resolver := &assertionResolver{
		interfaces: interfaces,
		methods:    capabilityMethods(interfaces),
		decls:      map[string]map[string][]typeDecl{},
	}

	var files []*sourceFile
	fset := token.NewFileSet()
	for _, rel := range testutil.GuardGoFiles(t, repoRoot) {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if errors.Is(err, fs.ErrNotExist) {
			// A tracked file deleted from the working tree or excluded by a
			// sparse checkout is not compiled here either; the checkout that
			// builds it runs this guard over it.
			t.Logf("skipping %s: %v", rel, err)
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		parsed, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		if err != nil {
			// A file with a syntax error cannot compile either, so it cannot
			// hide an assertion; it is more likely an untracked scratch file.
			t.Logf("skipping %s: %v", rel, err)
			continue
		}
		file := newSourceFile(rel, parsed)
		files = append(files, file)
		resolver.addDecls(file)
	}

	// The staleness check counts the files it scans that import package agent
	// rather than matched assertions, so routing the last allowed assertion
	// through a helper does not read as broken detection.
	importingFiles, assertionCount := 0, 0
	for _, f := range files {
		if f.qualifier != "-" && f.dir != agentPackageDir {
			importingFiles++
		}
		for _, a := range fileAssertions(f.ast) {
			assertionCount++
			expr := a.typ
			pos := fset.Position(expr.Pos())
			if name, ok := agentInterfaceName(expr, f.qualifier, interfaces); ok {
				if _, allowed := nonCapabilityInterfaces[name]; !allowed {
					t.Errorf("%s:%d: asserts an agent to capability %s directly; %s",
						f.rel, pos.Line, name, helperAdvice(name, helpers))
				}
				continue
			}
			if capability, via, found := resolver.reach(expr, f, a.locals, map[ast.Expr]bool{}); found {
				t.Errorf("%s:%d: asserts to %s, which reaches capability %s through %s; if the value is an agent, %s",
					f.rel, pos.Line, types.ExprString(expr), capability, via, helperAdvice(capability, helpers))
			}
		}
	}
	if importingFiles == 0 {
		t.Fatalf("no file outside package agent imports %s — the guard's import detection has gone stale", agentImportPath)
	}
	if assertionCount == 0 {
		t.Fatal("no type assertions found in any scanned file — the guard's assertion detection has gone stale")
	}

	for name := range nonCapabilityInterfaces {
		if _, ok := interfaces[name]; !ok {
			t.Errorf("nonCapabilityInterfaces lists %s, which is no longer an interface in package agent", name)
		}
	}
}

const (
	modulePath      = "github.com/entireio/cli"
	agentPackageDir = "cmd/entire/cli/agent"
	agentImportPath = modulePath + "/" + agentPackageDir
)

// sourceFile is a parsed production file and the names it uses for the
// packages it imports.
type sourceFile struct {
	rel       string
	dir       string
	ast       *ast.File
	qualifier string            // see agentImportName
	imports   map[string]string // local package name to directory, for repository packages
	dotDirs   []string          // directories of dot-imported repository packages
}

func newSourceFile(rel string, file *ast.File) *sourceFile {
	f := &sourceFile{
		rel:       rel,
		dir:       path.Dir(rel),
		ast:       file,
		qualifier: agentImportName(file, rel),
		imports:   map[string]string{},
	}
	for _, imp := range file.Imports {
		dir, ok := strings.CutPrefix(strings.Trim(imp.Path.Value, `"`), modulePath+"/")
		if !ok {
			continue
		}
		// Without an alias the package name is assumed to match its directory,
		// which holds for the repository's packages.
		name := path.Base(dir)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." {
			f.dotDirs = append(f.dotDirs, dir)
			continue
		}
		f.imports[name] = dir
	}
	return f
}

// typeDecl is a named type declared in a scanned file.
type typeDecl struct {
	typ  ast.Expr
	file *sourceFile
}

// assertionResolver follows an asserted type to the package-agent capability
// it reaches, if any.
type assertionResolver struct {
	interfaces map[string]*ast.InterfaceType // package agent's interfaces
	methods    map[string]string             // see capabilityMethods
	// decls holds every named type by directory and name. A name can have
	// several declarations when build-tagged files declare it differently.
	decls map[string]map[string][]typeDecl
}

func (r *assertionResolver) addDecls(file *sourceFile) {
	for _, decl := range file.ast.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if r.decls[file.dir] == nil {
				r.decls[file.dir] = map[string][]typeDecl{}
			}
			name := typeSpec.Name.Name
			r.decls[file.dir][name] = append(r.decls[file.dir][name], typeDecl{typeSpec.Type, file})
		}
	}
}

// reach reports the capability typ, as written in file, reaches: by naming a
// package-agent capability interface, declaring one of its methods, or naming
// or embedding a type that does. locals holds the types declared inside the
// function typ appears in; it is nil outside one. via describes the step that
// reached it.
func (r *assertionResolver) reach(typ ast.Expr, file *sourceFile, locals map[string][]ast.Expr, seen map[ast.Expr]bool) (string, string, bool) {
	if seen[typ] {
		return "", "", false
	}
	seen[typ] = true
	if name, ok := agentInterfaceName(typ, file.qualifier, r.interfaces); ok {
		if _, allowed := nonCapabilityInterfaces[name]; allowed {
			return "", "", false
		}
		return name, "agent." + name, true
	}
	switch t := typ.(type) {
	case *ast.ParenExpr:
		return r.reach(t.X, file, locals, seen)
	case *ast.IndexExpr:
		return r.reach(t.X, file, locals, seen)
	case *ast.IndexListExpr:
		return r.reach(t.X, file, locals, seen)
	case *ast.InterfaceType:
		for _, field := range t.Methods.List {
			for _, name := range field.Names {
				if capability, ok := r.methods[name.Name]; ok {
					return capability, "method " + name.Name, true
				}
			}
			if len(field.Names) == 0 {
				if capability, via, ok := r.reach(field.Type, file, locals, seen); ok {
					return capability, via, true
				}
			}
		}
	case *ast.Ident:
		for _, local := range locals[t.Name] {
			if capability, via, ok := r.reach(local, file, locals, seen); ok {
				return capability, via, true
			}
		}
		for _, dir := range append([]string{file.dir}, file.dotDirs...) {
			if capability, via, ok := r.reachDecls(dir, t.Name, seen); ok {
				return capability, via, true
			}
		}
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok {
			if dir, ok := file.imports[pkg.Name]; ok {
				return r.reachDecls(dir, t.Sel.Name, seen)
			}
		}
	}
	return "", "", false
}

func (r *assertionResolver) reachDecls(dir, name string, seen map[ast.Expr]bool) (string, string, bool) {
	for _, decl := range r.decls[dir][name] {
		if capability, via, ok := r.reach(decl.typ, decl.file, nil, seen); ok {
			return capability, via, true
		}
	}
	return "", "", false
}

// helperAdvice tells the developer how to reach capability through its As*
// helper, including how to add one when the capability has none yet.
func helperAdvice(capability string, helpers map[string]struct{}) string {
	helper := "As" + capability
	if _, ok := helpers[helper]; ok {
		return "call " + helper + " so its DeclaredCaps gate applies"
	}
	return "add " + helper + " to package agent, resolving through declaredCapability (with a DeclaredCaps field), " +
		"builtinCapability, or nonDeclarerCapability (listing " + capability + " in the DeclaredCaps doc for the last two), and call it"
}

// capabilityMethods maps each method declared by a capability interface to
// that interface, the alphabetically first when several declare it. Methods
// also declared by a non-capability interface (the Agent contract every agent
// implements) are left out, since asserting to them reaches no optional
// capability.
func capabilityMethods(interfaces map[string]*ast.InterfaceType) map[string]string {
	shared := map[string]struct{}{}
	for name := range nonCapabilityInterfaces {
		if iface, ok := interfaces[name]; ok {
			for _, method := range methodNames(iface) {
				shared[method] = struct{}{}
			}
		}
	}
	methods := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(interfaces)) {
		if _, ok := nonCapabilityInterfaces[name]; ok {
			continue
		}
		for _, method := range methodNames(interfaces[name]) {
			_, isShared := shared[method]
			_, taken := methods[method]
			if !isShared && !taken {
				methods[method] = name
			}
		}
	}
	return methods
}

// methodNames returns the methods iface declares itself, not those it embeds.
func methodNames(iface *ast.InterfaceType) []string {
	var names []string
	for _, field := range iface.Methods.List {
		for _, name := range field.Names {
			names = append(names, name.Name)
		}
	}
	return names
}

// declaredInterfaces returns the interface types file declares, by name.
func declaredInterfaces(file *ast.File) map[string]*ast.InterfaceType {
	interfaces := map[string]*ast.InterfaceType{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if iface, ok := typeSpec.Type.(*ast.InterfaceType); ok {
				interfaces[typeSpec.Name.Name] = iface
			}
		}
	}
	return interfaces
}

// agentImportName returns the identifier file uses to qualify package agent's
// names: the import's alias, or "agent" when it is imported plainly. Inside
// package agent itself the names are unqualified, reported as "". Files that do
// not import the package cannot assert to its interfaces at all; "-" matches
// nothing. rel is slash-separated, as git prints it.
func agentImportName(file *ast.File, rel string) string {
	if path.Dir(rel) == agentPackageDir {
		return ""
	}
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) != agentImportPath {
			continue
		}
		if imp.Name == nil {
			return "agent"
		}
		if imp.Name.Name == "." {
			return ""
		}
		return imp.Name.Name
	}
	return "-"
}

// agentInterfaceName reports which package-agent interface expr names, if any:
// qualifier.X for files that import the package, or a bare X where qualifier
// is "" (package agent and dot imports).
func agentInterfaceName(expr ast.Expr, qualifier string, interfaces map[string]*ast.InterfaceType) (string, bool) {
	var name string
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok || pkg.Name != qualifier {
			return "", false
		}
		name = e.Sel.Name
	case *ast.Ident:
		if qualifier != "" {
			return "", false
		}
		name = e.Name
	default:
		return "", false
	}
	_, ok := interfaces[name]
	return name, ok
}

// assertion is a type expression a value is asserted to, with the types
// declared inside the top-level declaration it appears in.
type assertion struct {
	typ    ast.Expr
	locals map[string][]ast.Expr
}

// fileAssertions returns every assertion in file. Local types are collected per
// top-level declaration rather than per block, so a local type name shadowed in
// a sibling block can be over-matched; that only risks a false report.
func fileAssertions(file *ast.File) []assertion {
	var assertions []assertion
	for _, decl := range file.Decls {
		locals := localTypes(decl)
		for _, typ := range assertedTypes(decl) {
			assertions = append(assertions, assertion{typ, locals})
		}
	}
	return assertions
}

// localTypes returns the types declared inside decl's function bodies, by name.
func localTypes(decl ast.Decl) map[string][]ast.Expr {
	locals := map[string][]ast.Expr{}
	ast.Inspect(decl, func(n ast.Node) bool {
		stmt, ok := n.(*ast.DeclStmt)
		if !ok {
			return true
		}
		if gen, ok := stmt.Decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
			for _, spec := range gen.Specs {
				if typeSpec, ok := spec.(*ast.TypeSpec); ok {
					locals[typeSpec.Name.Name] = append(locals[typeSpec.Name.Name], typeSpec.Type)
				}
			}
		}
		return true
	})
	return locals
}

// assertedTypes returns every type expression node asserts a value to: the
// X in v.(X) and in each case of a switch v.(type).
func assertedTypes(node ast.Node) []ast.Expr {
	var types []ast.Expr
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.TypeAssertExpr:
			if n.Type != nil {
				types = append(types, n.Type)
			}
		case *ast.TypeSwitchStmt:
			for _, stmt := range n.Body.List {
				if clause, ok := stmt.(*ast.CaseClause); ok {
					types = append(types, clause.List...)
				}
			}
		}
		return true
	})
	return types
}

// agentPackageDecls returns the interfaces declared in the production files of
// package agent, by name, and the names of its package-level functions.
func agentPackageDecls(t *testing.T) (map[string]*ast.InterfaceType, map[string]struct{}) {
	t.Helper()

	dir := findAgentDir(t)
	fset := token.NewFileSet()
	//nolint:staticcheck // ParseDir intentionally scans every production file regardless of build tags.
	pkgs, err := parser.ParseDir(fset, dir, func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parser.ParseDir(%s): %v", dir, err)
	}

	interfaces := map[string]*ast.InterfaceType{}
	funcs := map[string]struct{}{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			maps.Copy(interfaces, declaredInterfaces(file))
			for _, decl := range file.Decls {
				if funcDecl, ok := decl.(*ast.FuncDecl); ok && funcDecl.Recv == nil {
					funcs[funcDecl.Name.Name] = struct{}{}
				}
			}
		}
	}
	if len(interfaces) == 0 {
		t.Fatal("no interfaces found in package agent — test setup is broken")
	}
	return interfaces, funcs
}
