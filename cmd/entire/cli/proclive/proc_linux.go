//go:build linux

package proclive

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// procStat reads /proc/<pid>/stat and returns the parent PID, executable name
// (comm), and the process start time (field 22, in clock ticks since boot) used
// as the start fingerprint. The fingerprint only needs to be stable for the
// process lifetime and distinct across PID reuse within a boot; the boot guard
// in Check invalidates it across reboots, so raw ticks suffice and we avoid
// needing _SC_CLK_TCK.
func procStat(pid int) (ppid int, name, start string, err error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, "", "", errProcessGone
		}
		return 0, "", "", fmt.Errorf("proclive: read /proc/%d/stat: %w", pid, err)
	}
	return parseProcStat(string(data))
}

// parseProcStat parses the contents of /proc/<pid>/stat. It is separated from
// the file read so it can be unit-tested with adversarial comm values.
//
// The comm (field 2) is wrapped in parentheses and may itself contain spaces
// and ')'. Everything before the first '(' is the PID; the comm runs to the
// LAST ')'; the remaining space-separated fields begin at 'state' (field 3).
func parseProcStat(content string) (ppid int, name, start string, err error) {
	openIdx := strings.IndexByte(content, '(')
	closeIdx := strings.LastIndexByte(content, ')')
	if openIdx < 0 || closeIdx < 0 || closeIdx < openIdx {
		return 0, "", "", errors.New("proclive: malformed /proc stat: no comm parens")
	}
	name = content[openIdx+1 : closeIdx]

	// Fields after the comm, 0-indexed: 0=state (field 3), 1=ppid (field 4),
	// ... 19=starttime (field 22).
	const ppidIdx, starttimeIdx = 1, 19
	rest := strings.Fields(content[closeIdx+1:])
	if len(rest) <= starttimeIdx {
		return 0, "", "", errors.New("proclive: truncated /proc stat")
	}
	ppid, err = strconv.Atoi(rest[ppidIdx])
	if err != nil {
		return 0, "", "", fmt.Errorf("proclive: parse ppid: %w", err)
	}
	return ppid, name, rest[starttimeIdx], nil
}

// bootID returns the kernel boot id, which changes on every reboot. It falls
// back to /proc/stat's btime line if boot_id is unavailable.
func bootID() (string, error) {
	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return "", fmt.Errorf("proclive: read /proc/stat: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", nil
}
