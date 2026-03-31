//go:build !windows

package agents

import "os"

// linkFile creates a symbolic link from src to dst.
func linkFile(src, dst string) error {
	return os.Symlink(src, dst)
}
