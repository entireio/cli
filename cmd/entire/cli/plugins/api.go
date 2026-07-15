package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	lua "github.com/yuin/gopher-lua"
)

// entireModuleName is the global table plugins use to reach the host API.
const entireModuleName = "entire"

// installAPI registers the `entire` module (and a routed print) into the
// plugin's Lua state. Called once after the sandbox is built and before the
// entry script runs, so entire.on/entire.log are available during setup.
func (p *LoadedPlugin) installAPI(ls *lua.LState) {
	mod := ls.NewTable()

	ls.SetField(mod, "on", ls.NewFunction(p.luaOn))

	// Read-only accessors. Strings are copied by value into Lua, so a plugin
	// cannot mutate host state through them.
	ls.SetField(mod, "plugin_name", lua.LString(p.Manifest.Name))
	ls.SetField(mod, "version", lua.LString(p.Manifest.Version))
	ls.SetField(mod, "source", lua.LString(string(p.Source)))
	ls.SetField(mod, "repo_root", lua.LString(p.WorktreeRoot))
	if p.kv != nil {
		ls.SetField(mod, "data_dir", lua.LString(p.kv.dir))
	}

	logTbl := ls.NewTable()
	ls.SetField(logTbl, "debug", ls.NewFunction(p.luaLog(slog.LevelDebug)))
	ls.SetField(logTbl, "info", ls.NewFunction(p.luaLog(slog.LevelInfo)))
	ls.SetField(logTbl, "warn", ls.NewFunction(p.luaLog(slog.LevelWarn)))
	ls.SetField(logTbl, "error", ls.NewFunction(p.luaLog(slog.LevelError)))
	ls.SetField(mod, "log", logTbl)

	kvTbl := ls.NewTable()
	ls.SetField(kvTbl, "get", ls.NewFunction(p.luaKVGet))
	ls.SetField(kvTbl, "set", ls.NewFunction(p.luaKVSet))
	ls.SetField(kvTbl, "delete", ls.NewFunction(p.luaKVDelete))
	ls.SetField(mod, "kv", kvTbl)

	// Command contribution and stdout output (for plugin commands).
	ls.SetField(mod, "command", ls.NewFunction(p.luaCommand))
	ls.SetField(mod, "print", ls.NewFunction(luaStdoutPrint(true)))
	ls.SetField(mod, "write", ls.NewFunction(luaStdoutPrint(false)))

	p.installCapabilityAPI(ls, mod)

	ls.SetGlobal(entireModuleName, mod)

	// Re-provide print, routed to the plugin logger at info level, so authors
	// can debug with print() without corrupting hook stdout (the sandbox
	// stripped the stdout-writing base print).
	ls.SetGlobal("print", ls.NewFunction(func(l *lua.LState) int {
		top := l.GetTop()
		var sb strings.Builder
		for i := 1; i <= top; i++ {
			if i > 1 {
				sb.WriteByte('\t')
			}
			sb.WriteString(l.ToStringMeta(l.Get(i)).String())
		}
		p.log(slog.LevelInfo, sb.String())
		return 0
	}))
}

// luaOn implements entire.on(hook, callback): it records callback as a
// subscriber for the named hook. Unknown hook names are a hard error so a typo
// is caught at load time.
func (p *LoadedPlugin) luaOn(ls *lua.LState) int {
	hook := ls.CheckString(1)
	cb := ls.CheckFunction(2)
	if !IsKnownHook(hook) {
		ls.ArgError(1, fmt.Sprintf("unknown hook %q", hook))
		return 0
	}
	p.callbacks[hook] = append(p.callbacks[hook], cb)
	return 0
}

// luaLog returns an entire.log.<level> binding.
func (p *LoadedPlugin) luaLog(level slog.Level) lua.LGFunction {
	return func(ls *lua.LState) int {
		msg := ls.CheckString(1)
		p.log(level, msg)
		return 0
	}
}

// luaKVGet implements entire.kv.get(key) -> string|nil. It returns nil when
// the key is absent and raises a Lua error on a storage failure.
func (p *LoadedPlugin) luaKVGet(ls *lua.LState) int {
	key := ls.CheckString(1)
	if p.kv == nil {
		ls.Push(lua.LNil)
		return 1
	}
	v, ok, err := p.kv.get(key)
	if err != nil {
		ls.RaiseError("entire.kv.get(%q): %v", key, err)
		return 0
	}
	if !ok {
		ls.Push(lua.LNil)
		return 1
	}
	ls.Push(lua.LString(v))
	return 1
}

// luaKVSet implements entire.kv.set(key, value). value is coerced to a string.
func (p *LoadedPlugin) luaKVSet(ls *lua.LState) int {
	key := ls.CheckString(1)
	value := ls.CheckString(2)
	if p.kv == nil {
		ls.RaiseError("entire.kv is unavailable (no data dir resolved for plugin %q)", p.Manifest.Name)
		return 0
	}
	if err := p.kv.set(key, value); err != nil {
		ls.RaiseError("entire.kv.set(%q): %v", key, err)
	}
	return 0
}

// luaKVDelete implements entire.kv.delete(key).
func (p *LoadedPlugin) luaKVDelete(ls *lua.LState) int {
	key := ls.CheckString(1)
	if p.kv == nil {
		return 0
	}
	if err := p.kv.del(key); err != nil {
		ls.RaiseError("entire.kv.delete(%q): %v", key, err)
	}
	return 0
}

// log routes a plugin-authored message to the Entire log with the plugin's
// identity attached. The message text is carried as an attribute (not the log
// message) so log scraping stays keyed on the stable "lua plugin log" event.
func (p *LoadedPlugin) log(level slog.Level, msg string) {
	ctx := p.dispatchCtx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = logging.WithComponent(ctx, "plugins")
	attrs := []any{
		slog.String("plugin", p.Manifest.Name),
		slog.String("source", string(p.Source)),
		slog.String("message", msg),
	}
	switch level {
	case slog.LevelDebug:
		logging.Debug(ctx, "lua plugin log", attrs...)
	case slog.LevelWarn:
		logging.Warn(ctx, "lua plugin log", attrs...)
	case slog.LevelError:
		logging.Error(ctx, "lua plugin log", attrs...)
	case slog.LevelInfo:
		logging.Info(ctx, "lua plugin log", attrs...)
	default:
		logging.Info(ctx, "lua plugin log", attrs...)
	}
}
