package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v6"
)

// BackendTypeGitBranch is the built-in git-branch checkpoint backend: it stores
// the committed record on a git branch (entire/checkpoints/v1) in this repo. It
// is git-backed (see registeredBackend.gitBacked) and is the default primary
// when no backend is configured.
const BackendTypeGitBranch = "git-branch"

// BackendTypeGitRefs is the built-in git-refs checkpoint backend: it stores the
// committed record as one git ref per checkpoint (refs/entire/checkpoints/<shard>/
// <id>) in this repo. Like git-branch it is git-backed, so it may be the primary;
// the two can run side by side (git-refs primary + git-branch mirror) during the
// branch->refs rollout, since the one-of-each-type rule permits distinct
// git-backed backends in the same topology.
const BackendTypeGitRefs = "git-refs"

// OpenEnv carries the construction context a backend factory may need.
// Git-backed backends require Repo and use Refs/BlobFetcher/RefFetcher; other
// backends ignore the git-shaped fields and read their own configuration from cfg.
type OpenEnv struct {
	Repo        *git.Repository
	BlobFetcher BlobFetchFunc
	RefFetcher  RefFetchFunc
	Refs        PersistentRefs
}

// Factory constructs a persistent store for one backend type. cfg is the
// backend-specific JSON "config" block from settings (nil when absent).
type Factory func(ctx context.Context, env OpenEnv, cfg json.RawMessage) (PersistentStore, error)

// registeredBackend is a factory plus the capabilities the topology layer needs.
type registeredBackend struct {
	factory Factory
	// gitBacked reports whether the backend stores the committed record in this
	// repo's git object store. Only git-backed backends may be the primary,
	// because the checkpoint lifecycle (resume bootstrap, doctor reconcile,
	// explain tree-read, push, cleanup, pre-push OPF) drives the primary's record
	// through the repo and its refs — see buildPrimary. A backend that is not
	// git-backed has no such ref for those paths to operate on, so it is
	// mirror-only (write fan-out) until that lifecycle moves behind the store.
	// Whether a backend may be a *mirror* is governed separately by the
	// one-of-each-type topology rule in buildMirrors, not by this flag (a
	// git-backed backend can mirror alongside a different git-backed primary).
	gitBacked bool
}

var (
	registryMu sync.RWMutex
	// registry maps backend type to its factory and capabilities. Git-backed
	// backends are built in (registered here). Other backends add themselves
	// through Register — in practice only test-only backends do, via their
	// RegisterForTesting helpers, so a production binary can never select them.
	registry = map[string]registeredBackend{
		BackendTypeGitBranch: {factory: gitBranchBackendFactory, gitBacked: true},
		BackendTypeGitRefs:   {factory: gitRefsBackendFactory, gitBacked: true},
	}
)

// Register adds a non-git-backed backend factory under typ. Such backends can
// serve as mirrors but not as the primary (see registeredBackend.gitBacked).
// Git-backed backends are built in and not registered through this path. It
// panics on a duplicate type to surface wiring mistakes.
func Register(typ string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[typ]; exists {
		panic(fmt.Sprintf("checkpoint: backend type %q already registered", typ))
	}
	registry[typ] = registeredBackend{factory: f, gitBacked: false}
}

// lookupBackend returns the registered backend for typ, or a clear error
// (naming the registered types) when the type is unknown.
func lookupBackend(typ string) (registeredBackend, error) {
	registryMu.RLock()
	b, ok := registry[typ]
	registryMu.RUnlock()
	if !ok {
		return registeredBackend{}, fmt.Errorf("unknown checkpoint backend type %q (registered: %s)", typ, registeredTypes())
	}
	return b, nil
}

// build constructs the store for the named backend type.
func build(ctx context.Context, env OpenEnv, typ string, cfg json.RawMessage) (PersistentStore, error) {
	b, err := lookupBackend(typ)
	if err != nil {
		return nil, err
	}
	store, err := b.factory(ctx, env, cfg)
	if err != nil {
		return nil, fmt.Errorf("construct %q checkpoint backend: %w", typ, err)
	}
	return store, nil
}

func registeredTypes() string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	types := make([]string, 0, len(registry))
	for t := range registry {
		types = append(types, t)
	}
	sort.Strings(types)
	return strings.Join(types, ", ")
}

// gitBranchBackendFactory builds the git-branch persistent store from the open
// environment. It ignores cfg: git topology comes from env.Refs, not settings.
func gitBranchBackendFactory(_ context.Context, env OpenEnv, _ json.RawMessage) (PersistentStore, error) {
	if env.Repo == nil {
		return nil, errors.New("git-branch checkpoint backend requires a repository")
	}
	store := NewGitStore(env.Repo, env.Refs)
	if env.BlobFetcher != nil {
		store.SetBlobFetcher(env.BlobFetcher)
	}
	return store, nil
}

// gitRefsBackendFactory builds the git-refs persistent store from the open
// environment. It ignores cfg and env.Refs: each checkpoint resolves its own ref
// (refs/entire/checkpoints/<shard>/<id>) rather than a single configured ref.
func gitRefsBackendFactory(_ context.Context, env OpenEnv, _ json.RawMessage) (PersistentStore, error) {
	if env.Repo == nil {
		return nil, errors.New("git-refs checkpoint backend requires a repository")
	}
	store := newGitRefsStore(env.Repo)
	if env.BlobFetcher != nil {
		store.SetBlobFetcher(env.BlobFetcher)
	}
	if env.RefFetcher != nil {
		store.SetRefFetcher(env.RefFetcher)
	}
	return store, nil
}
