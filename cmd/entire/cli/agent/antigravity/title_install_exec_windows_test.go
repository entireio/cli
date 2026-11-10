package antigravity

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// TestWindowsTitleTeeCommand_IgnoresFolderPrograms installs the title tee, then
// runs the installed command the way agy runs its title slot on Windows: as
// one argument of `cmd.exe /C`, quoted by standard argument quoting (Go's
// os/exec quotes it the same way), from the folder agy was opened in. That
// folder holds a decoy entire.bat; the real stub is on PATH. The decoy must
// never run, and the stub must.
func TestWindowsTitleTeeCommand_IgnoresFolderPrograms(t *testing.T) {
	// No t.Parallel — uses t.Setenv.
	t.Setenv(configDirEnv, t.TempDir())
	if err := InstallTitleTee(); err != nil {
		t.Fatalf("InstallTitleTee: %v", err)
	}
	line := readTitleCommand(t, os.Getenv(configDirEnv))

	folder := t.TempDir()
	decoyMarker := filepath.Join(folder, "decoy-ran")
	writeBat(t, filepath.Join(folder, "entire.bat"), "@echo decoy>\""+decoyMarker+"\"\r\n@exit /b 0\r\n")

	stubDir := t.TempDir()
	realMarker := filepath.Join(stubDir, "real-ran")
	writeBat(t, filepath.Join(stubDir, "entire.bat"), "@echo real>\""+realMarker+"\"\r\n@exit /b 0\r\n")
	system32 := filepath.Dir(agent.SystemCmdExe())
	t.Setenv("PATH", stubDir+";"+system32)

	cmd := exec.CommandContext(t.Context(), agent.SystemCmdExe(), "/C", line)
	cmd.Dir = folder
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("title command %q failed: %v; output=%q", line, err, out.String())
	}
	if _, err := os.Stat(decoyMarker); err == nil {
		t.Fatalf("title command %q ran the folder's entire.bat", line)
	}
	if _, err := os.Stat(realMarker); err != nil {
		t.Fatalf("title command %q did not run entire from PATH (%v); output=%q", line, err, strings.TrimSpace(out.String()))
	}
}

func writeBat(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
