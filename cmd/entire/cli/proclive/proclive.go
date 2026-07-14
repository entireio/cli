// Package proclive captures a process's identity (PID plus a start-time
// fingerprint) and later reports whether that exact process is still alive.
//
// It exists to detect agent sessions left in an ACTIVE state when the owning
// process went away — a clean exit, a crash, a kill, a closed terminal, or a
// reboot — without firing a SessionStop hook. Recording the owner's identity at
// turn start lets `entire status` / `entire doctor` notice the process is gone
// immediately, instead of waiting out a coarse inactivity timeout.
//
// This package is a leaf: it imports only the standard library and
// golang.org/x/sys/unix. It must NOT import session, strategy, agent, or cli,
// so those packages can depend on it without an import cycle.
package proclive

import (
	"errors"
	"os"
	"strings"
)

// Liveness is the result of checking a recorded process Identity.
type Liveness int

const (
	// LivenessUnknown means liveness could not be determined: the identity is
	// empty, was recorded on another host, or the platform cannot introspect
	// processes. Callers should fall back to a time-based heuristic.
	LivenessUnknown Liveness = iota
	// LivenessAlive means the recorded process is still running.
	LivenessAlive
	// LivenessDead means the recorded process is gone (exited, killed, or the
	// machine rebooted) or its PID has been reused by a different process.
	LivenessDead
)

func (l Liveness) String() string {
	switch l {
	case LivenessAlive:
		return "alive"
	case LivenessDead:
		return "dead"
	case LivenessUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Identity fingerprints the process that owns a session turn. It is persisted
// in session state and later passed to Check. The zero value means "no owner
// recorded" and always yields LivenessUnknown.
type Identity struct {
	// PID is the operating-system process id of the owner.
	PID int `json:"pid"`
	// Start is an opaque, per-platform process start-time fingerprint. It need
	// only be stable for the process lifetime and distinct across PID reuse
	// within a single boot; the Boot guard invalidates it across reboots.
	Start string `json:"start"`
	// Boot identifies the current OS boot. A mismatch at check time means the
	// machine rebooted, so the recorded PID cannot still be the same process.
	Boot string `json:"boot,omitempty"`
	// Host is the hostname where the identity was recorded. PIDs are only
	// meaningful on their own machine, so a mismatch yields Unknown.
	Host string `json:"host,omitempty"`
	// Name is the owning process's executable name (comm). Diagnostic only.
	Name string `json:"name,omitempty"`
}

var (
	// errProcessGone is returned by procStat when no process with the given PID
	// exists. Check maps it to LivenessDead.
	errProcessGone = errors.New("proclive: process not found")
	// errUnsupported is returned by the per-platform seam when the OS cannot be
	// introspected (e.g. Windows). Check maps it to LivenessUnknown.
	errUnsupported = errors.New("proclive: unsupported platform")
)

// maxAncestorDepth bounds the ResolveOwner walk so a pathological or cyclic
// process tree can never loop or hang.
const maxAncestorDepth = 12

// transientNames are process names that are never the long-lived session owner:
// our own hook binary, the shells agents commonly use to exec hooks, and the Go
// toolchain (local-dev runs hooks via `go run`, whose short-lived `go` parent
// would otherwise be recorded as the owner and exit immediately). The walk skips
// past these to reach the real agent process. Note that interpreter runtimes
// (node, bun, python) are deliberately absent — for several agents the runtime
// IS the long-lived agent, so treating it as transient would skip the real owner.
var transientNames = map[string]bool{
	"entire": true,
	"sh":     true,
	"bash":   true,
	"zsh":    true,
	"dash":   true,
	"fish":   true,
	"ash":    true,
	"ksh":    true,
	"env":    true,
	"go":     true,
}

func isTransient(name string) bool {
	return transientNames[strings.ToLower(strings.TrimSpace(name))]
}

// ResolveOwner walks up the process tree from the current process and returns
// the Identity of the first ancestor that is not our own hook binary or a
// shell — i.e. the long-lived agent that owns this session.
//
// It returns (zero, false) when no such ancestor can be determined: an
// unsupported platform, a truncated/looping tree, or only transient ancestors.
// In that case the caller should record no owner and let liveness degrade to
// the time-based fallback. Resolving to nothing is always safer than recording
// a guessed PID, which could later be (mis)read as a live or dead owner.
func ResolveOwner() (Identity, bool) {
	// The host guard is essential — a PID is only meaningful on the machine that
	// recorded it — so if the hostname can't be determined, record no owner
	// rather than an unguarded one that Check could later (mis)classify as
	// alive/dead across machines. Boot is a best-effort secondary guard; an
	// empty value just disables it (darwin records none — see bootID there).
	host, err := os.Hostname()
	if err != nil || host == "" {
		return Identity{}, false
	}
	boot, err := bootID()
	if err != nil {
		boot = ""
	}

	// Walk up from our own process, reading each ancestor exactly once: procStat
	// returns its parent (to continue the walk), its name (to skip shells and our
	// own binary), and its start fingerprint (to record).
	candidate, _, _, err := procStat(os.Getpid())
	if err != nil {
		return Identity{}, false
	}
	for range maxAncestorDepth {
		if candidate <= 1 {
			return Identity{}, false
		}
		parent, name, start, err := procStat(candidate)
		if err != nil {
			return Identity{}, false
		}
		if !isTransient(name) {
			return Identity{PID: candidate, Start: start, Boot: boot, Host: host, Name: name}, true
		}
		candidate = parent
	}
	return Identity{}, false
}

// Check reports whether the process recorded in id is still alive.
//
// Precedence: an empty identity or a host mismatch is Unknown (cannot judge); a
// boot mismatch means a reboot, so the process is Dead; a missing PID or a
// start-fingerprint mismatch (PID reuse) is Dead; otherwise Alive. An
// unsupported platform is always Unknown so callers fall back to a timeout.
func Check(id Identity) Liveness {
	if id.PID <= 0 {
		return LivenessUnknown
	}
	if id.Host != "" {
		// Can't confirm we're on the recording host → can't trust its PIDs.
		host, err := os.Hostname()
		if err != nil || host != id.Host {
			return LivenessUnknown
		}
	}
	if id.Boot != "" {
		boot, err := bootID()
		switch {
		case err != nil || boot == "":
			return LivenessUnknown // can't confirm the boot → can't trust the PID
		case boot != id.Boot:
			return LivenessDead // rebooted: the process cannot have survived
		}
	}

	_, _, start, err := procStat(id.PID)
	switch {
	case errors.Is(err, errUnsupported):
		return LivenessUnknown
	case errors.Is(err, errProcessGone):
		return LivenessDead
	case err != nil:
		// Transient/unexpected error: don't claim the process is dead.
		return LivenessUnknown
	}
	if id.Start != "" && start != "" && id.Start != start {
		// Same PID, different start time: the PID was reused by another process.
		return LivenessDead
	}
	return LivenessAlive
}
