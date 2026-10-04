//go:build darwin

package auth

import (
	"bytes"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// lookupProc reads a process's parent and start time from the kernel.
func lookupProc(pid int) (procInfo, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return procInfo{}, fmt.Errorf("kern.proc.pid %d: %w", pid, err)
	}
	st := kp.Proc.P_starttime
	comm := kp.Proc.P_comm[:]
	if i := bytes.IndexByte(comm, 0); i >= 0 {
		comm = comm[:i]
	}
	return procInfo{
		ppid:  int(kp.Eproc.Ppid),
		start: st.Sec*1_000_000 + int64(st.Usec),
		comm:  string(comm),
	}, nil
}

// lookupPeer reads the kernel's record of who is on the other end.
func lookupPeer(conn *net.UnixConn) (pid, uid int, err error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, fmt.Errorf("socket control: %w", err)
	}
	var opErr error
	ctlErr := raw.Control(func(fd uintptr) {
		pid, opErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if opErr != nil {
			return
		}
		cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			opErr = err
			return
		}
		uid = int(cred.Uid)
	})
	if ctlErr != nil {
		return 0, 0, fmt.Errorf("socket control: %w", ctlErr)
	}
	if opErr != nil {
		return 0, 0, fmt.Errorf("peer credentials: %w", opErr)
	}
	return pid, uid, nil
}
