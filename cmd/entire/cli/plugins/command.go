package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	lua "github.com/yuin/gopher-lua"
)

// commandEntry is a CLI subcommand contributed by a plugin via entire.command.
type commandEntry struct {
	name   string
	short  string
	plugin string
	run    *lua.LFunction
}

// luaCommand implements entire.command{name=..., short=..., run=function(args) ... end}.
// It registers a subcommand invocable as `entire <name>`. The run callback
// receives a Lua array of the remaining CLI args and may return an integer exit
// code (nil/absent means 0).
func (p *LoadedPlugin) luaCommand(ls *lua.LState) int {
	tbl := ls.CheckTable(1)

	nameV := tbl.RawGetString("name")
	name, ok := nameV.(lua.LString)
	if !ok || string(name) == "" {
		ls.ArgError(1, "entire.command requires a string 'name'")
		return 0
	}
	if err := ValidatePluginName(string(name)); err != nil {
		ls.ArgError(1, fmt.Sprintf("entire.command name: %v", err))
		return 0
	}

	runV := tbl.RawGetString("run")
	run, ok := runV.(*lua.LFunction)
	if !ok {
		ls.ArgError(1, "entire.command requires a 'run' function")
		return 0
	}

	short := ""
	if s, ok := tbl.RawGetString("short").(lua.LString); ok {
		short = string(s)
	}

	p.commands[string(name)] = &commandEntry{
		name:   string(name),
		short:  short,
		plugin: p.Manifest.Name,
		run:    run,
	}
	return 0
}

// CommandInfo describes a plugin-contributed command for listing/help.
type CommandInfo struct {
	Name   string
	Short  string
	Plugin string
}

// Commands returns all plugin-contributed commands across the registry, sorted
// by name. When two plugins contribute the same name, the first in load order
// wins (matching RunCommand's resolution).
func (r *Registry) Commands() []CommandInfo {
	if r == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []CommandInfo
	for _, p := range r.plugins {
		for name, ce := range p.commands {
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, CommandInfo{Name: name, Short: ce.short, Plugin: ce.plugin})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FindCommand reports whether any plugin contributes a command with the given
// name (first in load order wins).
func (r *Registry) FindCommand(name string) (CommandInfo, bool) {
	if r == nil {
		return CommandInfo{}, false
	}
	for _, p := range r.plugins {
		if ce, ok := p.commands[name]; ok {
			return CommandInfo{Name: ce.name, Short: ce.short, Plugin: ce.plugin}, true
		}
	}
	return CommandInfo{}, false
}

// RunCommand runs the plugin command matching name with the given args and
// returns its exit code and whether a command was found. The command runs under
// the supplied context (cancellable via signal), not a short hook timeout, so
// interactive/long-running commands work. The first plugin (in load order)
// contributing the name wins.
func (r *Registry) RunCommand(ctx context.Context, name string, args []string) (exitCode int, found bool) {
	if r == nil {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.plugins {
		ce, ok := p.commands[name]
		if !ok {
			continue
		}
		return r.runCommand(ctx, p, ce, args), true
	}
	return 0, false
}

func (r *Registry) runCommand(ctx context.Context, p *LoadedPlugin, ce *commandEntry, args []string) (code int) {
	logCtx := logging.WithComponent(ctx, "plugins")
	p.dispatchCtx = ctx
	p.L.SetContext(ctx)
	defer p.L.SetContext(context.Background())
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "entire %s: plugin %q panicked\n", ce.name, p.Manifest.Name)
			code = 1
		}
	}()

	argTbl := p.L.NewTable()
	for _, a := range args {
		argTbl.Append(lua.LString(a))
	}
	if err := p.L.CallByParam(lua.P{Fn: ce.run, NRet: 1, Protect: true}, argTbl); err != nil {
		logging.Debug(logCtx, "plugin command failed",
			slog.String("plugin", p.Manifest.Name), slog.String("command", ce.name), slog.String("error", err.Error()))
		fmt.Fprintf(os.Stderr, "entire %s: %v\n", ce.name, err)
		return 1
	}
	ret := p.L.Get(-1)
	p.L.Pop(1)
	if n, ok := ret.(lua.LNumber); ok {
		return int(n)
	}
	return 0
}

// luaStdoutPrint returns an entire.print/entire.write binding that writes to the
// process stdout (for plugin commands' user-facing output). withNewline appends
// a trailing newline (print) vs not (write). Unlike the log-routed global
// print(), this reaches real stdout, so plugins should use it only in commands,
// not in hooks (where it could corrupt hook stdout).
func luaStdoutPrint(withNewline bool) lua.LGFunction {
	return func(ls *lua.LState) int {
		var sb strings.Builder
		top := ls.GetTop()
		for i := 1; i <= top; i++ {
			if i > 1 {
				sb.WriteByte('\t')
			}
			sb.WriteString(ls.ToStringMeta(ls.Get(i)).String())
		}
		if withNewline {
			sb.WriteByte('\n')
		}
		fmt.Fprint(os.Stdout, sb.String())
		return 0
	}
}
