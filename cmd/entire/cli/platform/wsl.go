// Package platform provides platform detection utilities for the Entire CLI.
// It detects WSL (Windows Subsystem for Linux) environments and provides
// helpers for resolving Windows filesystem paths from within WSL.
package platform

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var procVersionPath = "/proc/version"
var procOSReleasePath = "/proc/sys/kernel/osrelease"

var (
	isWSLOnce sync.Once
	isWSL     bool
)

// IsWSL returns true if the current process is running inside Windows Subsystem for Linux.
// The result is cached after the first call.
func IsWSL() bool {
	isWSLOnce.Do(func() {
		isWSL = detectWSL()
	})

	return isWSL
}

// WSL runs as linux, so detect it via env vars + /proc markers.
func detectWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}

	// Fast signals
	if os.Getenv("WSL_INTEROP") != "" || os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSLENV") != "" {
		return true
	}

	// Fallback heuristics
	if b, err := os.ReadFile(procVersionPath); err == nil {
		if strings.Contains(strings.ToLower(string(b)), "microsoft") {
			return true
		}
	}

	if b, err := os.ReadFile(procOSReleasePath); err == nil {
		if bytes.Contains(bytes.ToLower(b), []byte("microsoft")) {
			return true
		}
	}

	return false
}

// ResetWSLCache resets the cached WSL detection state.
// This is only intended for testing - do not use in production code.
func ResetWSLCache() {
	isWSLOnce = sync.Once{}
	isWSL = false
}

func OSVariant() string {
	if IsWSL() {
		return "wsl"
	}

	return runtime.GOOS
}

// WslpathWindows converts a WSL/Linux path to a Windows path using `wslpath -w`.
func WslpathWindows(linuxPath string) (string, error) {
	if linuxPath == "" {
		return "", os.ErrNotExist
	}
	
	cmd := exec.Command("wslpath", "-w", linuxPath)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", os.ErrNotExist
	}

	return s, nil
}

// WslpathLinux converts a Windows path to a WSL/Linux path using `wslpath -u`.
func WslpathLinux(winPath string) (string, error) {
	if winPath == "" {
		return "", os.ErrNotExist
	}
	
	cmd := exec.Command("wslpath", "-u", winPath)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", os.ErrNotExist
	}
	
	return s, nil
}

// WindowsPathCandidatesFromWSLPath returns likely Windows-side path strings that a
// Windows-native app might use to represent the same repo.
//
// Examples:
//   - /mnt/c/Users/Alice/src/repo -> C:\Users\Alice\src\repo
//   - /home/alice/repo -> \\wsl.localhost\Ubuntu\home\alice\repo (and \\wsl$\Ubuntu\home\alice\repo)
func WindowsPathCandidatesFromWSLPath(linuxPath string) []string {
	if !IsWSL() {
		return nil
	}

	// Optional override for debugging / edge-cases.
	if override := os.Getenv("ENTIRE_WSL_REPO_WIN_PATH"); override != "" {
		return []string{override}
	}

	win, err := WslpathWindows(linuxPath)
	if err != nil || win == "" {
		return nil
	}

	cands := []string{win}

	lower := strings.ToLower(win)
	if strings.HasPrefix(lower, `\\wsl.localhost\`) {
		// swap prefix to \\wsl$\
		cands = append(cands, `\\wsl$`+win[len(`\\wsl.localhost`):])
	}
	if strings.HasPrefix(lower, `\\wsl$\`) {
		// swap prefix to \\wsl.localhost\
		cands = append(cands, `\\wsl.localhost`+win[len(`\\wsl$`):])
	}

	return uniqueStrings(cands)
}

// HomeDirCandidates returns a list of "home" directories to try, in priority order.
// On WSL we prefer Windows home first (because Windows-native agents store there),
// then fall back to Linux home.
func HomeDirCandidates() []string {
	var out []string

	// On WSL, try Windows home first.
	if IsWSL() {
		if winHome, err := WindowsHomeDir(); err == nil && winHome != "" {
			out = append(out, winHome)
		}
	}

	if linuxHome, err := os.UserHomeDir(); err == nil && linuxHome != "" {
		out = append(out, linuxHome)
	}

	return uniqueStrings(out)
}

// WindowsHomeDir returns the Windows user home directory accessible from WSL.
// This is needed because Windows-native agents (Claude Code, Cursor) store
// their session data under the Windows user directory, not the WSL Linux home.
//
// Resolution order:
//  1. ENTIRE_WSL_WIN_HOME environment variable (for testing/overrides)
//  2. cmd.exe /C "echo %USERPROFILE%" via wslpath
//  3. /mnt/c/Users/<username> fallback
//
// Returns an error if the Windows home cannot be determined.
func WindowsHomeDir() (string, error) {
	if !IsWSL() {
		return "", fmt.Errorf("WindowsHomeDir called outside WSL")
	}

	// Allow override for testing and user customization
	if override := os.Getenv("ENTIRE_WSL_WIN_HOME"); override != "" {
		// Allow providing either Windows path or already-converted WSL path.
		if strings.Contains(override, `:\`) || strings.HasPrefix(override, `\\`) {
			if p, err := WslpathLinux(override); err == nil {
				return p, nil
			}
		}
		return override, nil
	}

	// Try resolving via cmd.exe (most reliable)
	if home, err := windowsHomeDirViaCmdExe(); err == nil {
		return home, nil
	}

	// Fallback: try /mnt/c/Users/<username>
	return windowsHomeDirFallback()
}

// windowsHomeDirViaCmdExe resolves the Windows home directory by invoking cmd.exe.
func windowsHomeDirViaCmdExe() (string, error) {
	// Get Windows USERPROFILE path
	cmd := exec.Command("cmd.exe", "/C", "echo", "%USERPROFILE%")
	cmd.Stderr = nil
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	winPath := strings.TrimSpace(string(output))
	if winPath == "" || winPath == "%USERPROFILE%" {
		return "", os.ErrNotExist
	}

	result, err := WslpathLinux(winPath)
	if err != nil {
		return "", err
	}

	if result == "" {
		return "", os.ErrNotExist
	}

	if info, err := os.Stat(result); err == nil && info.IsDir() {
		return result, nil
	}

	return result, nil
}

// windowsHomeDirFallback tries to resolve the Windows home via /mnt/c/Users/.
func windowsHomeDirFallback() (string, error) {
	// Try the current user's name
	username := os.Getenv("USER")
	if username == "" {
		username = os.Getenv("LOGNAME")
	}
	if username == "" {
		return "", os.ErrNotExist
	}

	candidate := filepath.Join("/mnt/c/Users", username)
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate, nil
	}

	return "", os.ErrNotExist
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	return out
}
