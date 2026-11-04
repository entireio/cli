//go:build windows

package strategy

import (
	"context"
	"os"
	"strconv"
	"unsafe"

	"github.com/entireio/cli/cmd/entire/cli/proclive"

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

// processStartTime is pid's creation time as a FILETIME, or 0.
func processStartTime(pid int) int64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // PIDs fit in uint32
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best effort
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0
	}
	return int64(created.HighDateTime)<<32 | int64(created.LowDateTime)
}

// proclive cannot introspect Windows processes, so the push's git process is
// found and fingerprinted here: a Toolhelp snapshot for the ancestry and the
// creation FILETIME as the start fingerprint.

// pushingGit returns the `git push` running this pre-push hook: the nearest
// git ancestor within gitAncestorSearchDepth.
func pushingGit() (proclive.Identity, bool) {
	lookup := processLookup()
	pid := os.Getppid()
	for range gitAncestorSearchDepth {
		if pid <= 1 {
			return proclive.Identity{}, false
		}
		ppid, name, ok := lookup(pid)
		if !ok {
			return proclive.Identity{}, false
		}
		if isGitProcessName(name) {
			return windowsIdentity(pid, name), true
		}
		pid = ppid
	}
	return proclive.Identity{}, false
}

// gitProcessIdentity fingerprints pid when it is a running git process.
func gitProcessIdentity(pid int) (proclive.Identity, bool) {
	_, name, ok := processLookup()(pid)
	if !ok || !isGitProcessName(name) {
		return proclive.Identity{}, false
	}
	return windowsIdentity(pid, name), true
}

func windowsIdentity(pid int, name string) proclive.Identity {
	id := proclive.Identity{PID: pid, Name: name}
	if start := processStartTime(pid); start != 0 {
		id.Start = strconv.FormatInt(start, 10)
	}
	return id
}

// isLiveGitProcess reports whether id's git process is still running; its PID
// reused by another process (a different name or start time) is not.
func isLiveGitProcess(id proclive.Identity) bool {
	_, name, ok := processLookup()(id.PID)
	if !ok || !isGitProcessName(name) {
		return false
	}
	if id.Start == "" {
		return true
	}
	now := processStartTime(id.PID)
	return now == 0 || strconv.FormatInt(now, 10) == id.Start
}

// waitProcessExit waits on the process handle until id's process exits or ctx
// ends. Its identity is checked after opening the handle, so a PID already
// reused by another process is not waited on.
func waitProcessExit(ctx context.Context, id proclive.Identity) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(id.PID)) //nolint:gosec // PIDs fit in uint32
	if err != nil {
		return
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best effort
	if !isLiveGitProcess(id) {
		return
	}
	for ctx.Err() == nil {
		ev, err := windows.WaitForSingleObject(h, 100)
		if err != nil || ev == windows.WAIT_OBJECT_0 {
			return
		}
	}
}
