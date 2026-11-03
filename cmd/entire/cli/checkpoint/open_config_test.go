package checkpoint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const fakeMirrorBackendType = "faketest-mirror"

var fakeMirrorBackendOnce sync.Once

// registerFakeMirrorBackend registers a non-git backend so mirror-selection
// paths can be exercised without the real fsstore. Registration is process-wide
// and idempotent (Register panics on duplicates).
func registerFakeMirrorBackend(t *testing.T) {
	t.Helper()
	fakeMirrorBackendOnce.Do(func() {
		Register(fakeMirrorBackendType, func(_ context.Context, _ OpenEnv, _ json.RawMessage) (PersistentStore, error) {
			return &fakePrimary{}, nil
		})
	})
}

func writeRawSettings(t *testing.T, dir, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".entire", paths.SettingsFileName), []byte(body), 0o644))
}

// Not parallel: uses t.Chdir so settings resolve to the test repo.
func TestOpen_DefaultIsGitPrimaryNoMirrors(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)

	stores, err := Open(context.Background(), repo, OpenOptions{})
	require.NoError(t, err)
	// Persistent is always the kind-routing store now (it routes id-keyed reads
	// across the git backends); with a git-branch primary it preserves the git
	// AuthorReader capability.
	_, isRouting := stores.Persistent.(*kindRoutingStoreWithAuthor)
	assert.True(t, isRouting, "default persistent store should be the kind-routing store")
	_, isAuthor := stores.Persistent.(AuthorReader)
	assert.True(t, isAuthor, "routing store should preserve the git primary's AuthorReader")
}

// Under a git-refs primary, a write to a hex checkpoint still on v1 copies it
// into its own ref. The copy needs the router to hold the built-in git stores
// and the caller's remote probe; without them those writes silently go back to
// v1, which git-refs never pushes.
// Not parallel: uses t.Chdir so settings resolve to the test repo.
func TestOpen_RefsPrimaryRouterCopiesLegacyCheckpoints(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	writeRawSettings(t, dir, `{"enabled": true, "checkpoints": {"primary": {"type": "`+BackendTypeGitRefs+`"}}}`)
	probed := false

	stores, err := Open(context.Background(), repo, OpenOptions{
		LegacyCopyRefFetcher: func(context.Context, plumbing.ReferenceName) error {
			probed = true
			return nil
		},
	})
	require.NoError(t, err)

	var router *kindRoutingStore
	switch s := stores.Persistent.(type) {
	case *kindRoutingStore:
		router = s
	case *kindRoutingStoreWithAuthor:
		router = s.kindRoutingStore
	default:
		t.Fatalf("persistent store is %T, want the kind-routing store", stores.Persistent)
	}
	require.NotNil(t, router.legacy, "the router must be able to copy a v1-only checkpoint into its ref")
	require.NotNil(t, router.legacy.refFetcher, "the copy must ask the remote with the caller's probe")
	require.NoError(t, router.legacy.refFetcher(context.Background(), "refs/entire/checkpoints/a1/b2c3d4e5f6"))
	assert.True(t, probed)
}

func TestOpen_RejectsNonGitBackedPrimary(t *testing.T) {
	registerFakeMirrorBackend(t) // a registered, non-git-backed backend
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	writeRawSettings(t, dir, `{"enabled": true, "checkpoints": {"primary": {"type": "`+fakeMirrorBackendType+`"}}}`)

	_, err := Open(context.Background(), repo, OpenOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be the primary")
	assert.Contains(t, err.Error(), "git-backed")
}

func TestOpen_RejectsUnknownPrimary(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	writeRawSettings(t, dir, `{"enabled": true, "checkpoints": {"primary": {"type": "nope"}}}`)

	_, err := Open(context.Background(), repo, OpenOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown checkpoint backend type")
}

func TestOpen_RejectsMirrorOfPrimaryType(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	// A git-branch mirror under a git-branch primary would double-write v1.
	writeRawSettings(t, dir, `{"enabled": true, "checkpoints": {"primary": {"type": "git-branch"}, "mirrors": [{"type": "git-branch"}]}}`)

	_, err := Open(context.Background(), repo, OpenOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most once")
}

func TestOpen_RejectsDuplicateMirrorType(t *testing.T) {
	registerFakeMirrorBackend(t)
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	// Two mirrors of the same type are rejected (one of each type).
	writeRawSettings(t, dir, `{"enabled": true, "checkpoints": {"primary": {"type": "git-branch"}, "mirrors": [{"type": "`+fakeMirrorBackendType+`"}, {"type": "`+fakeMirrorBackendType+`"}]}}`)

	_, err := Open(context.Background(), repo, OpenOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most once")
}

func TestOpen_BuildsConfiguredMirror(t *testing.T) {
	registerFakeMirrorBackend(t)
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	writeRawSettings(t, dir, `{"enabled": true, "checkpoints": {"primary": {"type": "git-branch"}, "mirrors": [{"type": "`+fakeMirrorBackendType+`"}]}}`)

	stores, err := Open(context.Background(), repo, OpenOptions{})
	require.NoError(t, err)

	// The persistent store is the kind-routing store (never the raw git store),
	// and it still exposes AuthorReader (git primary has it).
	_, isGit := stores.Persistent.(*GitStore)
	assert.False(t, isGit, "configured mirror should not expose the raw git store")
	_, isAuthor := stores.Persistent.(AuthorReader)
	assert.True(t, isAuthor, "routing store should preserve the git primary's AuthorReader")
}

func TestOpen_InvalidCheckpointsBlockErrors(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	// Present checkpoints block, but a mirror with no type is invalid.
	writeRawSettings(t, dir, `{"enabled": true, "checkpoints": {"primary": {"type": "git-branch"}, "mirrors": [{"config": {}}]}}`)

	_, err := Open(context.Background(), repo, OpenOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checkpoints")
}

func TestOpen_ToleratesUnrelatedMalformedSettings(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	// summary_generation is the wrong shape and the JSON has no checkpoints
	// block: checkpoint construction must stay fail-soft and default to git.
	writeRawSettings(t, dir, `{"enabled": true, "summary_generation": "not-an-object"}`)

	stores, err := Open(context.Background(), repo, OpenOptions{})
	require.NoError(t, err)
	// Fail-soft default is the git-branch backend, so the routing store still
	// exposes the git AuthorReader capability.
	_, isAuthor := stores.Persistent.(AuthorReader)
	assert.True(t, isAuthor)
}

func TestOpen_ToleratesWholeFileSyntaxError(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	writeRawSettings(t, dir, `{"enabled": true,,}`) // invalid JSON

	stores, err := Open(context.Background(), repo, OpenOptions{})
	require.NoError(t, err)
	// Fail-soft default is the git-branch backend, so the routing store still
	// exposes the git AuthorReader capability.
	_, isAuthor := stores.Persistent.(AuthorReader)
	assert.True(t, isAuthor)
}
