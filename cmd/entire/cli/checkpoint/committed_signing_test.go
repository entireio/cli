package checkpoint

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/x/plugin"
)

type stubSigner struct {
	sig []byte
	err error
}

func (s *stubSigner) Sign(_ io.Reader) ([]byte, error) {
	return s.sig, s.err
}

func setupSigningEnv(t *testing.T, disableSigning bool) {
	t.Helper()

	dir := t.TempDir()

	// Minimal git repo so paths.WorktreeRoot resolves.
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	entireDir := filepath.Join(dir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if disableSigning {
		content := `{"sign_checkpoint_commits": false}`
		if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths.ClearWorktreeRootCache()
	t.Chdir(dir)
	t.Cleanup(func() {
		objectSignerLoader = loadObjectSigner
		paths.ClearWorktreeRootCache()
	})
}

func newTestCommit() *object.Commit {
	sig := object.Signature{
		Name:  "Test",
		Email: "test@test.com",
		When:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	return &object.Commit{
		TreeHash:  plumbing.ZeroHash,
		Author:    sig,
		Committer: sig,
		Message:   "test commit",
	}
}

func TestSignCommitBestEffort_Signs(t *testing.T) { //nolint:paralleltest // t.Chdir requires non-parallel
	setupSigningEnv(t, false)

	objectSignerLoader = func(context.Context) (plugin.Signer, bool) {
		return &stubSigner{sig: []byte("FAKESIG")}, true
	}

	commit := newTestCommit()
	SignCommitBestEffort(context.Background(), commit)

	if commit.Signature != "FAKESIG" {
		t.Errorf("expected signature %q, got %q", "FAKESIG", commit.Signature)
	}
}

func TestSignCommitBestEffort_SkipsWhenDisabled(t *testing.T) { //nolint:paralleltest // t.Chdir requires non-parallel
	setupSigningEnv(t, true)

	objectSignerLoader = func(context.Context) (plugin.Signer, bool) {
		t.Fatal("signer should not be called when signing is disabled")
		return nil, true
	}

	commit := newTestCommit()
	SignCommitBestEffort(context.Background(), commit)

	if commit.Signature != "" {
		t.Errorf("expected empty signature, got %q", commit.Signature)
	}
}

func TestSignCommitBestEffort_ErrorIsBestEffort(t *testing.T) { //nolint:paralleltest // t.Chdir requires non-parallel
	setupSigningEnv(t, false)

	objectSignerLoader = func(context.Context) (plugin.Signer, bool) {
		return &stubSigner{err: errors.New("signing failed")}, true
	}

	commit := newTestCommit()
	SignCommitBestEffort(context.Background(), commit)

	if commit.Signature != "" {
		t.Errorf("expected empty signature after error, got %q", commit.Signature)
	}
}

func TestSignCommitBestEffort_NoSignerRegistered(t *testing.T) { //nolint:paralleltest // t.Chdir requires non-parallel
	setupSigningEnv(t, false)

	objectSignerLoader = func(context.Context) (plugin.Signer, bool) {
		return nil, false
	}

	commit := newTestCommit()
	SignCommitBestEffort(context.Background(), commit)

	if commit.Signature != "" {
		t.Errorf("expected empty signature without signer, got %q", commit.Signature)
	}
}

func TestSignCommitBestEffort_NilSigner(t *testing.T) { //nolint:paralleltest // t.Chdir requires non-parallel
	setupSigningEnv(t, false)

	objectSignerLoader = func(context.Context) (plugin.Signer, bool) {
		return nil, true
	}

	commit := newTestCommit()
	SignCommitBestEffort(context.Background(), commit)

	if commit.Signature != "" {
		t.Errorf("expected empty signature with nil signer, got %q", commit.Signature)
	}
}
