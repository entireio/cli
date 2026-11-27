//go:build windows

package execx

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// detachFromTTY configures cmd to run without an inherited console so any
// /dev/tty-style probe in the child fails. Sets CREATE_NEW_PROCESS_GROUP and
// DETACHED_PROCESS so the child has no console and no signal coupling.
func detachFromTTY(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS
}

// detachBackgroundWorker configures cmd as a background worker whose own
// subprocesses stay invisible. A DETACHED_PROCESS worker has no console, so
// every console program it starts (git, for one) allocates a fresh console with
// a visible window. CREATE_NO_WINDOW instead gives the worker a console without
// a window, which its children inherit.
//
// The catch: CONIN$ still opens on that console, so a child that prompts hangs
// instead of flashing. Workers are safe today because they run only local git
// commands and Entire's own prompts go through
// interactive.CanPromptInteractively, which reports no window.
//
// Do not combine with NonInteractive on the same cmd: Windows ignores
// CREATE_NO_WINDOW when DETACHED_PROCESS is also set, so the worker would
// silently lose its console again.
func detachBackgroundWorker(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW
}

// killProcessGroupOnCancel terminates the process tree on ctx-cancel.
// exec.Cmd's default Cancel only kills the direct child, leaving any descendant
// (such as git-remote-https, an MCP server, or child filters) alive and holding
// output pipes open.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return KillProcessTree(cmd.Process.Pid)
	}
}

// KillProcessTree terminates the process with PID rootPid and all of its
// descendant processes on Windows. Descendants are discovered via a
// Toolhelp32 process snapshot and terminated bottom-up (leaves first) before
// terminating the root process.
func KillProcessTree(rootPid int) error {
	root := uint32(rootPid)
	if root == 0 {
		return nil
	}

	var rootCreationTime int64
	if hRoot, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, root); err == nil {
		rootCreationTime, _ = getProcessCreationTime(hRoot)
		_ = windows.CloseHandle(hRoot)
	}

	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		// Fallback to terminating the root process directly.
		if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, root); err == nil {
			_ = windows.TerminateProcess(h, 1)
			_ = windows.CloseHandle(h)
		}
		return err
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	if err := windows.Process32First(snapshot, &entry); err != nil {
		if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, root); err == nil {
			_ = windows.TerminateProcess(h, 1)
			_ = windows.CloseHandle(h)
		}
		return err
	}

	children := make(map[uint32][]uint32)
	for {
		children[entry.ParentProcessID] = append(children[entry.ParentProcessID], entry.ProcessID)
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}

	var descendants []uint32
	queue := []uint32{root}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		for _, child := range children[curr] {
			// Protect against PID reuse: a legitimate child was created >= root
			if rootCreationTime > 0 {
				if hChild, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, child); err == nil {
					cTime, err := getProcessCreationTime(hChild)
					_ = windows.CloseHandle(hChild)
					if err == nil && cTime < rootCreationTime {
						continue // Stale or reused PID from before root started
					}
				}
			}
			descendants = append(descendants, child)
			queue = append(queue, child)
		}
	}

	// Terminate descendants bottom-up (deepest children first)
	for i := len(descendants) - 1; i >= 0; i-- {
		pid := descendants[i]
		if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid); err == nil {
			_ = windows.TerminateProcess(h, 1)
			_ = windows.CloseHandle(h)
		}
	}

	// Terminate root process
	if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, root); err == nil {
		_ = windows.TerminateProcess(h, 1)
		_ = windows.CloseHandle(h)
	}
	return nil
}

func getProcessCreationTime(h windows.Handle) (int64, error) {
	var cTime, eTime, kTime, uTime windows.Filetime
	if err := windows.GetProcessTimes(h, &cTime, &eTime, &kTime, &uTime); err != nil {
		return 0, err
	}
	return cTime.Nanoseconds(), nil
}
