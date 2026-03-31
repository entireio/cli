//go:build windows

package agents

import "os"

// linkFile copies src to dst because symlinks on Windows require elevated
// privileges or Developer Mode.
func linkFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
