//go:build linux

package strategy

import (
	"os"
	"strconv"
	"strings"
)

func processLookup() func(pid int) (ppid int, name string, ok bool) {
	return func(pid int) (int, string, bool) {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return 0, "", false
		}
		// "pid (comm) state ppid ...": comm may contain spaces and parens.
		s := string(data)
		open, closeIdx := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || closeIdx < open {
			return 0, "", false
		}
		fields := strings.Fields(s[closeIdx+1:])
		if len(fields) < 2 {
			return 0, "", false
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, "", false
		}
		return ppid, s[open+1 : closeIdx], true
	}
}
