//go:build !windows

package agent

// systemCmdExe names cmd.exe off Windows, where nothing runs it outside tests
// that substitute the Windows host. See syscmd_windows.go.
func systemCmdExe() string {
	return "cmd.exe"
}
