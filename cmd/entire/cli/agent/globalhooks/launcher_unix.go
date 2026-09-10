//go:build !windows

package globalhooks

func platformLauncher() (string, error) { return "/bin/sh", nil }
