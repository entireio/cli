package globalhooks

import (
	"fmt"
	"golang.org/x/sys/windows"
	"path/filepath"
)

func platformLauncher() (string, error) {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return "", fmt.Errorf("resolve Windows hook launcher: %w", err)
	}
	return filepath.Join(dir, "WindowsPowerShell", "v1.0", "powershell.exe"), nil
}
