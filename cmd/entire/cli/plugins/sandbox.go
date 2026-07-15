package plugins

import (
	"context"

	lua "github.com/yuin/gopher-lua"
)

// Sandbox tuning. The call-stack and registry sizes bound a runaway plugin's
// memory before the context deadline trips. They are deliberately modest:
// plugins are event callbacks, not long-running programs.
const (
	sandboxCallStackSize = 120
	sandboxRegistrySize  = 1024 * 8
)

// curatedLibs is the allow-list of standard libraries opened in a sandboxed
// state: base, string, table, and math only. os, io, package (require), debug,
// coroutine, and channel are intentionally omitted — they are the escape
// hatches to the host filesystem, process, and module loader.
var curatedLibs = []struct {
	name string
	open lua.LGFunction
}{
	{lua.BaseLibName, lua.OpenBase},
	{lua.TabLibName, lua.OpenTable},
	{lua.StringLibName, lua.OpenString},
	{lua.MathLibName, lua.OpenMath},
}

// strippedBaseGlobals are base-library globals removed after OpenBase runs.
// They can load bytecode/source or reach the host outside the curated set, so
// they are unset even though os/io/package were never opened (dofile/loadfile
// use Go's os package directly regardless of the io/os Lua libs). print is
// stripped here and re-provided by the API layer so it routes to the plugin
// logger instead of the process stdout, which would corrupt hook output.
var strippedBaseGlobals = []string{
	"dofile",
	"loadfile",
	"load",
	"loadstring",
	"require",
	"module",
	"collectgarbage",
	"newproxy",
	"print",
	"_printregs",
}

// NewSandboxedState builds a Lua state with only the curated standard library
// and the host escape hatches removed. When ctx is non-nil it is attached via
// SetContext, so a context deadline/cancellation aborts a running script
// between VM instructions (the per-hook timeout mechanism).
//
// The returned state does NOT yet expose the `entire` module; callers layer
// that on via the API installer so it can capture per-plugin state.
func NewSandboxedState(ctx context.Context) *lua.LState {
	L := lua.NewState(lua.Options{
		SkipOpenLibs:        true,
		CallStackSize:       sandboxCallStackSize,
		RegistrySize:        sandboxRegistrySize,
		IncludeGoStackTrace: false,
	})

	// Open only the curated libraries. Each open function is called as a Lua
	// function with the library name argument, mirroring linit.go's loader.
	for _, lib := range curatedLibs {
		L.Push(L.NewFunction(lib.open))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}

	for _, name := range strippedBaseGlobals {
		L.SetGlobal(name, lua.LNil)
	}

	if ctx != nil {
		L.SetContext(ctx)
	}
	return L
}
