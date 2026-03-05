package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsWSL_NonWSL(t *testing.T) {
	// NOTE: no t.Parallel() because this file mutates package globals (paths + cache).

	// On macOS, IsWSL should always return false regardless of /proc/version
	if runtime.GOOS != "linux" {
		ResetWSLCache()
		defer ResetWSLCache()

		if IsWSL() {
			t.Error("IsWSL() should return false on non-Linux OS")
		}
		return
	}

	t.Setenv("WSL_INTEROP", "")
	t.Setenv("WSL_DISTRO_NAME", "")
	t.Setenv("WSLENV", "")

	// On Linux, test with mock /proc/version
	tmpDir := t.TempDir()
	mockProcVersion := filepath.Join(tmpDir, "version")
	if err := os.WriteFile(mockProcVersion, []byte("Linux version 6.5.0-generic (builder@host)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mockOSRelease := filepath.Join(tmpDir, "osrelease")
	if err := os.WriteFile(mockOSRelease, []byte("6.5.0-generic\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Override the proc version path and reset cache
	origPath := procVersionPath
	origOSRel := procOSReleasePath
	procVersionPath = mockProcVersion
	procOSReleasePath = mockOSRelease
	ResetWSLCache()
	defer func() {
		procVersionPath = origPath
		procOSReleasePath = origOSRel
		ResetWSLCache()
	}()

	if IsWSL() {
		t.Error("IsWSL() should return false when /proc/version does not contain 'microsoft'")
	}
}

func TestIsWSL_WSL2(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("WSL detection only works on Linux")
	}

	t.Setenv("WSL_INTEROP", "")
	t.Setenv("WSL_DISTRO_NAME", "")
	t.Setenv("WSLENV", "")

	tmpDir := t.TempDir()
	mockProcVersion := filepath.Join(tmpDir, "version")
	content := "Linux version 5.15.153.1-microsoft-standard-WSL2 " +
		"(root@65c757a075e2) (gcc (GCC) 11.2.0, GNU ld (GNU Binutils) 2.37) " +
		"#1 SMP Fri Mar 29 23:14:13 UTC 2024\n"
	if err := os.WriteFile(mockProcVersion, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	mockOSRelease := filepath.Join(tmpDir, "osrelease")
	if err := os.WriteFile(mockOSRelease, []byte("5.15.153.1-microsoft-standard-WSL2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origPath := procVersionPath
	origOSRel := procOSReleasePath
	procVersionPath = mockProcVersion
	procOSReleasePath = mockOSRelease
	ResetWSLCache()
	defer func() {
		procVersionPath = origPath
		procOSReleasePath = origOSRel
		ResetWSLCache()
	}()

	if !IsWSL() {
		t.Error("IsWSL() should return true when /proc/version contains 'microsoft'")
	}
}

func TestIsWSL_MissingProcVersion(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("WSL detection only works on Linux")
	}

	t.Setenv("WSL_INTEROP", "")
	t.Setenv("WSL_DISTRO_NAME", "")
	t.Setenv("WSLENV", "")

	origPath := procVersionPath
	origOSRel := procOSReleasePath
	procVersionPath = "/nonexistent/path/version"
	procOSReleasePath = "/nonexistent/path/osrelease"
	ResetWSLCache()
	defer func() {
		procVersionPath = origPath
		procOSReleasePath = origOSRel
		ResetWSLCache()
	}()

	if IsWSL() {
		t.Error("IsWSL() should return false when /proc/version is missing")
	}
}

func TestOSVariant_NonWSL(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" {
		// On non-Linux, OSVariant should return runtime.GOOS
		ResetWSLCache()
		defer ResetWSLCache()

		got := OSVariant()
		if got != runtime.GOOS {
			t.Errorf("OSVariant() = %q, want %q", got, runtime.GOOS)
		}
		return
	}

	// On Linux, mock a non-WSL /proc/version
	tmpDir := t.TempDir()
	mockProcVersion := filepath.Join(tmpDir, "version")
	if err := os.WriteFile(mockProcVersion, []byte("Linux version 6.5.0-generic\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origPath := procVersionPath
	procVersionPath = mockProcVersion
	ResetWSLCache()
	defer func() {
		procVersionPath = origPath
		ResetWSLCache()
	}()

	got := OSVariant()
	if got != "linux" {
		t.Errorf("OSVariant() = %q, want %q", got, "linux")
	}
}

func TestWindowsHomeDir_EnvOverride(t *testing.T) {
	t.Setenv("ENTIRE_WSL_WIN_HOME", "/mnt/c/Users/testuser")

	got, err := WindowsHomeDir()
	if err != nil {
		t.Fatalf("WindowsHomeDir() error = %v", err)
	}
	if got != "/mnt/c/Users/testuser" {
		t.Errorf("WindowsHomeDir() = %q, want %q", got, "/mnt/c/Users/testuser")
	}
}

func TestWindowsHomeDir_EmptyEnv(t *testing.T) {
	t.Setenv("ENTIRE_WSL_WIN_HOME", "")

	if runtime.GOOS != "linux" {
		// On non-Linux, cmd.exe and wslpath won't be available,
		// so this just verifies we don't panic
		_, _ = WindowsHomeDir()
		return
	}

	// On actual Linux (non-WSL), it's expected to fail gracefully
	_, err := WindowsHomeDir()
	// We just verify it doesn't panic; error is expected on non-WSL Linux
	_ = err
}
