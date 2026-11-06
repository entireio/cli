//go:build linux

package proclive

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestParseProcStat_CommWithSpacesAndParens(t *testing.T) {
	t.Parallel()

	// comm contains both spaces and ')', which must NOT confuse field parsing.
	const comm = "weird) proc name"
	const wantPPID = 1000
	const wantStart = "987654"

	// Build the post-comm fields: index 0=state, 1=ppid, ..., 19=starttime.
	rest := make([]string, 20)
	for i := range rest {
		rest[i] = strconv.Itoa(i) // distinct filler so a wrong index is obvious
	}
	rest[0] = "R"
	rest[1] = strconv.Itoa(wantPPID)
	rest[19] = wantStart

	content := "4242 (" + comm + ") " + strings.Join(rest, " ") + "\n"

	ppid, name, start, err := parseProcStat(content)
	if err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	if name != comm {
		t.Errorf("name = %q, want %q", name, comm)
	}
	if ppid != wantPPID {
		t.Errorf("ppid = %d, want %d", ppid, wantPPID)
	}
	if start != wantStart {
		t.Errorf("start = %q, want %q", start, wantStart)
	}
}

func TestParseProcStat_Malformed(t *testing.T) {
	t.Parallel()
	for _, content := range []string{
		"no parens here",
		"123 (proc) R 1", // truncated: too few post-comm fields
		"",
	} {
		if _, _, _, err := parseProcStat(content); err == nil {
			t.Errorf("parseProcStat(%q) = nil error, want error", content)
		}
	}
}

func TestParseProcStat_ZombieIsGone(t *testing.T) {
	t.Parallel()
	const tail = " 4200 1 1 0 -1 4194560 100 0 0 0 0 0 0 0 20 0 1 0 98765 1000 100"
	for _, state := range []string{"Z", "X"} {
		if _, _, _, err := parseProcStat("4242 (git) " + state + tail); !errors.Is(err, errProcessGone) {
			t.Errorf("state %s: err = %v, want errProcessGone", state, err)
		}
	}
}
