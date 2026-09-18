package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type capabilityHelper struct {
	function   string
	capability string
	resolver   string
	position   token.Position
}

func TestCapabilityHelpersUseStandardResolver(t *testing.T) {
	t.Parallel()

	helpers := discoverCapabilityHelpers(t)
	if len(helpers) == 0 {
		t.Fatal("no As* capability helpers found — test setup is broken")
	}

	for _, helper := range helpers {
		switch helper.resolver {
		case "builtinCapability", "declaredCapability":
			if helper.capability == "" {
				t.Errorf("%s: %s must resolve a single named capability type",
					helper.position, helper.function)
			}
		case "":
			t.Errorf("%s: %s must return builtinCapability[T] or declaredCapability[T]",
				helper.position, helper.function)
		default:
			t.Errorf("%s: %s uses unsupported resolver %s",
				helper.position, helper.function, helper.resolver)
		}
	}
}

func TestDeclaredCapsBuiltinListMatchesCapabilityHelpers(t *testing.T) {
	t.Parallel()

	var resolved []string
	for _, helper := range discoverCapabilityHelpers(t) {
		if helper.resolver == "builtinCapability" {
			resolved = append(resolved, helper.capability)
		}
	}
	slices.Sort(resolved)

	documented := documentedBuiltinCapabilities(t)
	if !slices.IsSorted(documented) {
		t.Fatalf("DeclaredCaps built-in capability list must be sorted: %v", documented)
	}
	if !slices.Equal(documented, resolved) {
		t.Fatalf("DeclaredCaps built-in capability list is out of sync\ndocumented: %v\nresolved:   %v",
			documented, resolved)
	}
}

func discoverCapabilityHelpers(t *testing.T) []capabilityHelper {
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

	var helpers []capabilityHelper
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || !isCapabilityHelperName(fn.Name.Name) {
					continue
				}
				resolver, capability := capabilityResolver(fn)
				helpers = append(helpers, capabilityHelper{
					function:   fn.Name.Name,
					capability: capability,
					resolver:   resolver,
					position:   fset.Position(fn.Pos()),
				})
			}
		}
	}
	return helpers
}

func isCapabilityHelperName(name string) bool {
	return strings.HasPrefix(name, "As") && len(name) > len("As") && ast.IsExported(name[len("As"):])
}

func capabilityResolver(fn *ast.FuncDecl) (string, string) {
	if fn.Body == nil || len(fn.Body.List) != 1 {
		return "", ""
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return "", ""
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	if !ok {
		return "", ""
	}

	resolver, typeArg := indexedCall(call.Fun)
	if resolver == nil || typeArg == nil {
		return "", ""
	}
	capability, ok := typeArg.(*ast.Ident)
	if !ok {
		return resolver.Name, ""
	}
	return resolver.Name, capability.Name
}

func indexedCall(expr ast.Expr) (*ast.Ident, ast.Expr) {
	switch indexed := expr.(type) {
	case *ast.IndexExpr:
		resolver, ok := indexed.X.(*ast.Ident)
		if !ok {
			return nil, nil
		}
		return resolver, indexed.Index
	case *ast.IndexListExpr:
		resolver, ok := indexed.X.(*ast.Ident)
		if !ok || len(indexed.Indices) != 1 {
			return nil, nil
		}
		return resolver, indexed.Indices[0]
	default:
		return nil, nil
	}
}

func documentedBuiltinCapabilities(t *testing.T) []string {
	t.Helper()

	path := filepath.Join(findAgentDir(t), "capabilities.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parser.ParseFile(%s): %v", path, err)
	}

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != "DeclaredCaps" {
				continue
			}
			doc := typeSpec.Doc
			if doc == nil {
				doc = gen.Doc
			}
			if doc == nil {
				t.Fatal("DeclaredCaps must have a doc comment")
			}

			var capabilities []string
			for _, comment := range doc.List {
				line := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
				name, ok := strings.CutPrefix(line, "- ")
				if !ok {
					continue
				}
				if fields := strings.Fields(name); len(fields) != 1 || !ast.IsExported(name) {
					t.Fatalf("invalid capability entry in DeclaredCaps doc: %q", line)
				}
				capabilities = append(capabilities, name)
			}
			if len(capabilities) == 0 {
				t.Fatal("DeclaredCaps doc must list built-in capabilities as '- CapabilityName' entries")
			}
			return capabilities
		}
	}

	t.Fatal("DeclaredCaps type not found")
	return nil
}

// TestAgentPackages_NoForbiddenImports verifies that agent implementation packages
// (claudecode, geminicli, opencode, cursor, etc.) only import from allowed packages.
//
// This prevents agent implementations from coupling to framework internals
// (strategy, checkpoint, session, commands, hook_registry, lifecycle) which
// would create brittle dependencies that break when internals change.
//
// Agents should depend only on:
//   - The agent contract package (agent, agent/types, agent/testutil)
//   - Utility packages (logging, paths, jsonutil, textutil, transcript, etc.)
//   - Standard library and external dependencies
func TestAgentPackages_NoForbiddenImports(t *testing.T) {
	t.Parallel()

	const repoPrefix = "github.com/entireio/cli/cmd/entire/cli/"

	// Packages that agent implementations must NOT import.
	// These are framework internals that agents should be decoupled from.
	forbiddenSuffixes := []string{
		"strategy",
		"checkpoint",
		"session",
		"commands",
	}

	// Individual files in the cli package that agents must not depend on.
	// We check for the cli package import itself since these are all in
	// the top-level cli package (not sub-packages).
	forbiddenCLIPackage := repoPrefix[:len(repoPrefix)-1] // "github.com/entireio/cli/cmd/entire/cli"

	// Packages within the repo that agents ARE allowed to import.
	allowedPrefixes := []string{
		repoPrefix + "agent",      // agent contract + sub-packages
		repoPrefix + "gitrepo",    // repository layout utilities
		repoPrefix + "logging",    // logging utilities
		repoPrefix + "paths",      // path utilities
		repoPrefix + "entiredir",  // the shared .entire root; the only sanctioned way to read or write there
		repoPrefix + "osroot",     // traversal-resistant os.Root helpers, used with the above
		repoPrefix + "jsonutil",   // JSON utilities
		repoPrefix + "textutil",   // text utilities
		repoPrefix + "transcript", // transcript utilities
		repoPrefix + "trailers",   // git trailer utilities
		repoPrefix + "stringutil", // string utilities
		repoPrefix + "telemetry",  // telemetry
		repoPrefix + "validation", // validation utilities
		repoPrefix + "settings",   // settings (read-only access)
		repoPrefix + "review",     // review env contract + AgentReviewer types (used by per-agent reviewer.go files)
		repoPrefix + "testutil",   // canonical isolated repository fixtures for agent tests
	}

	agentDir := findAgentDir(t)
	agentPkgs := discoverAgentPackages(t, agentDir)

	if len(agentPkgs) == 0 {
		t.Fatal("no agent packages found — test setup is broken")
	}

	for _, pkgDir := range agentPkgs {
		pkgName := filepath.Base(pkgDir)
		t.Run(pkgName, func(t *testing.T) {
			t.Parallel()
			imports := extractImports(t, pkgDir)
			for _, imp := range imports {
				if !strings.HasPrefix(imp, repoPrefix) && imp != forbiddenCLIPackage {
					continue // external or stdlib — allowed
				}

				// Check if it's the top-level cli package itself
				if imp == forbiddenCLIPackage {
					t.Errorf("forbidden import %q — agent packages must not import the top-level cli package directly", imp)
					continue
				}

				// Check forbidden suffixes
				rel := strings.TrimPrefix(imp, repoPrefix)
				isForbidden := false
				for _, forbidden := range forbiddenSuffixes {
					if rel == forbidden || strings.HasPrefix(rel, forbidden+"/") {
						t.Errorf("forbidden import %q — agent packages must not import %s internals", imp, forbidden)
						isForbidden = true
					}
				}
				if isForbidden {
					continue
				}

				// Check it's in the allowed list
				allowed := false
				for _, prefix := range allowedPrefixes {
					if imp == prefix || strings.HasPrefix(imp, prefix+"/") {
						allowed = true
						break
					}
				}
				if !allowed {
					t.Errorf("unexpected internal import %q — if this is intentional, add it to allowedPrefixes in architecture_test.go", imp)
				}
			}
		})
	}
}

// findAgentDir returns the absolute path to cmd/entire/cli/agent/.
// Go test runner sets cwd to the package directory, so os.Getwd() gives us
// the agent dir directly.
func findAgentDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	return wd
}

// discoverAgentPackages finds all subdirectories of the agent dir that contain
// Go files, excluding "types" and "testutil" (those are contract/shared packages).
func discoverAgentPackages(t *testing.T, agentDir string) []string {
	t.Helper()

	skipDirs := map[string]bool{
		"types":          true, // contract types, not an agent implementation
		"testutil":       true, // shared test utilities
		"external":       true, // external agent adapter, not a self-registering agent
		"skilldiscovery": true, // shared capability helper (registries, match), not an agent
		"spawn":          true, // shared Spawner interface for review/investigate, not an agent
	}

	entries, err := os.ReadDir(agentDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", agentDir, err)
	}

	var pkgs []string
	for _, e := range entries {
		if !e.IsDir() || skipDirs[e.Name()] {
			continue
		}
		dir := filepath.Join(agentDir, e.Name())
		// Only include if it has Go files
		goFiles, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("filepath.Glob(%s): %v", dir, err)
		}
		if len(goFiles) > 0 {
			pkgs = append(pkgs, dir)
		}
	}
	return pkgs
}

// extractImports parses all Go files in a directory and returns unique import paths.
// Includes both production and test files — test-only imports should also respect boundaries.
func extractImports(t *testing.T, dir string) []string {
	t.Helper()

	fset := token.NewFileSet()
	//nolint:staticcheck // ParseDir is deprecated in favor of go/packages, but we intentionally
	// scan all files regardless of build tags to catch forbidden imports in test files too.
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parser.ParseDir(%s): %v", dir, err)
	}

	seen := make(map[string]bool)
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("strconv.Unquote(%s): %v", imp.Path.Value, err)
				}
				seen[path] = true
			}
		}
	}

	imports := make([]string, 0, len(seen))
	for imp := range seen {
		imports = append(imports, imp)
	}
	return imports
}

// TestAgentPackages_SelfRegister verifies that each agent package has an init()
// function that calls agent.Register(). This ensures agents are properly
// discoverable via the registry.
func TestAgentPackages_SelfRegister(t *testing.T) {
	t.Parallel()

	agentDir := findAgentDir(t)
	agentPkgs := discoverAgentPackages(t, agentDir)

	if len(agentPkgs) == 0 {
		t.Fatal("no agent packages found — test setup is broken")
	}

	for _, pkgDir := range agentPkgs {
		pkgName := filepath.Base(pkgDir)
		t.Run(pkgName, func(t *testing.T) {
			t.Parallel()
			if !hasInitWithRegister(t, pkgDir) {
				t.Errorf("agent package %q has no init() function calling Register() — agents must self-register", pkgName)
			}
		})
	}
}

// hasInitWithRegister checks if any Go file in the directory has an init()
// function that contains a call to Register.
func hasInitWithRegister(t *testing.T, dir string) bool {
	t.Helper()

	fset := token.NewFileSet()
	//nolint:staticcheck // See extractImports for rationale.
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parser.ParseDir(%s): %v", dir, err)
	}

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != "init" || fn.Recv != nil {
					continue
				}
				// Walk the init body looking for a call to Register or agent.Register
				found := false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch fun := call.Fun.(type) {
					case *ast.Ident:
						if fun.Name == "Register" {
							found = true
						}
					case *ast.SelectorExpr:
						if fun.Sel.Name == "Register" {
							found = true
						}
					}
					return !found
				})
				if found {
					return true
				}
			}
		}
	}
	return false
}
