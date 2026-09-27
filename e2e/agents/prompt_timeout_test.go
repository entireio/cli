package agents

import (
	"context"

	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

// TestPromptTimeoutPrecedence pins the chain documented in e2e/README.md:
// the runner's own default is widened by E2E_TIMEOUT, and an individual test
// still overrides both.
//
// Cannot be parallel: t.Setenv is process-global state.
func TestPromptTimeoutPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		agentDflt  time.Duration
		env        string
		perTest    time.Duration
		want       time.Duration
		wantErrSub string
	}{
		{name: "default applies when nothing overrides", agentDflt: 60 * time.Second, want: 60 * time.Second},
		{name: "env widens the default", agentDflt: 60 * time.Second, env: "4m", want: 4 * time.Minute},
		{name: "env narrows the default", agentDflt: 90 * time.Second, env: "30s", want: 30 * time.Second},
		{name: "per-test beats env", agentDflt: 60 * time.Second, env: "4m", perTest: 2 * time.Minute, want: 2 * time.Minute},
		{name: "per-test beats default", agentDflt: 60 * time.Second, perTest: 2 * time.Minute, want: 2 * time.Minute},
		{name: "surrounding whitespace is tolerated", agentDflt: 60 * time.Second, env: "  45s\n", want: 45 * time.Second},

		// A zero default means "no bound of our own". It must survive an empty
		// env untouched, because that is what keeps the scenario context from
		// ForEachAgent in charge for the runners that never had a ceiling.
		{name: "zero default stays unbounded", agentDflt: 0, want: 0},
		{name: "zero default still honors env", agentDflt: 0, env: "90s", want: 90 * time.Second},
		{name: "zero default still honors per-test", agentDflt: 0, perTest: 30 * time.Second, want: 30 * time.Second},

		// Rejected rather than ignored: a typo that silently kept the old
		// ceiling is indistinguishable from a slow agent.
		{name: "malformed env is an error", agentDflt: 60 * time.Second, env: "4min", wantErrSub: "not a valid duration"},
		{name: "bare number is an error", agentDflt: 60 * time.Second, env: "240", wantErrSub: "not a valid duration"},
		{name: "zero env is an error", agentDflt: 60 * time.Second, env: "0s", wantErrSub: "must be positive"},
		{name: "negative env is an error", agentDflt: 60 * time.Second, env: "-30s", wantErrSub: "must be positive"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(PromptTimeoutEnv, tc.env)
			if tc.env == "" {
				// t.Setenv cannot unset, and an empty value is what an unset
				// var looks like to os.Getenv anyway.
				t.Setenv(PromptTimeoutEnv, "")
			}

			got, err := promptTimeout(tc.agentDflt, &runConfig{PromptTimeout: tc.perTest})

			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("promptTimeout(%v, %q) = %v, want error containing %q", tc.agentDflt, tc.env, got, tc.wantErrSub)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErrSub)
				}
				// The message must name the variable so the operator knows
				// which knob they mistyped.
				if !strings.Contains(err.Error(), PromptTimeoutEnv) {
					t.Errorf("error = %q, want it to name %s", err, PromptTimeoutEnv)
				}
				return
			}
			if err != nil {
				t.Fatalf("promptTimeout(%v, %q) returned unexpected error: %v", tc.agentDflt, tc.env, err)
			}
			if got != tc.want {
				t.Errorf("promptTimeout(%v, env=%q, perTest=%v) = %v, want %v",
					tc.agentDflt, tc.env, tc.perTest, got, tc.want)
			}
		})
	}
}

// TestPromptTimeoutNilConfig covers the defensive nil branch: a runner that
// resolves before building its runConfig must not panic.
func TestPromptTimeoutNilConfig(t *testing.T) {
	t.Setenv(PromptTimeoutEnv, "")
	got, err := promptTimeout(60*time.Second, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 60*time.Second {
		t.Errorf("got %v, want 60s", got)
	}
}

// TestBoundPrompt covers the ctx-wrapping half: a zero resolution must leave
// the caller's context alone, which is what keeps the scenario timeout in
// charge for the runners that never had a per-prompt ceiling.
func TestBoundPrompt(t *testing.T) {
	t.Run("zero leaves ctx unbounded", func(t *testing.T) {
		t.Setenv(PromptTimeoutEnv, "")
		got, cancel, err := boundPrompt(context.Background(), 0, &runConfig{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer cancel()
		if _, ok := got.Deadline(); ok {
			t.Error("boundPrompt(0) set a deadline; the scenario context must stay in charge")
		}
	})

	t.Run("env bounds a previously unbounded runner", func(t *testing.T) {
		t.Setenv(PromptTimeoutEnv, "30s")
		got, cancel, err := boundPrompt(context.Background(), 0, &runConfig{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer cancel()
		deadline, ok := got.Deadline()
		if !ok {
			t.Fatal("boundPrompt did not set a deadline despite E2E_TIMEOUT")
		}
		if d := time.Until(deadline); d <= 0 || d > 30*time.Second {
			t.Errorf("deadline is %v away, want (0, 30s]", d)
		}
	})

	t.Run("agent default bounds ctx", func(t *testing.T) {
		t.Setenv(PromptTimeoutEnv, "")
		got, cancel, err := boundPrompt(context.Background(), 60*time.Second, &runConfig{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer cancel()
		if _, ok := got.Deadline(); !ok {
			t.Error("boundPrompt did not apply the agent default")
		}
	})

	// The cancel must be safe to defer even on the error path, since callers
	// check err after deferring.
	t.Run("error path returns a usable cancel", func(t *testing.T) {
		t.Setenv(PromptTimeoutEnv, "nonsense")
		got, cancel, err := boundPrompt(context.Background(), 0, &runConfig{})
		if err == nil {
			t.Fatal("expected an error for a malformed E2E_TIMEOUT")
		}
		if got == nil {
			t.Error("boundPrompt returned a nil context on the error path")
		}
		if cancel == nil {
			t.Fatal("boundPrompt returned a nil cancel on the error path")
		}
		cancel() // must not panic
	})
}

// TestEveryRunPromptResolvesThroughPromptTimeout fails the build on any agent
// runner that resolves its per-prompt deadline itself instead of calling
// promptTimeout.
//
// This is a source-level guard because the drift it catches is silent and
// already happened twice. Each runner used to open-code the chain, and the
// copies diverged: only opencode ever read E2E_TIMEOUT, so the variable the
// docs describe as universal moved nothing for the other nine; and claude,
// droid, pi, vogon and roger-roger accepted WithPromptTimeout and dropped it on
// the floor, which made the per-test overrides in split_commits_test.go and
// multi_session_test.go no-ops for half the matrix. Neither failure shows up as
// a test failure — you get a run that ignores the budget you set.
func TestEveryRunPromptResolvesThroughPromptTimeout(t *testing.T) {
	t.Parallel()

	// A plain directory walk rather than parser.ParseDir (deprecated, and it
	// ignores build tags): this package carries no build constraints, so every
	// non-test .go file here is part of it.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	var checked int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "RunPrompt" || fn.Recv == nil || fn.Body == nil {
				continue
			}
			checked++
			if !callsAny(fn.Body, "promptTimeout", "boundPrompt") {
				t.Errorf("%s: %s.RunPrompt resolves its own per-prompt deadline.\n"+
					"Every runner must go through boundPrompt (when it just wants a "+
					"bounded ctx) or promptTimeout (when it needs the duration), so "+
					"E2E_TIMEOUT and WithPromptTimeout apply uniformly. Pass the "+
					"runner's own default, or 0 to stay bounded only by the scenario "+
					"context.",
					name, receiverName(fn))
			}
		}
	}

	// A walk that matched nothing would pass vacuously, which is the one way
	// this guard could stop guarding.
	if checked < 5 {
		t.Fatalf("found only %d RunPrompt implementations; the AST walk is broken", checked)
	}
}

// callsAny reports whether body contains a call to any of the named
// package-level functions. boundPrompt delegates to promptTimeout, so a runner
// using either is resolving through the shared chain.
func callsAny(body *ast.BlockStmt, names ...string) bool {
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			if _, ok := want[id.Name]; ok {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return "?"
	}
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return "*" + id.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return "?"
}
