//go:build !windows

package agent

// SystemCmdExe names cmd.exe off Windows, where nothing runs it outside tests
// that substitute the Windows host. See syscmd_windows.go.
func SystemCmdExe() string {
	return "cmd.exe"
}
