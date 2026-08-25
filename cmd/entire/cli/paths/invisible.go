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

// ErrUnroutableRuntimePath marks a policy or route failure for runtime data.
// Callers must skip the Entire operation rather than fall back to a different
// layout and split one session across roots.
var ErrUnroutableRuntimePath = errors.New("repository runtime route cannot be verified")

// IsUnroutableRuntimePath reports whether err carries ErrUnroutableRuntimePath.
func IsUnroutableRuntimePath(err error) bool {
	return errors.Is(err, ErrUnroutableRuntimePath)
}

// InvisibleRuntimeDir returns the git-common runtime registry for root.
func InvisibleRuntimeDir(commonDir, root string) (string, error) {
	worktreeID, err := GetWorktreeID(root)
	if err != nil {
		return "", err
	}
	return repopolicy.WorktreeRegistryDir(commonDir, HashWorktreeID(worktreeID)), nil
}

var runtimeDataPrefixes = []string{EntireMetadataDir, EntireLogsDir, EntireTmpDir}

// InvisibleRuntimeSubdirs returns the runtime subtrees affected by routing.
func InvisibleRuntimeSubdirs() []string {
	subs := make([]string, len(runtimeDataPrefixes))
	for i, prefix := range runtimeDataPrefixes {
		subs[i] = strings.TrimPrefix(prefix, EntireDir+"/")
	}
	return subs
}

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

// SetInvisibleProbeFailureForTesting forces compatibility classification to
// fail. It remains only as the established path-layer test seam.
func SetInvisibleProbeFailureForTesting(fail bool) {
	invisibleTestSeam.Lock()
	invisibleTestSeam.fail = fail
	invisibleTestSeam.Unlock()
}

// ClearInvisibleRuntimeCache is retained as a compatibility no-op. Runtime
// policy is no longer cached or reconstructed by paths.
func ClearInvisibleRuntimeCache() {}

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
	base, err := repopolicy.RuntimeRoot(policy)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnroutableRuntimePath, err)
	}
	return base, nil
}
