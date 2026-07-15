package plugins

import (
	"github.com/entireio/cli/cmd/entire/cli/settings"
	lua "github.com/yuin/gopher-lua"
)

// installCapabilityStubs registers the privileged API surfaces (http, exec, fs,
// net) on the entire module. In this phase every entry is a capability gate:
// calling one without the matching grant raises a Lua error naming the missing
// capability; with the grant it raises a "not implemented yet" error. Phase 3
// replaces the granted branch with the real implementation while keeping the
// same denial semantics, so plugins written now fail loudly (never silently
// no-op) when a capability is missing.
func (p *LoadedPlugin) installCapabilityStubs(ls *lua.LState, mod *lua.LTable) {
	http := ls.NewTable()
	ls.SetField(http, "get", ls.NewFunction(p.capabilityStub(settings.PluginCapabilityHTTP, "entire.http.get")))
	ls.SetField(http, "post", ls.NewFunction(p.capabilityStub(settings.PluginCapabilityHTTP, "entire.http.post")))
	ls.SetField(mod, "http", http)

	exec := ls.NewTable()
	ls.SetField(exec, "run", ls.NewFunction(p.capabilityStub(settings.PluginCapabilityExec, "entire.exec.run")))
	ls.SetField(mod, "exec", exec)

	fsTbl := ls.NewTable()
	ls.SetField(fsTbl, "read", ls.NewFunction(p.capabilityStub(settings.PluginCapabilityFS, "entire.fs.read")))
	ls.SetField(fsTbl, "write", ls.NewFunction(p.capabilityStub(settings.PluginCapabilityFS, "entire.fs.write")))
	ls.SetField(mod, "fs", fsTbl)

	net := ls.NewTable()
	ls.SetField(net, "connect", ls.NewFunction(p.capabilityStub(settings.PluginCapabilityNet, "entire.net.connect")))
	ls.SetField(mod, "net", net)
}

// capabilityStub returns a Lua function that enforces the named capability and
// then reports the API as not yet implemented. The two-stage error keeps the
// denial path (which is the security-relevant contract) authoritative now,
// while the granted path is filled in later.
func (p *LoadedPlugin) capabilityStub(capName, apiName string) lua.LGFunction {
	return func(ls *lua.LState) int {
		if !p.hasCapability(capName) {
			ls.RaiseError("%s requires the %q capability, which plugin %q was not granted (add it to plugins.%s.capabilities in settings)",
				apiName, capName, p.Manifest.Name, p.Manifest.Name)
			return 0
		}
		ls.RaiseError("%s is not implemented yet", apiName)
		return 0
	}
}

// hasCapability reports whether the plugin's allow-list grant includes capName.
func (p *LoadedPlugin) hasCapability(capName string) bool {
	return p.Grant.HasCapability(capName)
}
