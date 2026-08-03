package procsignal

import (
	"os"
	"syscall"
	"testing"
)

// These tests mutate the package-global caught signal, so they must not run in
// parallel with each other.

func TestStoreLoad(t *testing.T) {
	Reset()
	if got := Load(); got != nil {
		t.Fatalf("Load() after Reset = %v, want nil", got)
	}

	Store(os.Interrupt)
	if got := Load(); got != os.Interrupt {
		t.Fatalf("Load() = %v, want %v", got, os.Interrupt)
	}

	// Storing a different concrete-typed signal must not panic and must win.
	Store(syscall.SIGTERM)
	if got := Load(); got != syscall.SIGTERM {
		t.Fatalf("Load() = %v, want SIGTERM", got)
	}
}

func TestResetClears(t *testing.T) {
	Store(os.Interrupt)
	Reset()
	if got := Load(); got != nil {
		t.Fatalf("Load() after Reset = %v, want nil", got)
	}
}
