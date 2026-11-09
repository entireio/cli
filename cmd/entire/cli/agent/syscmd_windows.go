//go:build windows

package agent

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// defaultSystemCmdExe is used only if the system directory cannot be read.
const defaultSystemCmdExe = `C:\Windows\System32\cmd.exe`

// systemCmdExe returns the full path of the system cmd.exe, so Entire never
// starts a cmd.exe found in its working directory. os/exec's own lookup checks
// the working directory first and refuses a match there with ErrDot, which
// would let a repository's cmd.exe fail the process instead. It never returns
// a bare name: if the system directory cannot be read it falls back to the
// default system path.
func systemCmdExe() string {
	if dir, err := windows.GetSystemDirectory(); err == nil && dir != "" {
		return filepath.Join(dir, "cmd.exe")
	}
	return defaultSystemCmdExe
}
