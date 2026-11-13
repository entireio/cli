//go:build windows

package strategy

import "os"

// canExecuteHook: Windows has no execute bit, so any regular file can run.
func canExecuteHook(*os.Root, string) bool { return true }
