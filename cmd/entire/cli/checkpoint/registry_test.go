package checkpoint

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistry_GitBranchBackendRegistered(t *testing.T) {
	t.Parallel()

	_, err := build(context.Background(), OpenEnv{}, BackendTypeGitBranch, nil)
	// The git-branch factory rejects a nil repo, which proves it is registered
	// and reached (an unknown type would fail earlier with a different message).
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git-branch checkpoint backend requires a repository")
}

func TestRegistry_GitBranchIsGitBacked(t *testing.T) {
	t.Parallel()

	b, err := lookupBackend(BackendTypeGitBranch)
	require.NoError(t, err)
	assert.True(t, b.gitBacked, "git-branch backend must be git-backed so it can serve as the primary")
}

func TestRegistry_UnknownType(t *testing.T) {
	t.Parallel()

	_, err := build(context.Background(), OpenEnv{}, "definitely-not-a-backend", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown checkpoint backend type "definitely-not-a-backend"`)
	// The error lists registered types so misconfiguration is debuggable.
	assert.Contains(t, err.Error(), BackendTypeGitBranch)
}

func TestValidatePrimaryBackend_GitBackedTypesAllowed(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidatePrimaryBackend(BackendTypeGitBranch))
	require.NoError(t, ValidatePrimaryBackend(BackendTypeGitRefs))
}

func TestValidatePrimaryBackend_UnknownTypeRejected(t *testing.T) {
	t.Parallel()

	err := ValidatePrimaryBackend("definitely-not-a-backend")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown checkpoint backend type "definitely-not-a-backend"`)
	// The error lists registered types so a typo is debuggable.
	assert.Contains(t, err.Error(), BackendTypeGitBranch)
}

func TestValidatePrimaryBackend_NonGitBackedRejected(t *testing.T) {
	t.Parallel()

	// Register a mirror-only (non-git-backed) backend and confirm it cannot be the
	// primary. The unique type name avoids colliding with the built-ins.
	const typ = "test-mirror-only-primary-check"
	Register(typ, func(context.Context, OpenEnv, json.RawMessage) (PersistentStore, error) {
		return nil, nil //nolint:nilnil // never constructed; validation fails before build
	})

	err := ValidatePrimaryBackend(typ)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be the primary")
	assert.Contains(t, err.Error(), BackendTypeGitRefs)
}

func TestRegistry_GitBranchFactoryIgnoresConfig(t *testing.T) {
	t.Parallel()

	// A non-nil cfg block must not change the nil-repo rejection: the git-branch
	// backend takes its topology from env.Refs, not from settings cfg.
	_, err := build(context.Background(), OpenEnv{}, BackendTypeGitBranch, json.RawMessage(`{"anything":true}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git-branch checkpoint backend requires a repository")
}
