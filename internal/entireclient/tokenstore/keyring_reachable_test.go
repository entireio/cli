package tokenstore

import (
	"errors"
	"os/exec"
	"runtime"
	"testing"
)

func TestCheckNotFound(t *testing.T) {
	t.Parallel()

	other := errors.New("backend exploded")
	tests := []struct {
		name      string
		val       string
		err       error
		reachable bool
		wantVal   string
		wantErr   error
		wantProbe bool
	}{
		{name: "found", val: "tok", reachable: false, wantVal: "tok"},
		{name: "not found, keyring reachable", err: ErrNotFound, reachable: true, wantErr: ErrNotFound, wantProbe: true},
		{name: "not found, keyring unreachable", err: ErrNotFound, reachable: false, wantErr: ErrKeyringUnavailable, wantProbe: true},
		{name: "other error passes through", err: other, reachable: false, wantErr: other},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			probed := false
			got, err := checkNotFound(tt.val, tt.err, func() bool {
				probed = true
				return tt.reachable
			})
			if got != tt.wantVal {
				t.Errorf("val = %q, want %q", got, tt.wantVal)
			}
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			// An unreachable keyring is not a "not found": callers that
			// tolerate a missing slot must see it as a store failure.
			if errors.Is(err, ErrKeyringUnavailable) && errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v must not also match ErrNotFound", err)
			}
			if probed != tt.wantProbe {
				t.Errorf("probed = %v, want %v", probed, tt.wantProbe)
			}
		})
	}
}

// Only a probe that exited on its own with a failure status proves the keyring
// unreachable; one killed by a signal (or that never started) is inconclusive.
func TestExitedNonZero(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == goosWindows {
		t.Skip("uses a POSIX shell; the probe only runs on macOS")
	}

	tests := []struct {
		name   string
		script string
		want   bool
	}{
		{name: "non-zero exit", script: "exit 1", want: true},
		{name: "killed by signal", script: "kill -KILL $$", want: false},
		{name: "success", script: "exit 0", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := exec.CommandContext(t.Context(), "/bin/sh", "-c", tt.script).Run()
			if got := exitedNonZero(err); got != tt.want {
				t.Errorf("exitedNonZero(%v) = %v, want %v", err, got, tt.want)
			}
		})
	}

	t.Run("did not start", func(t *testing.T) {
		t.Parallel()

		if exitedNonZero(exec.ErrNotFound) {
			t.Error("a probe that never started must not count as unreachable")
		}
	})
}

// The probe must never shell out to the real Keychain from a test.
func TestProbeKeyringReachable_NoProbeUnderTest(t *testing.T) {
	t.Parallel()

	if !probeKeyringReachable() {
		t.Fatal("probeKeyringReachable under go test = false; it must not probe the real keyring")
	}
}
