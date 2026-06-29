package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// OpenOptions configures Open. The zero value uses the default committed-ref
// topology and attaches no blob fetcher.
type OpenOptions struct {
	// BlobFetcher is the CLI-level on-demand blob fetcher. The checkpoint
	// package cannot resolve it itself, so the CLI layer injects it here and
	// Open attaches it to the constructed store(s). nil leaves on-demand
	// fetching off.
	BlobFetcher BlobFetchFunc

	// Refs overrides the default committed-ref topology. A non-nil value wins,
	// e.g. attach pins reads to Primary via PrimaryAsRead().
	Refs *PersistentRefs
}

// Stores is the facade returned by Open: the persistent store plus the git-only
// ephemeral (shadow-branch) capability and resolved committed-ref topology.
type Stores struct {
	// Persistent is the committed store that serves permanent reads and writes.
	Persistent PersistentStore

	ephemeral EphemeralStore
	refs      PersistentRefs
}

// Open resolves the checkpoint storage topology and constructs the backing
// store(s). It keeps ref resolution, backend selection, and blob-fetcher wiring
// in one place. The primary is built through the backend registry; with no
// checkpoints config it resolves to the git-branch backend with no mirrors, so
// default behavior is unchanged. When mirrors are configured, the persistent
// store is a fan-out wrapper (reads from primary, best-effort writes to mirrors).
//
// Backend selection is read via settings.LoadCheckpointsConfig, which resolves
// like settings.Load: from the context's worktree root if set, else relative to
// the current working directory — not from repo. Callers opening a repository
// that is not the cwd should wrap ctx with that worktree root (as dispatch does).
// Resolution is fail-soft: a missing or unreadable settings file yields the
// default git-branch backend with no mirrors, preserving default behavior.
func Open(ctx context.Context, repo *git.Repository, opts OpenOptions) (*Stores, error) {
	refs := resolveOpenRefs(ctx, opts)
	env := OpenEnv{Repo: repo, BlobFetcher: opts.BlobFetcher, Refs: refs}

	cfg, err := settings.LoadCheckpointsConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve checkpoints config: %w", err)
	}

	primaryType := resolvePrimaryType(cfg)
	primary, err := buildPrimary(ctx, env, primaryType, primaryConfig(cfg))
	if err != nil {
		return nil, err
	}
	mirrors, err := buildMirrors(ctx, env, cfg, primaryType)
	if err != nil {
		return nil, err
	}

	return &Stores{
		Persistent: newFanoutStore(primary, mirrors),
		ephemeral:  newEphemeralStore(repo, refs),
		refs:       refs,
	}, nil
}

// resolvePrimaryType returns the configured primary backend type, defaulting to
// the git-branch backend when none is configured.
func resolvePrimaryType(cfg *settings.CheckpointsConfig) string {
	if cfg != nil && cfg.Primary.Type != "" {
		return cfg.Primary.Type
	}
	return BackendTypeGitBranch
}

// primaryConfig returns the primary backend's config block, if any.
func primaryConfig(cfg *settings.CheckpointsConfig) json.RawMessage {
	if cfg == nil {
		return nil
	}
	return cfg.Primary.Config
}

// buildPrimary constructs the primary persistent store. The primary must be a
// git-backed backend: attach, resume, push, doctor, cleanup, and OPF all drive
// the primary's record through the repo and its refs, so a non-git-backed
// primary is rejected rather than silently half-supported.
func buildPrimary(ctx context.Context, env OpenEnv, typ string, raw json.RawMessage) (PersistentStore, error) {
	b, err := lookupBackend(typ)
	if err != nil {
		return nil, fmt.Errorf("checkpoints.primary: %w", err)
	}
	if !b.gitBacked {
		return nil, fmt.Errorf("checkpoints.primary.type %q cannot be the primary: only git-backed backends (e.g. %q) may be the primary", typ, BackendTypeGitBranch)
	}
	return build(ctx, env, typ, raw)
}

// buildMirrors constructs the mirror writers. Each backend type may appear at
// most once across the topology (primary + mirrors), so a mirror cannot reuse
// the primary's type or another mirror's. This is the conservative form of "no
// two backends may write the same target": today two backends of the same type
// share the same refs/storage, so a duplicate type is a guaranteed collision
// (e.g. a git-branch mirror under a git-branch primary would double-write the v1
// branch). A future per-mirror config (same backend type pointed at a distinct
// repo/refs) could relax this; for now it is one of each type.
func buildMirrors(ctx context.Context, env OpenEnv, cfg *settings.CheckpointsConfig, primaryType string) ([]Writer, error) {
	if cfg == nil || len(cfg.Mirrors) == 0 {
		return nil, nil
	}
	seen := map[string]bool{primaryType: true}
	mirrors := make([]Writer, 0, len(cfg.Mirrors))
	for i, m := range cfg.Mirrors {
		if _, err := lookupBackend(m.Type); err != nil {
			return nil, fmt.Errorf("checkpoints.mirrors[%d]: %w", i, err)
		}
		if seen[m.Type] {
			return nil, fmt.Errorf("checkpoints.mirrors[%d]: backend type %q is already used by the primary or another mirror; each backend type may appear at most once", i, m.Type)
		}
		seen[m.Type] = true
		store, err := build(ctx, env, m.Type, m.Config)
		if err != nil {
			return nil, fmt.Errorf("checkpoints.mirrors[%d]: %w", i, err)
		}
		mirrors = append(mirrors, store)
	}
	return mirrors, nil
}

func resolveOpenRefs(ctx context.Context, opts OpenOptions) PersistentRefs {
	if opts.Refs != nil {
		return *opts.Refs
	}
	return ResolveRefs(ctx)
}

// Ephemeral returns the git-backed shadow-branch (temporary) store.
func (s *Stores) Ephemeral() EphemeralStore { return s.ephemeral }

// Refs returns the resolved committed-ref topology.
func (s *Stores) Refs() PersistentRefs { return s.refs }
