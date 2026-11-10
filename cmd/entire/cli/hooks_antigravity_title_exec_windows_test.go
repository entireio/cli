package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// TestWindowsWrappedTitleCommand_ResolvesProgramsFromPath pins the one way the
// tee runs a user's preserved title command differently from agy: its cmd.exe
// resolves the programs that command names from PATH, never from the folder
// agy was opened in, even when the tee's own environment does not ask for it.
// The folder holds a decoy title-helper.bat; the real one is on PATH.
func TestWindowsWrappedTitleCommand_ResolvesProgramsFromPath(t *testing.T) {
	// No t.Parallel — uses t.Setenv.
	// Unset, not empty: Windows checks only whether the variable exists, so an
	// empty value would already stop cmd.exe searching the folder. t.Setenv
	// registers the restore.
	const key = "NoDefaultCurrentDirectoryInExePath"
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}

	folder := t.TempDir()
	decoyMarker := filepath.Join(folder, "decoy-ran")
	writeTitleBat(t, filepath.Join(folder, "title-helper.bat"), "@echo decoy>\""+decoyMarker+"\"\r\n@exit /b 0\r\n")

	stubDir := t.TempDir()
	realMarker := filepath.Join(stubDir, "real-ran")
	writeTitleBat(t, filepath.Join(stubDir, "title-helper.bat"), "@echo real>\""+realMarker+"\"\r\n@exit /b 0\r\n")
	t.Setenv("PATH", stubDir+";"+filepath.Dir(agent.SystemCmdExe()))

	cmd := wrappedTitleCommand(t.Context(), "title-helper")
	cmd.Dir = folder
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("wrapped title command failed: %v; output=%q", err, out.String())
	}
	if _, err := os.Stat(decoyMarker); err == nil {
		t.Fatal("wrapped title command ran the folder's title-helper.bat")
	}
	if _, err := os.Stat(realMarker); err != nil {
		t.Fatalf("wrapped title command did not run title-helper from PATH (%v); output=%q", err, out.String())
	}
}

func writeTitleBat(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
