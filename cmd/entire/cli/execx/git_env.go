package execx

import (
	"os"
	"strings"
)

// EnvWithoutRepoOverrides preserves the process environment except for Git's
// repository selectors. Use it when a subprocess has an explicit target;
// current-repository operations should keep their inherited environment.
func EnvWithoutRepoOverrides() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE":
			continue
		}
		filtered = append(filtered, kv)
	}
	return filtered
}
