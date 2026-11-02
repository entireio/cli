package tokenstore

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"sync"
	"testing"
)

// ErrKeyringUnavailable is returned instead of ErrNotFound when the OS keyring
// reports a credential missing but could not be searched at all, so a saved
// login is not misreported as "not logged in". The text is the whole
// explanation because it is what the user (or their agent) reads: logging in
// again cannot help, since the new token could not be stored either.
var ErrKeyringUnavailable = errors.New("the macOS Keychain is not accessible from this process, so the saved login cannot be read " +
	"(a sandbox is the usual cause); re-run the command where it can reach the Keychain — `entire login` will not fix this")

// securityPath is the macOS Keychain CLI that go-keyring itself shells out to.
const securityPath = "/usr/bin/security"

// keyringReachable reports whether the OS keyring can be searched. Probed once
// per process: reachability does not change mid-run, and the probe only runs
// after a "not found", which normal flows hit repeatedly (absent refresh
// slots, cache misses).
var keyringReachable = sync.OnceValue(probeKeyringReachable)

// probeKeyringReachable answers the question go-keyring cannot. On macOS it
// reads through `security find-generic-password` and maps any output
// containing "could not be found" to ErrNotFound, but `security` prints that
// same text when it cannot reach the Keychain at all (e.g. a Seatbelt sandbox
// that denies the security daemon). `security list-keychains` fails outright
// in that state and succeeds otherwise, so it tells the two apart.
//
// Only a clean non-zero exit counts as unreachable. A probe that cannot start
// or does not finish proves nothing, so it leaves ErrNotFound as reported.
// Other platforms' keyrings return a distinct error when unreachable, so they
// need no probe.
//
// Under `go test` it never runs: keyring.MockInit fakes go-keyring but not this
// subprocess, so a test reaching keyringStore would otherwise query the real
// Keychain. testing.Testing() is the same signal testdirs uses to keep tests
// off the real credential store.
func probeKeyringReachable() bool {
	if runtime.GOOS != "darwin" || testing.Testing() {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), keyringTimeout())
	defer cancel()
	err := exec.CommandContext(ctx, securityPath, "list-keychains").Run()
	return ctx.Err() != nil || !exitedNonZero(err)
}

// exitedNonZero reports whether err is a process that exited on its own with a
// non-zero status. A child killed by a signal is also an *exec.ExitError, but
// its ExitCode is -1, and being killed says nothing about the keyring.
func exitedNonZero(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() > 0
}

// checkNotFound turns a keyring "not found" into ErrKeyringUnavailable when
// the keyring could not be searched; every other result passes through.
func checkNotFound(val string, err error, reachable func() bool) (string, error) {
	if errors.Is(err, ErrNotFound) && !reachable() {
		return "", ErrKeyringUnavailable
	}
	return val, err
}
