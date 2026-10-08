//go:build darwin

package proclive

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// procStat looks up a process via sysctl(kern.proc.pid) and returns its parent
// PID, executable name (comm), and start time as the fingerprint. It uses the
// typed KinfoProc decoder from golang.org/x/sys/unix rather than hand-decoding
// raw sysctl bytes. p_starttime is an absolute wall-clock timeval, so it is a
// stable per-process fingerprint without needing the boot guard.
func procStat(pid int) (ppid int, name, start string, err error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// A missing process surfaces as ESRCH or as EIO (sysctl returns a
		// zero-length result, which the wrapper rejects). Either way it's gone.
		if err == unix.ESRCH || err == unix.EIO || err == unix.ENOENT {
			return 0, "", "", errProcessGone
		}
		return 0, "", "", fmt.Errorf("proclive: sysctl kern.proc.pid %d: %w", pid, err)
	}
	tv := k.Proc.P_starttime
	return int(k.Eproc.Ppid),
		unix.ByteSliceToString(k.Proc.P_comm[:]),
		fmt.Sprintf("%d.%06d", tv.Sec, tv.Usec),
		nil
}

// bootID returns no boot guard on darwin. kern.boottime is NOT stable for a
// running machine — the kernel recomputes it whenever the wall clock is stepped
// (e.g. an NTP correction), so using it would let a clock adjustment falsely
// declare a still-running session dead. It is also unnecessary here: darwin's
// P_starttime fingerprint is an absolute wall-clock timestamp fixed at process
// creation, so it already distinguishes a reused PID across reboots without a
// boot guard. (Linux uses ticks-since-boot, which does need the guard.)
func bootID() (string, error) {
	return "", nil
}
