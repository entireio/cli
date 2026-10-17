//go:build windows

package strategy

import (
	"context"
	"unsafe"

	"golang.org/x/sys/windows"
)

type winProc struct {
	ppid int
	name string
}

// processLookup snapshots the process table once, so walking several
// ancestors costs one snapshot.
func processLookup() func(pid int) (ppid int, name string, ok bool) {
	procs := map[int]winProc{}
	if snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0); err == nil {
		var e windows.ProcessEntry32
		e.Size = uint32(unsafe.Sizeof(e))
		for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
			procs[int(e.ProcessID)] = winProc{ppid: int(e.ParentProcessID), name: windows.UTF16ToString(e.ExeFile[:])}
		}
		windows.CloseHandle(snap) //nolint:errcheck,gosec // best effort
	}
	return func(pid int) (int, string, bool) {
		p, ok := procs[pid]
		return p.ppid, p.name, ok
	}
}

// waitProcessExit waits on the process handle until pid exits or ctx ends.
// The name is checked after opening the handle, so a PID already reused by
// another program is not waited on.
func waitProcessExit(ctx context.Context, pid int) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // PIDs fit in uint32
	if err != nil {
		return
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best effort
	if !isLiveGitProcess(pid) {
		return
	}
	for ctx.Err() == nil {
		ev, err := windows.WaitForSingleObject(h, 100)
		if err != nil || ev == windows.WAIT_OBJECT_0 {
			return
		}
	}
}
