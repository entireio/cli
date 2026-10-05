//go:build darwin

package senclave

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// requireEnclaveEnv makes a missing enclave a failure instead of a skip,
// for developer Macs where a Generate error means a binding regression.
// CI macOS runners are VMs without an enclave, so the default is to skip.
const requireEnclaveEnv = "ENTIRE_TEST_REQUIRE_SECURE_ENCLAVE"

// Generate, Load and Seal never prompt, so they can run unattended. Unseal
// shows the Touch ID dialog and is covered manually (see
// docs/architecture/token-protection.md).
func TestGenerateLoadSeal(t *testing.T) {
	t.Parallel()
	blob, err := Generate()
	if err != nil {
		if os.Getenv(requireEnclaveEnv) != "" {
			t.Fatalf("Generate: %v", err)
		}
		t.Skipf("no Secure Enclave available (set %s=1 to fail instead): %v", requireEnclaveEnv, err)
	}
	if len(blob) == 0 {
		t.Fatal("empty key blob")
	}
	k1, err := Load(blob)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer k1.Close()
	k2, err := Load(blob)
	if err != nil {
		t.Fatalf("Load again: %v", err)
	}
	defer k2.Close()

	// The blob names one key: both loads expose the same public point.
	pub1, err := k1.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	pub2, err := k2.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if !bytes.Equal(pub1, pub2) {
		t.Fatalf("loaded keys differ: %x vs %x", pub1[:8], pub2[:8])
	}
	if pub1[0] != 0x04 || len(pub1) != 65 {
		t.Fatalf("unexpected public key encoding: len=%d first=%#x", len(pub1), pub1[0])
	}

	ct, err := k1.Seal([]byte("hello"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(ct, []byte("hello")) {
		t.Fatal("ciphertext contains plaintext")
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := Load(nil); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Load(nil) err = %v, want ErrNoKey", err)
	}
	if _, err := load(); err != nil {
		t.Skipf("frameworks unavailable: %v", err)
	}
	if _, err := Load([]byte("not a key blob")); err == nil {
		t.Fatal("Load(garbage) succeeded")
	}
}
