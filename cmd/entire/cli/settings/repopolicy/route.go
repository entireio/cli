package repopolicy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

const (
	routeFileName = "route.json"
	routeLockWait = 2 * time.Second
)

func routePath(repository Repository) string {
	return filepath.Join(registryDir(repository), routeFileName)
}

// ReadRuntimeRoute reads and fully verifies an established route. found is
// false only when the route does not exist; corrupt and identity-mismatched
// records return an error.
func ReadRuntimeRoute(repository Repository) (route RuntimeRoute, found bool, err error) {
	if err := validateRepository(repository); err != nil {
		return RuntimeRoute{}, false, err
	}
	data, err := os.ReadFile(routePath(repository))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return RuntimeRoute{}, false, nil
		}
		return RuntimeRoute{}, false, fmt.Errorf("reading runtime route: %w", err)
	}
	if err := decodeStrict(data, &route); err != nil {
		return RuntimeRoute{}, false, fmt.Errorf("parsing runtime route: %w", err)
	}
	if err := validateRuntimeRoute(repository, route); err != nil {
		return RuntimeRoute{}, false, err
	}
	return route, true, nil
}

func validateRuntimeRoute(repository Repository, route RuntimeRoute) error {
	if route.Version != recordVersion {
		return fmt.Errorf("unsupported runtime route version %d", route.Version)
	}
	if route.Layout != RuntimeWorktree && route.Layout != RuntimeGitCommon {
		return fmt.Errorf("invalid runtime route layout %q", route.Layout)
	}
	if route.CanonicalWorktree != repository.WorktreeRoot || route.CanonicalGitCommon != repository.GitCommonDir {
		return errors.New("runtime route repository identity mismatch")
	}
	return nil
}

// EnsureRuntimeRoute establishes the policy's proposed route without replacing
// an existing winner, then rereads and returns that complete winner.
func EnsureRuntimeRoute(ctx context.Context, policy RepoPolicy) (RepoPolicy, error) {
	if err := ctx.Err(); err != nil {
		return policy, fmt.Errorf("ensuring runtime route: %w", err)
	}
	repository := Repository{
		WorktreeRoot: policy.WorktreeRoot,
		GitCommonDir: policy.GitCommonDir,
		WorktreeKey:  policy.WorktreeKey,
	}
	if err := validateRepository(repository); err != nil {
		return policy, err
	}
	if err := validateRuntimeRoute(repository, policy.Route); err != nil {
		return policy, fmt.Errorf("invalid proposed runtime route: %w", err)
	}
	if winner, found, err := ReadRuntimeRoute(repository); err != nil {
		return policy, err
	} else if found {
		policy.Route = winner
		return policy, nil
	}
	if err := ensureRegistryDir(repository); err != nil {
		return policy, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, routeLockWait)
	defer cancel()
	release, err := flock.AcquireContext(lockCtx, routePath(repository)+".lock")
	if err != nil {
		return policy, fmt.Errorf("locking runtime route: %w", err)
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return policy, fmt.Errorf("locking runtime route: %w", err)
	}
	if winner, found, err := ReadRuntimeRoute(repository); err != nil {
		return policy, err
	} else if found {
		policy.Route = winner
		return policy, nil
	}
	if err := publishRouteNoReplace(routePath(repository), policy.Route); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return policy, fmt.Errorf("publishing runtime route: %w", err)
		}
	}
	winner, found, err := ReadRuntimeRoute(repository)
	if err != nil {
		return policy, err
	}
	if !found {
		return policy, errors.New("runtime route winner was not published")
	}
	policy.Route = winner
	return policy, nil
}

func publishRouteNoReplace(path string, route RuntimeRoute) error {
	data, err := jsonutil.MarshalIndentWithNewline(route, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding route: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating route temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing route temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("securing route temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing route temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing route temp: %w", err)
	}
	if err := os.Link(tmpName, path); err != nil {
		return fmt.Errorf("linking route record: %w", err)
	}
	if err := jsonutil.SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("syncing route registry: %w", err)
	}
	return nil
}

// RuntimeRoot returns the base directory for runtime metadata, logs, and tmp
// according to a verified policy route.
func RuntimeRoot(policy RepoPolicy) (string, error) {
	repository := Repository{
		WorktreeRoot: policy.WorktreeRoot,
		GitCommonDir: policy.GitCommonDir,
		WorktreeKey:  policy.WorktreeKey,
	}
	if err := validateRuntimeRoute(repository, policy.Route); err != nil {
		return "", err
	}
	if policy.Route.Layout == RuntimeGitCommon {
		return registryDir(repository), nil
	}
	return filepath.Join(repository.WorktreeRoot, ".entire"), nil // entire-join-ok: this is the verified worktree route selected by route.json
}
