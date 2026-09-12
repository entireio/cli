package claudecode

// Architectural guard for the launch boundary.
//
// The isolation contract in review_launch.go protects one specific call site.
// Nothing stops a later change from adding a second way to spawn `claude`
// non-interactively that never passes through it — and such a launch would read
// the reviewed checkout's configuration again, silently reopening the boundary
// while every existing test stayed green.
//
// So this test enumerates the places in this package that name the `claude`
// binary directly, and requires each to be a known site with a stated policy.
// Adding a new one fails the build until it is recorded here, which forces the
// isolation question to be answered deliberately rather than by omission.
//
// Scope is this package only: other review adapters have their own launch
// paths and their own assessment, and are deliberately not covered here.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// claudeBinaryName is the argv-0 this guard looks for in package sources.
const claudeBinaryName = "claude"

// claudeLaunchPolicy records why each direct `claude` launch in this package is
// or is not isolated from the working directory's configuration.
var claudeLaunchPolicy = map[string]string{
	// The reviewer reads untrusted code: fully isolated, see review_launch.go.
	"buildReviewCmd": "isolated via claudeReviewFlags",

	// `entire investigate` runs against the user's own checked-out work at their
	// explicit request, and needs to write files, so it runs with elevated
	// permissions. That is a different trust position from review, not an
	// oversight — but it is also why it must not quietly acquire review's
	// callers. Tracked separately; do not widen it here.
	"BuildCmd": "deliberately not isolated: user-initiated spawn with bypassPermissions",

	// Text generation processes untrusted input (dispatch data, transcripts) and
	// needs no repository context at all, so it is isolated harder than review:
	// --setting-sources "" plus a temp working directory. See buildGenerateArgs.
	"GenerateTextStreaming": "isolated via buildStreamingGenerateArgs + temp cwd",

	// The interactive launch is the user starting their own agent session in
	// their own checkout. Loading their configuration is the entire point, and
	// there is a human at the terminal. Not a review-style trust boundary.
	"LaunchCmd": "deliberately not isolated: interactive, user-initiated session",
}

// TestNoUnreviewedClaudeLaunchSites fails when a function spawns `claude`
// without an entry in claudeLaunchPolicy.
func TestNoUnreviewedClaudeLaunchSites(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	found := map[string]string{} // function name -> file:line
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		var enclosing string
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				enclosing = node.Name.Name
			case *ast.CallExpr:
				for _, arg := range node.Args {
					lit, ok := arg.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					// Unquote so `"claude"` matches but `"claude-code"` does not.
					if v, err := strconv.Unquote(lit.Value); err == nil && v == claudeBinaryName && enclosing != "" {
						found[enclosing] = name + ":" + strconv.Itoa(fset.Position(lit.Pos()).Line)
					}
				}
			}
			return true
		})
	}

	if len(found) == 0 {
		t.Fatal("no claude launch sites found; the guard is not scanning what it thinks it is")
	}

	var names []string
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if _, ok := claudeLaunchPolicy[name]; !ok {
			t.Errorf(`%s (%s) spawns claude but has no entry in claudeLaunchPolicy.

A non-interactive claude launch loads the configuration of the directory it
starts in. If that directory can hold code this machine did not write, the
launch must carry the isolation contract in review_launch.go. Decide which
applies, then record it in claudeLaunchPolicy with the reason.`, name, found[name])
		}
	}

	// The reverse direction: a stale entry means the policy map is describing
	// code that no longer exists, which makes it misleading to the next reader.
	for name := range claudeLaunchPolicy {
		if _, ok := found[name]; !ok {
			t.Errorf("claudeLaunchPolicy has an entry for %q but no such launch site exists; remove it", name)
		}
	}
}
