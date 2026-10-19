//go:build linux

package strategy

import (
	"os"
	"strconv"
)

func processLookup() func(pid int) (ppid int, name string, ok bool) {
	return func(pid int) (int, string, bool) {
		ppid, comm, _, ok := readProcStat(pid)
		return ppid, comm, ok
	}
}

// processStartTime is pid's start time in clock ticks since boot, or 0.
func processStartTime(pid int) int64 {
	_, _, start, ok := readProcStat(pid)
	if !ok {
		return 0
	}
	return start
}

func readProcStat(pid int) (ppid int, comm string, start int64, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, "", 0, false
	}
	return parseProcStat(string(data))
}
