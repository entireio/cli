//go:build windows

package cli

// pauseForks does nothing on Windows, which has no fork: CreateProcess builds
// the child's handle table explicitly, so it cannot copy a write handle to a
// file this process is in the middle of writing. There is no ETXTBSY race to
// close. syscall.ForkLock does exist on this platform, but the standard library
// declares it unused here ("ForkLock is not used on Windows",
// syscall/exec_windows.go), so taking it would serialize against nothing.
func pauseForks() (resume func()) { return func() {} }
