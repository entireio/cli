package agents

import (
	"context"

	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

// stubScaler is a TimeoutMultiplier and nothing else, which is all
// promptTimeout takes. Spelled as a named float so a case reads
// stubScaler(2.5).
type stubScaler float64

func (s stubScaler) TimeoutMultiplier() float64 { return float64(s) }

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
		mult       float64 // 0 means "no scaler", i.e. the nil branch
		perTest    time.Duration
		exact      bool // perTest arrived via withExactPromptTimeout
		want       time.Duration
		wantErrSub string
	}{
		{name: "default applies when nothing overrides", agentDflt: 60 * time.Second, want: 60 * time.Second},
		{name: "env widens the default", agentDflt: 60 * time.Second, env: "4m", want: 4 * time.Minute},
		{name: "env narrows the default", agentDflt: 90 * time.Second, env: "30s", want: 30 * time.Second},
		{name: "per-test beats env", agentDflt: 60 * time.Second, env: "4m", perTest: 2 * time.Minute, want: 2 * time.Minute},
		{name: "per-test beats default", agentDflt: 60 * time.Second, perTest: 2 * time.Minute, want: 2 * time.Minute},

		// A per-test number is authored for the work, not for one of ten
		// runners, so it scales by the same multiplier runForAgents applies to
		// the scenario budget — upward only.
		{name: "per-test scales up for a slow runner", agentDflt: 60 * time.Second, mult: 2.5, perTest: 2 * time.Minute, want: 5 * time.Minute},
		{name: "per-test is untouched at 1x", agentDflt: 60 * time.Second, mult: 1.0, perTest: 2 * time.Minute, want: 2 * time.Minute},
		{name: "per-test never scales down for a fast runner", agentDflt: 60 * time.Second, mult: 0.5, perTest: 2 * time.Minute, want: 2 * time.Minute},

		// A harness deadline was written by one runner for that runner, so
		// scaling it by that runner's own multiplier double-counts.
		{name: "exact timeout is not scaled", agentDflt: 60 * time.Second, mult: 2.5, perTest: 90 * time.Second, exact: true, want: 90 * time.Second},
		{name: "exact timeout still beats env", agentDflt: 60 * time.Second, mult: 2.0, env: "4m", perTest: 30 * time.Second, exact: true, want: 30 * time.Second},

		// The other two inputs stay verbatim: a runner default is already
		// agent-specific, and E2E_TIMEOUT is typed by a human for one run.
		{name: "runner default is not scaled", agentDflt: 60 * time.Second, mult: 2.5, want: 60 * time.Second},
		{name: "env is not scaled", agentDflt: 60 * time.Second, mult: 2.5, env: "90s", want: 90 * time.Second},
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

			var scaler timeoutScaler
			if tc.mult != 0 {
				scaler = stubScaler(tc.mult)
			}
			cfg := &runConfig{}
			if tc.perTest > 0 {
				// Through the options, so the test cannot disagree with them
				// about which one sets promptTimeoutExact.
				opt := WithPromptTimeout(tc.perTest)
				if tc.exact {
					opt = withExactPromptTimeout(tc.perTest)
				}
				opt(cfg)
			}
			got, err := promptTimeout(scaler, tc.agentDflt, cfg)

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

// TestPromptTimeoutNilConfig covers the defensive nil branches: a runner that
// resolves before building its runConfig, or with no scaler at all, must not
// panic.
func TestPromptTimeoutNilConfig(t *testing.T) {
	t.Setenv(PromptTimeoutEnv, "")
	got, err := promptTimeout(nil, 60*time.Second, nil)
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
		got, cancel, err := boundPrompt(context.Background(), stubScaler(1), 0, &runConfig{})
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
		got, cancel, err := boundPrompt(context.Background(), stubScaler(1), 0, &runConfig{})
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
		got, cancel, err := boundPrompt(context.Background(), stubScaler(1), 60*time.Second, &runConfig{})
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
		got, cancel, err := boundPrompt(context.Background(), stubScaler(1), 0, &runConfig{})
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
//
// It also pins that each runner passes its own receiver as the timeoutScaler.
// That argument decides how far a per-test budget is widened, and a runner
// handing over someone else's multiplier — or a literal — is the same class of
// silent drift: the run still passes, on the wrong budget.
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
			call, fname, scalerArg := resolverCall(fn.Body)
			if call == nil {
				t.Errorf("%s: %s.RunPrompt resolves its own per-prompt deadline.\n"+
					"Every runner must go through boundPrompt (when it just wants a "+
					"bounded ctx) or promptTimeout (when it needs the duration), so "+
					"E2E_TIMEOUT and WithPromptTimeout apply uniformly. Pass the "+
					"runner's own default, or 0 to stay bounded only by the scenario "+
					"context.",
					name, receiverName(fn))
				continue
			}

			recv := receiverIdent(fn)
			if recv == "" {
				t.Errorf("%s: %s.RunPrompt has an unnamed receiver, so it cannot pass "+
					"itself as the timeoutScaler", name, receiverName(fn))
				continue
			}
			if scalerArg >= len(call.Args) {
				t.Errorf("%s: %s.RunPrompt calls %s with %d args, too few to carry a "+
					"timeoutScaler", name, receiverName(fn), fname, len(call.Args))
				continue
			}
			if id, ok := call.Args[scalerArg].(*ast.Ident); !ok || id.Name != recv {
				t.Errorf("%s: %s.RunPrompt passes %s as %s's timeoutScaler, want the "+
					"receiver %q.\nThat argument scales the per-test budget by the "+
					"runner's own TimeoutMultiplier; anything else silently runs on "+
					"another agent's budget.",
					name, receiverName(fn), exprString(call.Args[scalerArg]), fname, recv)
			}
		}
	}

	// A walk that matched nothing would pass vacuously, which is the one way
	// this guard could stop guarding.
	if checked < 5 {
		t.Fatalf("found only %d RunPrompt implementations; the AST walk is broken", checked)
	}
}

// TestHarnessDeadlinesAreNotScaled fails the build when a non-test file in this
// package bounds a prompt with WithPromptTimeout instead of
// withExactPromptTimeout.
//
// The exported option scales by the runner's TimeoutMultiplier, which is right
// for a duration written in a test file and wrong for one the harness wrote
// about itself. opencode's Bootstrap warmup was exactly that: it passed
// WithPromptTimeout(openCodeWarmupBudget), so scaling turned a documented
// 90s/30s/30s retry sequence into 180s/60s/60s and broke the four-minute bound
// those constants exist to enforce. Nothing failed — CI just blocked for longer
// on the path the warmup exists to survive, which is why this is a source guard
// and not an assertion.
func TestHarnessDeadlinesAreNotScaled(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	var scanned int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// agent.go declares both options; a mention there is the declaration.
		if name == "agent.go" {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "WithPromptTimeout" {
				t.Errorf("%s:%d: harness code calls WithPromptTimeout, which scales the "+
					"budget by the runner's TimeoutMultiplier.\nA deadline this package "+
					"wrote about itself already accounts for how slow that runner is, so "+
					"scaling double-counts it. Use withExactPromptTimeout.",
					name, fset.Position(call.Pos()).Line)
			}
			return true
		})
	}

	if scanned < 5 {
		t.Fatalf("scanned only %d files; the walk is broken", scanned)
	}
}

// scalerArgIndex gives the position of the timeoutScaler parameter in each of
// the two shared resolvers. boundPrompt delegates to promptTimeout, so a runner
// using either is resolving through the shared chain.
var scalerArgIndex = map[string]int{
	"promptTimeout": 0, // promptTimeout(scaler, agentDefault, cfg)
	"boundPrompt":   1, // boundPrompt(ctx, scaler, agentDefault, cfg)
}

// resolverCall returns the first call in body to one of the shared resolvers,
// its name, and the index of that resolver's timeoutScaler argument. A nil
// call means the body resolves its deadline some other way.
func resolverCall(body *ast.BlockStmt) (*ast.CallExpr, string, int) {
	var found *ast.CallExpr
	var name string
	ast.Inspect(body, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			if _, ok := scalerArgIndex[id.Name]; ok {
				found, name = call, id.Name
				return false
			}
		}
		return true
	})
	if found == nil {
		return nil, "", 0
	}
	return found, name, scalerArgIndex[name]
}

// receiverIdent returns the receiver's variable name (the "c" of "c *Claude"),
// or "" when the receiver is unnamed.
func receiverIdent(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 || len(fn.Recv.List[0].Names) == 0 {
		return ""
	}
	return fn.Recv.List[0].Names[0].Name
}

// exprString renders an argument for an error message, so a wrong scaler is
// named rather than merely reported as wrong.
func exprString(e ast.Expr) string {
	var buf strings.Builder
	if err := printer.Fprint(&buf, token.NewFileSet(), e); err != nil {
		return "<unprintable>"
	}
	return buf.String()
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
