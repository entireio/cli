//go:build darwin

package strategy

import (
	"bytes"

	"golang.org/x/sys/unix"
)

func processLookup() func(pid int) (ppid int, name string, ok bool) {
	return func(pid int) (int, string, bool) {
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil || kp == nil || int(kp.Proc.P_pid) != pid {
			return 0, "", false
		}
		comm := kp.Proc.P_comm[:]
		if i := bytes.IndexByte(comm, 0); i >= 0 {
			comm = comm[:i]
		}
		return int(kp.Eproc.Ppid), string(bytes.TrimSpace(comm)), true
	}
}
