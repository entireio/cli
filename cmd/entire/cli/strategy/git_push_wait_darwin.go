//go:build darwin

package strategy

import (
	"bytes"

	"golang.org/x/sys/unix"
)

// sZomb is the kernel's p_stat for an exited process its parent has not yet
// reaped (SZOMB in sys/proc.h).
const sZomb = 5

func processLookup() func(pid int) (ppid int, name string, ok bool) {
	return func(pid int) (int, string, bool) {
		kp, ok := kinfoProc(pid)
		if !ok {
			return 0, "", false
		}
		comm := kp.Proc.P_comm[:]
		if i := bytes.IndexByte(comm, 0); i >= 0 {
			comm = comm[:i]
		}
		return int(kp.Eproc.Ppid), string(bytes.TrimSpace(comm)), true
	}
}

// processStartTime is pid's start time in microseconds since the epoch, or 0.
func processStartTime(pid int) int64 {
	kp, ok := kinfoProc(pid)
	if !ok {
		return 0
	}
	return kp.Proc.P_starttime.Sec*1_000_000 + int64(kp.Proc.P_starttime.Usec)
}

// kinfoProc returns pid's kernel record; a zombie has exited and is not found.
func kinfoProc(pid int) (*unix.KinfoProc, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || int(kp.Proc.P_pid) != pid || kp.Proc.P_stat == sZomb {
		return nil, false
	}
	return kp, true
}
