//go:build !windows

package interactive

import "os"

// releasePendingReads is a no-op off Windows: Bubble Tea's poll-based readers
// cancel any descriptor there, so nothing is left pending when Close runs.
func releasePendingReads(*os.File) error { return nil }
