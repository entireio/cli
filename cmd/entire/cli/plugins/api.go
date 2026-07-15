package plugins

import (
	"context"
	"fmt"
	"log/slog"

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
	ls.SetField(mod, "plugin_name", lua.LString(p.Manifest.Name))
	ls.SetField(mod, "version", lua.LString(p.Manifest.Version))

	logTbl := ls.NewTable()
	ls.SetField(logTbl, "debug", ls.NewFunction(p.luaLog(slog.LevelDebug)))
	ls.SetField(logTbl, "info", ls.NewFunction(p.luaLog(slog.LevelInfo)))
	ls.SetField(logTbl, "warn", ls.NewFunction(p.luaLog(slog.LevelWarn)))
	ls.SetField(logTbl, "error", ls.NewFunction(p.luaLog(slog.LevelError)))
	ls.SetField(mod, "log", logTbl)

	ls.SetGlobal(entireModuleName, mod)

	// Re-provide print, routed to the plugin logger at info level, so authors
	// can debug with print() without corrupting hook stdout (the sandbox
	// stripped the stdout-writing base print).
	ls.SetGlobal("print", ls.NewFunction(func(l *lua.LState) int {
		top := l.GetTop()
		msg := ""
		var msgSb40 strings.Builder
		for i := 1; i <= top; i++ {
			if i > 1 {
				msgSb40.WriteString("\t")
			}
			msgSb40.WriteString(l.ToStringMeta(l.Get(i)).String())
		}
		msg += msgSb40.String()
		p.log(slog.LevelInfo, msg)
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
