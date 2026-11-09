package agent

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// systemCmdExe returns the full path of the system cmd.exe, so Entire never
// starts a cmd.exe found in its working directory. os/exec's own lookup checks
// the working directory first and refuses a match there with ErrDot, which
// would let a repository's cmd.exe fail the process instead.
func systemCmdExe() string {
	dir, err := windows.GetSystemDirectory()
	if err != nil || dir == "" {
		return "cmd.exe"
	}
	return filepath.Join(dir, "cmd.exe")
}
