package cliauth

import (
	"fmt"
	"os"
)

// Debugf is the [entire-repo] / [entiredb] counterpart to git-remote-entire's
// debugf, gated by ENTIRE_DEBUG so a single env var lights up debug output
// across the CLIs.
func Debugf(format string, args ...any) {
	if os.Getenv("ENTIRE_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[entire-cli] "+format+"\n", args...)
	}
}
