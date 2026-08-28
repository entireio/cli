//go:build unix

package paths

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A FIFO at `.entire/settings.json` used to be left alone on the grounds that
// it hangs the settings read rather than redirecting it. A hang with no error
// and no way to interrupt the hook is not a better outcome than a refusal, and
// Entire never creates one, so it is refused with the same remedy as any other
// entry it did not put there.
func TestValidateEntireDirAt_NamedPipeEntryIsRejected(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	entire := makeEntireDir(t, root)
	entry := filepath.Join(entire, "settings.json")
	if err := syscall.Mkfifo(entry, 0o600); err != nil {
		t.Skipf("cannot create a named pipe: %v", err)
	}

	err := ValidateEntireDirAt(root)
	if !errors.Is(err, ErrEntireDirUnsupportedEntry) {
		t.Fatalf("want ErrEntireDirUnsupportedEntry, got %v", err)
	}
	if errors.Is(err, ErrEntireDirNotDirectory) {
		t.Errorf("an unsupported entry must not read as `.entire` being the wrong type: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, entry) {
		t.Errorf("message %q does not name the entry %q", msg, entry)
	}
	if !strings.Contains(msg, "named pipe") {
		t.Errorf("message %q does not say what was found", msg)
	}
}

func TestValidateEntireDirAt_SocketEntryIsRejected(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	entire := makeEntireDir(t, root)
	entry := filepath.Join(entire, "tmp")
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "unix", entry)
	if err != nil {
		t.Skipf("cannot create a unix socket: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Logf("close listener: %v", err)
		}
	})

	if err := ValidateEntireDirAt(root); !errors.Is(err, ErrEntireDirUnsupportedEntry) {
		t.Fatalf("want ErrEntireDirUnsupportedEntry, got %v", err)
	}
}

// The allowlist must not start refusing the layout Entire itself writes.
func TestValidateEntireDirAt_EntireOwnLayoutIsAccepted(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	entire := makeEntireDir(t, root)
	for _, sub := range []string{"logs", "metadata", "tmp"} {
		if err := os.Mkdir(filepath.Join(entire, sub), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"settings.json", "settings.local.json"} {
		if err := os.WriteFile(filepath.Join(entire, file), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := ValidateEntireDirAt(root); err != nil {
		t.Fatalf("ValidateEntireDirAt rejected Entire's own layout: %v", err)
	}
}
