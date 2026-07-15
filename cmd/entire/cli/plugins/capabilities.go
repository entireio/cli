package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	lua "github.com/yuin/gopher-lua"
)

// installCapabilityAPI registers the privileged API surfaces (http, exec, fs,
// net) on the entire module. Every entry is capability-gated: calling one
// without the matching grant raises a Lua error naming the missing capability
// (fail loud, never silently no-op). Once granted, the API performs its action
// bounded by a timeout and, for fs, confined to the repo root and plugin data
// dir.
//
// The allow-list is the trust boundary: a plugin only reaches these APIs after
// the user explicitly enabled it AND granted the capability. The sandbox limits
// accidental damage and blast radius; it does not claim to fully contain a
// deliberately malicious allow-listed plugin.
func (p *LoadedPlugin) installCapabilityAPI(ls *lua.LState, mod *lua.LTable) {
	httpTbl := ls.NewTable()
	ls.SetField(httpTbl, "get", ls.NewFunction(p.luaHTTPGet))
	ls.SetField(httpTbl, "post", ls.NewFunction(p.luaHTTPPost))
	ls.SetField(mod, "http", httpTbl)

	execTbl := ls.NewTable()
	ls.SetField(execTbl, "run", ls.NewFunction(p.luaExecRun))
	ls.SetField(mod, "exec", execTbl)

	fsTbl := ls.NewTable()
	ls.SetField(fsTbl, "read", ls.NewFunction(p.luaFSRead))
	ls.SetField(fsTbl, "write", ls.NewFunction(p.luaFSWrite))
	ls.SetField(mod, "fs", fsTbl)

	// net is a coarse raw-network capability. entire.http is the supported
	// network path; net.connect is reserved and gated but not implemented, so a
	// plugin granting net still cannot open raw sockets in this build.
	netTbl := ls.NewTable()
	ls.SetField(netTbl, "connect", ls.NewFunction(func(l *lua.LState) int {
		if !p.requireCapability(l, settings.PluginCapabilityNet, "entire.net.connect") {
			return 0
		}
		l.RaiseError("entire.net.connect is not implemented; use entire.http")
		return 0
	}))
	ls.SetField(mod, "net", netTbl)
}

// requireCapability raises a Lua error and returns false when the plugin lacks
// capName. The error names the capability and how to grant it.
func (p *LoadedPlugin) requireCapability(ls *lua.LState, capName, apiName string) bool {
	if p.Grant.HasCapability(capName) {
		return true
	}
	ls.RaiseError("%s requires the %q capability, which plugin %q was not granted (add it to plugins.%s.capabilities in settings)",
		apiName, capName, p.Manifest.Name, p.Manifest.Name)
	return false
}

// capabilityContext derives a timeout-bounded context from the in-flight
// dispatch context so a capability call is bounded even though a blocking Go
// call inside an LGFunction is not interrupted by the Lua state's own context.
func (p *LoadedPlugin) capabilityContext(timeout capTimeout) (context.Context, context.CancelFunc) {
	parent := p.dispatchCtx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, timeout.dur())
}

// capTimeout is a named duration to keep call sites self-documenting.
type capTimeout time.Duration

func (c capTimeout) dur() time.Duration { return time.Duration(c) }

// --- http ---

func (p *LoadedPlugin) luaHTTPGet(ls *lua.LState) int {
	if !p.requireCapability(ls, settings.PluginCapabilityHTTP, "entire.http.get") {
		return 0
	}
	url := ls.CheckString(1)
	return p.doHTTP(ls, http.MethodGet, url, "", "")
}

func (p *LoadedPlugin) luaHTTPPost(ls *lua.LState) int {
	if !p.requireCapability(ls, settings.PluginCapabilityHTTP, "entire.http.post") {
		return 0
	}
	url := ls.CheckString(1)
	body := ls.OptString(2, "")
	contentType := ls.OptString(3, "application/json")
	return p.doHTTP(ls, http.MethodPost, url, body, contentType)
}

// doHTTP performs an HTTP request bounded by httpCapTimeout, limits the response
// body to httpMaxResponseBytes, and returns a Lua table {status, body}.
func (p *LoadedPlugin) doHTTP(ls *lua.LState, method, rawURL, body, contentType string) int {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		ls.RaiseError("entire.http: only http/https URLs are allowed, got %q", rawURL)
		return 0
	}
	ctx, cancel := p.capabilityContext(capTimeout(httpCapTimeout))
	defer cancel()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		ls.RaiseError("entire.http: build request: %v", err)
		return 0
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	client := &http.Client{Timeout: httpCapTimeout}
	resp, err := client.Do(req)
	if err != nil {
		ls.RaiseError("entire.http: request failed: %v", err)
		return 0
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, httpMaxResponseBytes))
	if err != nil {
		ls.RaiseError("entire.http: read response: %v", err)
		return 0
	}

	tbl := ls.NewTable()
	ls.SetField(tbl, "status", lua.LNumber(resp.StatusCode))
	ls.SetField(tbl, "body", lua.LString(string(data)))
	ls.Push(tbl)
	return 1
}

// --- exec ---

func (p *LoadedPlugin) luaExecRun(ls *lua.LState) int {
	if !p.requireCapability(ls, settings.PluginCapabilityExec, "entire.exec.run") {
		return 0
	}
	name := ls.CheckString(1)
	args := make([]string, 0, ls.GetTop()-1)
	for i := 2; i <= ls.GetTop(); i++ {
		args = append(args, ls.CheckString(i))
	}

	ctx, cancel := p.capabilityContext(capTimeout(execCapTimeout))
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	if p.WorktreeRoot != "" {
		cmd.Dir = p.WorktreeRoot
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	code := 0
	if runErr := cmd.Run(); runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			ls.RaiseError("entire.exec.run(%q): %v", name, runErr)
			return 0
		}
	}

	tbl := ls.NewTable()
	ls.SetField(tbl, "stdout", lua.LString(stdout.String()))
	ls.SetField(tbl, "stderr", lua.LString(stderr.String()))
	ls.SetField(tbl, "code", lua.LNumber(code))
	ls.Push(tbl)
	return 1
}

// --- fs ---

func (p *LoadedPlugin) luaFSRead(ls *lua.LState) int {
	if !p.requireCapability(ls, settings.PluginCapabilityFS, "entire.fs.read") {
		return 0
	}
	abs, err := p.resolveFSPath(ls.CheckString(1))
	if err != nil {
		ls.RaiseError("entire.fs.read: %v", err)
		return 0
	}
	data, err := os.ReadFile(abs) //nolint:gosec // path confined to repo root or plugin data dir by resolveFSPath
	if err != nil {
		ls.RaiseError("entire.fs.read: %v", err)
		return 0
	}
	if len(data) > fsMaxReadBytes {
		ls.RaiseError("entire.fs.read: file exceeds %d bytes", fsMaxReadBytes)
		return 0
	}
	ls.Push(lua.LString(string(data)))
	return 1
}

func (p *LoadedPlugin) luaFSWrite(ls *lua.LState) int {
	if !p.requireCapability(ls, settings.PluginCapabilityFS, "entire.fs.write") {
		return 0
	}
	abs, err := p.resolveFSPath(ls.CheckString(1))
	if err != nil {
		ls.RaiseError("entire.fs.write: %v", err)
		return 0
	}
	contents := ls.CheckString(2)
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		ls.RaiseError("entire.fs.write: %v", err)
		return 0
	}
	if err := os.WriteFile(abs, []byte(contents), 0o600); err != nil {
		ls.RaiseError("entire.fs.write: %v", err)
		return 0
	}
	return 0
}

// resolveFSPath confines a plugin-supplied path to the repo root or the
// plugin's data dir, rejecting traversal outside both. Relative paths resolve
// against the repo root when known, else the data dir.
func (p *LoadedPlugin) resolveFSPath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("empty path")
	}
	dataDir := ""
	if p.kv != nil {
		dataDir = p.kv.dir
	}

	if filepath.IsAbs(raw) {
		abs := filepath.Clean(raw)
		if p.WorktreeRoot != "" && paths.IsSubpath(p.WorktreeRoot, abs) {
			return abs, nil
		}
		if dataDir != "" && paths.IsSubpath(dataDir, abs) {
			return abs, nil
		}
		return "", fmt.Errorf("path %q is outside the repo root and plugin data dir", raw)
	}

	base := p.WorktreeRoot
	if base == "" {
		base = dataDir
	}
	if base == "" {
		return "", errors.New("no repo root or data dir to resolve a relative path")
	}
	abs := filepath.Clean(filepath.Join(base, raw))
	if !paths.IsSubpath(base, abs) {
		return "", fmt.Errorf("path %q escapes %s", raw, base)
	}
	return abs, nil
}
