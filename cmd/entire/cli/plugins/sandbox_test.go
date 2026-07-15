package plugins

import (
	"context"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

func TestNewSandboxedState_CuratedLibsPresent(t *testing.T) {
	t.Parallel()
	L := NewSandboxedState(context.Background())
	defer L.Close()

	// base, string, table, math must be usable.
	scripts := []string{
		`assert(type(tostring) == "function")`,
		`assert(("abc"):upper() == "ABC")`,
		`assert(type(string.format) == "function")`,
		`assert(type(table.insert) == "function")`,
		`assert(math.floor(1.9) == 1)`,
	}
	for _, s := range scripts {
		if err := L.DoString(s); err != nil {
			t.Fatalf("curated lib script failed: %q: %v", s, err)
		}
	}
}

func TestNewSandboxedState_EscapeHatchesRemoved(t *testing.T) {
	t.Parallel()
	L := NewSandboxedState(context.Background())
	defer L.Close()

	// os and io libs are never opened; the escape-hatch base globals are unset.
	removed := []string{
		"os", "io", "package", "require", "dofile", "loadfile",
		"load", "loadstring", "module", "collectgarbage", "newproxy", "debug",
	}
	for _, name := range removed {
		if err := L.DoString("assert(" + name + " == nil)"); err != nil {
			t.Errorf("expected global %q to be nil in sandbox: %v", name, err)
		}
	}
}

func TestNewSandboxedState_ContextTimeoutAbortsRunaway(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	L := NewSandboxedState(ctx)
	defer L.Close()

	start := time.Now()
	err := L.DoString(`while true do end`)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected infinite loop to be aborted by context timeout, got nil error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("context timeout took too long to abort: %v", elapsed)
	}
}

func TestNewSandboxedState_BackgroundContextOK(t *testing.T) {
	t.Parallel()
	L := NewSandboxedState(context.Background())
	defer L.Close()
	if err := L.DoString(`return 1 + 1`); err != nil {
		t.Fatalf("background-context sandbox should run: %v", err)
	}
}

func BenchmarkFireObserverEmptyHook(b *testing.B) {
	// Measures dispatch overhead for a registered but trivial callback — the
	// decision-gate latency number for wiring hooks into the commit path.
	L := NewSandboxedState(context.Background())
	defer L.Close()
	var called int
	fn := L.NewFunction(func(*lua.LState) int { called++; return 0 })

	p := &LoadedPlugin{
		Manifest:  Manifest{Name: "bench"},
		Source:    SourceUser,
		L:         L,
		callbacks: map[string][]*lua.LFunction{HookTurnEnd: {fn}},
	}
	r := &Registry{}
	r.Add(p)

	ctx := context.Background()
	payload := map[string]any{"session_id": "s1"}
	b.ResetTimer()
	for range b.N {
		r.FireObserver(ctx, HookTurnEnd, payload)
	}
	b.StopTimer()
	if called == 0 {
		b.Fatal("callback never fired")
	}
}
