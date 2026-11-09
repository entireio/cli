//go:build windows

package execx

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// treeKillPasses bounds how many times Cancel re-reads the process table for
// descendants that were started while it was killing the ones it had found.
const treeKillPasses = 3

// killProcessGroupOnCancel makes ctx-cancel terminate cmd's process and every
// descendant it can prove is one. Windows has no process group to signal, and
// a process's children keep running when it dies, so Cancel walks the process
// table instead, as taskkill /T does.
//
// A Job Object would also catch a descendant whose own parent has already
// exited, but a process can only join one race-free while it is suspended,
// and exec.Cmd has no step between creating the process and running it.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killProcessTree(cmd.Process)
	}
}

// killProcessTree terminates p, then its descendants. It runs inside
// WithHandle so p's PID keeps naming p for the whole walk, even if p has
// already exited.
func killProcessTree(p *os.Process) error {
	pid, ok := toPID(p.Pid)
	if !ok {
		return p.Kill() //nolint:wrapcheck // the default Cancel's error, unchanged
	}
	var killErr error
	if err := p.WithHandle(func(handle uintptr) {
		killErr = terminateTree(pid, windows.Handle(handle))
	}); err != nil {
		// Wait has already reaped p. Nothing is left to kill, and p's PID may
		// already name an unrelated process, so it proves nothing about the
		// processes that list it as their parent.
		return os.ErrProcessDone
	}
	return killErr
}

// terminateTree terminates root, then every process that descends from it.
// root's descendants are killed even when root has already exited: a child
// that exits after starting a grandchild leaves the grandchild holding the
// output pipe, which is the case this exists for.
func terminateTree(rootPID uint32, root windows.Handle) error {
	var killErr error
	if err := windows.TerminateProcess(root, 1); err != nil && !hasExited(root) {
		killErr = fmt.Errorf("terminate process %d: %w", rootPID, err)
	}
	created, err := creationTime(root)
	if err != nil {
		// Without root's creation time no process can be proven to be its child.
		return killErr
	}

	tree := map[uint32]int64{rootPID: created}
	var opened []windows.Handle
	defer func() {
		for _, h := range opened {
			_ = windows.CloseHandle(h) //nolint:errcheck // closing a handle we opened; nothing to recover
		}
	}()
	for range treeKillPasses {
		entries, err := processTable()
		if err != nil {
			break
		}
		found := findDescendants(entries, tree, openProcess, closeProcess)
		if len(found) == 0 {
			break
		}
		for _, proc := range found {
			// Keep every handle open until the walk ends: it pins the PID, so
			// the next pass can still prove which processes this one started.
			opened = append(opened, proc.handle)
			// A descendant that exits on its own first cannot be terminated,
			// which is the outcome we wanted anyway.
			_ = windows.TerminateProcess(proc.handle, 1) //nolint:errcheck // best effort, see above
		}
	}
	return killErr
}

// processEntry is one row of the process table: a PID and the PID that was
// its parent when it was created.
type processEntry struct {
	pid  uint32
	ppid uint32
}

// openedProc is a process opened by PID, with what the kernel reports for that
// handle. While the handle is open, the PID cannot be reused.
type openedProc struct {
	handle  windows.Handle
	ppid    uint32
	created int64 // creation time, in 100ns ticks since 1601
}

// findDescendants returns every process in entries that descends from a
// member of tree and is not one yet, parents before children, and adds each to
// tree. tree maps a PID to its process's creation time, and each PID in it must
// stay pinned by an open handle while findDescendants runs.
//
// A table entry's parent PID only says where to look. Windows keeps a process's
// parent PID after the parent exits, and the PID can then be reused, so a
// process listing a member's PID may have been started by an earlier holder of
// that PID. Each candidate is opened, which pins its PID, and kept only if the
// kernel reports a member as its parent for that handle and it was created no
// earlier than that member. A member has held its PID since its own creation,
// so any process created later that names that PID as parent was started by it.
func findDescendants(entries []processEntry, tree map[uint32]int64, open func(uint32) (openedProc, error), release func(openedProc)) []openedProc {
	children := make(map[uint32][]uint32)
	for _, e := range entries {
		if e.pid != 0 && e.pid != e.ppid {
			children[e.ppid] = append(children[e.ppid], e.pid)
		}
	}
	queue := make([]uint32, 0, len(tree))
	for pid := range tree {
		queue = append(queue, pid)
	}
	var found []openedProc
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, pid := range children[parent] {
			if _, member := tree[pid]; member {
				continue
			}
			proc, err := open(pid)
			if err != nil {
				continue // already gone, or not ours to open
			}
			if proc.ppid != parent || proc.created < tree[parent] {
				release(proc)
				continue
			}
			tree[pid] = proc.created
			found = append(found, proc)
			queue = append(queue, pid)
		}
	}
	return found
}

// processTable lists every process's PID and parent PID.
func processTable() ([]processEntry, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("snapshot processes: %w", err)
	}
	defer windows.CloseHandle(snapshot) //nolint:errcheck // read-only snapshot handle

	var entries []processEntry
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		entries = append(entries, processEntry{pid: entry.ProcessID, ppid: entry.ParentProcessID})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("read process snapshot: %w", err)
	}
	return entries, nil
}

// openProcess opens pid for termination and reads its parent PID and
// creation time through the new handle.
func openProcess(pid uint32) (openedProc, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return openedProc{}, fmt.Errorf("open process %d: %w", pid, err)
	}
	proc := openedProc{handle: handle}
	if proc.ppid, err = parentPID(handle); err == nil {
		proc.created, err = creationTime(handle)
	}
	if err != nil {
		closeProcess(proc)
		return openedProc{}, err
	}
	return proc, nil
}

func closeProcess(proc openedProc) {
	_ = windows.CloseHandle(proc.handle) //nolint:errcheck // closing a handle we opened; nothing to recover
}

// parentPID returns the PID that was handle's parent when it was created.
func parentPID(handle windows.Handle) (uint32, error) {
	var info windows.PROCESS_BASIC_INFORMATION
	//nolint:gosec // G103: x/sys has no typed wrapper; the kernel fills info, bounded by its size
	if err := windows.NtQueryInformationProcess(handle, windows.ProcessBasicInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return 0, fmt.Errorf("query parent process: %w", err)
	}
	ppid, ok := toPID(int(info.InheritedFromUniqueProcessId))
	if !ok {
		return 0, fmt.Errorf("parent process id %d out of range", info.InheritedFromUniqueProcessId)
	}
	return ppid, nil
}

// creationTime returns when handle's process was created, in 100ns ticks
// since 1601.
func creationTime(handle windows.Handle) (int64, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return 0, fmt.Errorf("read process times: %w", err)
	}
	return int64(created.HighDateTime)<<32 | int64(created.LowDateTime), nil
}

// hasExited reports whether handle's process has already exited.
func hasExited(handle windows.Handle) bool {
	event, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && event == windows.WAIT_OBJECT_0
}

// toPID converts a Go PID to the DWORD Windows uses.
func toPID(pid int) (uint32, bool) {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return 0, false
	}
	return uint32(pid), true
}
