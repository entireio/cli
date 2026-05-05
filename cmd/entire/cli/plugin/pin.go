package plugin

import (
	"os"
	"strings"
)

const pinPrefix = ".pin-"

// readPinSHA returns the pinned SHA from a .pin-<sha> marker in dir, or "" if
// none exists. The first matching file wins; multiple pins are not expected.
func readPinSHA(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, pinPrefix) {
			return strings.TrimPrefix(name, pinPrefix)
		}
	}
	return ""
}
