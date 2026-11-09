//go:build windows

package proclive

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code Windows reports for a process that has not
// exited (STILL_ACTIVE).
const stillActive = 259

// noLivingParent is the parent PID procStat reports when a process's recorded
// parent has exited, or its PID now names a process created after the child.
// It is init's PID, where walkAncestry ends a complete chain. Windows never
// assigns PID 1 (its process IDs are multiples of 4), and has no init a walk
// could reach: Windows keeps a process's parent PID after the parent exits.
// So the chain up to the first process without a living parent is every
// living ancestor, which is what a complete chain has to hold.
const noLivingParent = 1

// procStat opens pid and returns its parent PID, executable name, and creation
// time as the start fingerprint. Creation time is an absolute timestamp fixed
// when the process starts, so, as on darwin, it tells a reused PID apart
// across reboots without a boot guard.
//
// An exited process whose handle another process still holds counts as gone:
// Windows keeps such a process, and its PID, until the last handle closes.
func procStat(pid int) (ppid int, name, start string, err error) {
	handle, err := openProcess(pid)
	if err != nil {
		return 0, "", "", err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck // query-only handle

	proc, err := inspect(handle)
	if err != nil {
		return 0, "", "", fmt.Errorf("proclive: process %d: %w", pid, err)
	}
	if proc.exited {
		return 0, "", "", errProcessGone
	}
	name, err = imageName(handle)
	if err != nil {
		return 0, "", "", fmt.Errorf("proclive: process %d: %w", pid, err)
	}
	return livingParent(proc), name, strconv.FormatInt(proc.created, 10), nil
}

// bootID returns no boot guard on Windows: the creation-time fingerprint is
// already absolute, as on darwin.
func bootID() (string, error) {
	return "", nil
}

// process is what procStat reads from a process handle.
type process struct {
	ppid    int
	created int64 // creation time, in 100ns ticks since 1601
	exited  bool
}

// livingParent returns proc's parent PID, or noLivingParent when that parent
// has exited or the PID now names a newer process. A parent it cannot open for
// another reason (access denied, typically another user's process) is
// returned unverified, so the walk stops there and stays incomplete.
func livingParent(proc process) int {
	handle, err := openProcess(proc.ppid)
	if errors.Is(err, errProcessGone) {
		return noLivingParent
	}
	if err != nil {
		return proc.ppid
	}
	defer windows.CloseHandle(handle) //nolint:errcheck // query-only handle

	parent, err := inspect(handle)
	switch {
	case err != nil:
		return proc.ppid
	case parent.exited, parent.created > proc.created:
		return noLivingParent
	default:
		return proc.ppid
	}
}

// openProcess opens pid for querying. It returns errProcessGone when no
// process holds pid.
func openProcess(pid int) (windows.Handle, error) {
	if pid <= 0 || uint64(pid) > math.MaxUint32 {
		return 0, errProcessGone
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return 0, errProcessGone
	}
	if err != nil {
		return 0, fmt.Errorf("proclive: open process %d: %w", pid, err)
	}
	return handle, nil
}

// inspect reads handle's parent PID, creation time, and whether it exited.
func inspect(handle windows.Handle) (process, error) {
	var info windows.PROCESS_BASIC_INFORMATION
	//nolint:gosec // G103: x/sys has no typed wrapper; the kernel fills info, bounded by its size
	if err := windows.NtQueryInformationProcess(handle, windows.ProcessBasicInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return process{}, fmt.Errorf("query parent: %w", err)
	}
	var created, exitedAt, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exitedAt, &kernel, &user); err != nil {
		return process{}, fmt.Errorf("read times: %w", err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return process{}, fmt.Errorf("read exit code: %w", err)
	}
	return process{
		ppid:    int(info.InheritedFromUniqueProcessId),
		created: int64(created.HighDateTime)<<32 | int64(created.LowDateTime),
		// A process that exits with STILL_ACTIVE as its code reads as running
		// for as long as a handle keeps it; once the last closes, it is gone.
		exited: code != stillActive,
	}, nil
}

// imageName returns the base name of handle's executable, such as "node.exe".
func imageName(handle windows.Handle) (string, error) {
	for _, capacity := range []uint32{windows.MAX_PATH, windows.MAX_LONG_PATH} {
		buf := make([]uint16, capacity)
		size := capacity
		err := windows.QueryFullProcessImageName(handle, 0, &buf[0], &size)
		if err == nil {
			return filepath.Base(windows.UTF16ToString(buf[:size])), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return "", fmt.Errorf("read image name: %w", err)
		}
	}
	return "", errors.New("read image name: longer than the longest Windows path")
}
