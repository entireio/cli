//go:build !windows

package cli

import "os"

// symlinkManagedEntry links destName to src. Symlink-first keeps the dev-loop
// property that rebuilding the source is reflected in the managed entry.
// See plugin_store_windows.go for why Windows never does this.
func symlinkManagedEntry(root *os.Root, src, destName string) bool {
	return root.Symlink(src, destName) == nil
}
