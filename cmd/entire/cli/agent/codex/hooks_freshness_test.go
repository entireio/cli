package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
)

// releasedSessionStart is the SessionStart command released versions wrote on
// a native Windows host: the direct JSON-warning form, which does not parse
// under Codex's default PowerShell hook shell.
const releasedSessionStart = `where.exe entire >nul 2>nul & if errorlevel 1 (echo {^"systemMessage^":^"Entire CLI is enabled but not installed or not on PATH.^"}) else (entire hooks codex session-start)`

// legacyWindowsCommand turns a current Windows wrapper back into the form a
// version from before the PATH-only setting wrote: no `set …& pushd …& ` before
// the where.exe guard and no `popd& ` opening the branches. SessionStart gets
// the direct JSON-warning form those versions wrote.
func legacyWindowsCommand(command string) string {
	if strings.Contains(command, "entire hooks codex session-start") && strings.Contains(command, "where.exe entire") {
		return releasedSessionStart
	}
	command = strings.Replace(command, "set NoDefaultCurrentDirectoryInExePath=1& pushd %SystemRoot%& ", "", 1)
	return strings.ReplaceAll(command, "popd& ", "")
}

// rewriteStrings applies f to every string value in v, a decoded JSON value.
func rewriteStrings(v any, f func(string) string) any {
	switch x := v.(type) {
	case string:
		return f(x)
	case map[string]any:
		for k, e := range x {
			x[k] = rewriteStrings(e, f)
		}
	case []any:
		for i, e := range x {
			x[i] = rewriteStrings(e, f)
		}
	}
	return v
}

// TestCheckHookConfig_WindowsLegacyWrapperIsOutdated pins that `entire status`
// and `entire doctor` flag Codex hooks written before the PATH-only setting,
// both the nested silent wrappers and the direct JSON-warning SessionStart,
// and that the reinstall they point to clears the warning. Mutates the shared
// probe and the working directory, so no t.Parallel().
func TestCheckHookConfig_WindowsLegacyWrapperIsOutdated(t *testing.T) {
	tempDir := setupTestEnv(t)
	withCodexHookEnvironment(t, "windows", false)
	ctx := context.Background()
	ag := &CodexAgent{}

	if _, err := ag.InstallHooks(ctx, false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if got := ag.CheckHookConfig(ctx); got != agentpkg.HooksCurrent {
		t.Fatalf("after install: CheckHookConfig = %v, want current", got)
	}

	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	raw, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	rewritten := 0
	sessionStartRewritten := false
	doc = rewriteStrings(doc, func(s string) string {
		if legacy := legacyWindowsCommand(s); legacy != s {
			rewritten++
			sessionStartRewritten = sessionStartRewritten || strings.Contains(s, "entire hooks codex "+HookNameSessionStart)
			return legacy
		}
		return s
	})
	if rewritten == 0 {
		t.Fatal("no Windows wrapper found to rewrite; the install did not write the Windows form")
	}
	if !sessionStartRewritten {
		t.Fatal("SessionStart's wrapper was not rewritten to its legacy form")
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ag.CheckHookConfig(ctx); got != agentpkg.HooksOutdated {
		t.Fatalf("legacy Windows wrappers: CheckHookConfig = %v, want outdated", got)
	}

	if _, err := ag.InstallHooks(ctx, false); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if got := ag.CheckHookConfig(ctx); got != agentpkg.HooksCurrent {
		t.Fatalf("after reinstall: CheckHookConfig = %v, want current", got)
	}
	// The reinstall must have replaced every legacy wrapper, not added the
	// current one beside it.
	raw, err = os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	guards := 0
	rewriteStrings(doc, func(s string) string {
		guards += strings.Count(s, "where.exe entire")
		if n := strings.Count(s, "pushd %SystemRoot%& where.exe entire"); n != strings.Count(s, "where.exe entire") {
			t.Errorf("after reinstall, a legacy guard survived: %q", s)
		}
		return s
	})
	if guards != rewritten {
		t.Errorf("after reinstall: %d where.exe guards, want %d (one per rewritten wrapper)", guards, rewritten)
	}
}
