package paths

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

// ErrUnroutableRuntimePath marks a policy failure while resolving runtime
// data. Callers must skip the Entire operation rather than fall back to a
// different layout and split one session across roots.
var ErrUnroutableRuntimePath = errors.New("repository runtime route cannot be verified")

// IsUnroutableRuntimePath reports whether err carries ErrUnroutableRuntimePath.
func IsUnroutableRuntimePath(err error) bool {
	return errors.Is(err, ErrUnroutableRuntimePath)
}

var runtimeDataPrefixes = []string{EntireMetadataDir, EntireLogsDir, EntireTmpDir}

func runtimeDataSubpath(relPath string) (string, bool) {
	rel := filepath.ToSlash(relPath)
	for _, prefix := range runtimeDataPrefixes {
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return strings.TrimPrefix(rel, EntireDir+"/"), true
		}
	}
	return "", false
}

var invisibleTestSeam struct {
	sync.RWMutex

	fail bool
}

// SetInvisibleProbeFailureForTesting forces runtime-root classification to
// fail. Real failures (git unavailable mid-hook, unreadable user settings)
// cannot be produced portably on disk, so tests inject the failure here.
func SetInvisibleProbeFailureForTesting(fail bool) {
	invisibleTestSeam.Lock()
	invisibleTestSeam.fail = fail
	invisibleTestSeam.Unlock()
}

// runtimeRootForPath resolves the base directory for .entire/{metadata,logs,tmp}
// from the repository policy: a hook-boundary snapshot on ctx when present,
// otherwise a fresh classification. Repo-level activation keeps the worktree
// layout; global activation routes under the git common dir.
func runtimeRootForPath(ctx context.Context, root string) (string, error) {
	policy, ok := repopolicy.RepoPolicyFromContext(ctx)
	if !ok {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("classifying repository policy: %w", err)
		}
		invisibleTestSeam.RLock()
		forcedFailure := invisibleTestSeam.fail
		invisibleTestSeam.RUnlock()
		if forcedFailure {
			return "", fmt.Errorf("%w: forced policy failure (test seam)", ErrUnroutableRuntimePath)
		}
		var err error
		policy, err = repopolicy.ClassifyRepoPolicy(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", fmt.Errorf("classifying repository policy: %w", ctxErr)
			}
			return "", fmt.Errorf("%w: classifying repository policy: %w", ErrUnroutableRuntimePath, err)
		}
	}
	if filepath.Clean(policy.WorktreeRoot) != filepath.Clean(root) {
		return "", fmt.Errorf("%w: policy worktree identity mismatch", ErrUnroutableRuntimePath)
	}
	return policy.RuntimeRoot(), nil
}
