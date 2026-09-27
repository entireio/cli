//go:build windows

package agent

import "testing"

// The exact shapes the two producers emit on Windows. git prints a forward-slash
// toplevel; filepath.Join rewrites it with backslashes. Comparing them raw is
// what silently disabled allow_symlinked_agent_dirs on this platform.
func TestKeyFor_GitToplevelAndJoinedPathAgree(t *testing.T) {
	t.Parallel()

	fromGit := keyFor(`C:/Users/dev/repo`)
	fromJoin := keyFor(`C:\Users\dev\repo`)
	if fromGit != fromJoin {
		t.Errorf("keyFor(%q) = %q but keyFor(%q) = %q; one directory must produce one key",
			`C:/Users/dev/repo`, fromGit, `C:\Users\dev\repo`, fromJoin)
	}
	if mixedCase := keyFor(`c:\users\dev\REPO`); mixedCase != fromGit {
		t.Errorf("Windows paths are case-insensitive; got %q, want %q", mixedCase, fromGit)
	}
}
