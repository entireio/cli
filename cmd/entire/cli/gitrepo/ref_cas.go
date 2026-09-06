package gitrepo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	gitfilesystem "github.com/go-git/go-git/v6/storage/filesystem"
)

const refCASWaitDelay = 3 * time.Second

var (
	// ErrRefCASConflict means the ref no longer has the expected old value.
	ErrRefCASConflict = errors.New("git reference changed during compare-and-swap")
	// ErrRefLocked means native Git could not acquire the ref lock.
	ErrRefLocked = errors.New("git reference lock is unavailable")
	// ErrRefSymbolic means the requested CAS target is a symbolic reference.
	ErrRefSymbolic = errors.New("git reference is symbolic")
)

// CompareAndSwapRef atomically updates a direct ref through native Git and
// rejects symbolic refs. ZeroHash as expectedHash requires that refName does
// not exist.
func CompareAndSwapRef(
	ctx context.Context,
	repoRoot string,
	refName plumbing.ReferenceName,
	newHash, expectedHash plumbing.Hash,
) error {
	tx, err := prepareRefCAS(ctx, repoRoot, refName, newHash, expectedHash)
	if err != nil {
		return err
	}

	target, symbolic, err := symbolicRefTarget(ctx, repoRoot, refName)
	if err != nil {
		return errors.Join(err, tx.abort())
	}
	if symbolic {
		return errors.Join(
			fmt.Errorf("ref %s points to %s: %w", refName, target, ErrRefSymbolic),
			tx.abort(),
		)
	}
	return tx.commit()
}

type refCASTransaction struct {
	ctx       context.Context
	refName   plumbing.ReferenceName
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Scanner
	stderr    bytes.Buffer
	done      bool
	committed bool
	canceled  atomic.Bool
}

func prepareRefCAS(
	ctx context.Context,
	repoRoot string,
	refName plumbing.ReferenceName,
	newHash, expectedHash plumbing.Hash,
) (*refCASTransaction, error) {
	if err := refName.Validate(); err != nil {
		return nil, fmt.Errorf("validate ref for compare-and-swap: %w", err)
	}
	oldValue := strings.Repeat("0", newHash.HexSize())
	if expectedHash != plumbing.ZeroHash {
		oldValue = expectedHash.String()
	}

	cmd := exec.CommandContext(ctx, "git", "update-ref", "--stdin")
	cmd.Dir = repoRoot
	cmd.Env = gitPlumbingEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open git update-ref stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open git update-ref stdout: %w", err)
	}
	tx := &refCASTransaction{
		ctx:     ctx,
		refName: refName,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewScanner(stdout),
	}
	cmd.Cancel = func() error {
		tx.canceled.Store(true)
		_ = stdin.Close()
		return nil
	}
	// Closing stdin gives Git a chance to abort cleanly; bound hooks or inherited
	// pipes that prevent the process from finishing after cancellation.
	cmd.WaitDelay = refCASWaitDelay
	cmd.Stderr = &tx.stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start git update-ref transaction for %s: %w", refName, err)
	}

	commands := []struct {
		request  string
		response string
	}{
		{request: "start", response: "start: ok"},
		{request: "option no-deref"},
		{request: fmt.Sprintf("update %s %s %s", refName, newHash, oldValue)},
		{request: "prepare", response: "prepare: ok"},
	}
	for _, command := range commands {
		if err := tx.exchange(command.request, command.response); err != nil {
			return nil, tx.preparationError(err)
		}
	}
	return tx, nil
}

func (tx *refCASTransaction) exchange(request, response string) error {
	if _, err := fmt.Fprintln(tx.stdin, request); err != nil {
		return fmt.Errorf("write %q: %w", request, err)
	}
	if response == "" {
		return nil
	}
	if !tx.stdout.Scan() {
		if err := tx.stdout.Err(); err != nil {
			return fmt.Errorf("read response to %q: %w", request, err)
		}
		return fmt.Errorf("read response to %q: %w", request, io.ErrUnexpectedEOF)
	}
	if got := tx.stdout.Text(); got != response {
		return fmt.Errorf("response to %q was %q, want %q", request, got, response)
	}
	if request == "commit" {
		tx.committed = true
	}
	return nil
}

func (tx *refCASTransaction) preparationError(protocolErr error) error {
	waitErr := tx.wait()
	if waitErr != nil {
		return classifyRefCASError(tx.ctx, tx.cmd.Dir, tx.refName, tx.stderr.Bytes(), waitErr)
	}
	return fmt.Errorf("prepare git update-ref transaction for %s: %w", tx.refName, protocolErr)
}

func (tx *refCASTransaction) commit() error {
	return tx.finish("commit", "commit: ok")
}

func (tx *refCASTransaction) abort() error {
	if tx.done {
		return nil
	}
	return tx.finish("abort", "abort: ok")
}

func (tx *refCASTransaction) finish(request, response string) error {
	protocolErr := tx.exchange(request, response)
	waitErr := tx.wait()
	if waitErr != nil {
		return classifyRefCASError(tx.ctx, tx.cmd.Dir, tx.refName, tx.stderr.Bytes(), waitErr)
	}
	if protocolErr != nil {
		return fmt.Errorf("finish git update-ref transaction for %s: %w", tx.refName, protocolErr)
	}
	return nil
}

func (tx *refCASTransaction) wait() error {
	if tx.done {
		return nil
	}
	tx.done = true
	_ = tx.stdin.Close()
	err := tx.cmd.Wait()
	// Git's commit acknowledgement establishes the durable outcome even if
	// cancellation or a shutdown error arrives before the child is reaped.
	if tx.committed {
		return nil
	}
	if tx.canceled.Load() {
		return fmt.Errorf("wait for git update-ref transaction: %w", tx.ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("wait for git update-ref transaction: %w", err)
	}
	return nil
}

func classifyRefCASError(ctx context.Context, repoRoot string, refName plumbing.ReferenceName, stderr []byte, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("git update-ref %s: %w", refName, ctxErr)
	}
	detail := strings.TrimSpace(string(stderr))
	// Git uses the same unresolved diagnostic for deletion and a directory at
	// the ref path. Only confirmed absence permits a rebuild after this error.
	if isUnresolvedRef(refName, stderr) {
		absent, readErr := refIsAbsent(repoRoot, refName)
		if !absent {
			return fmt.Errorf("git update-ref %s: %s: %w", refName, detail, errors.Join(err, readErr))
		}
	}
	switch {
	case isRefCASConflict(refName, stderr):
		return fmt.Errorf("git update-ref %s: %s: %w", refName, detail, ErrRefCASConflict)
	case isRefLockContention(stderr):
		return fmt.Errorf("git update-ref %s: %s: %w", refName, detail, ErrRefLocked)
	default:
		return fmt.Errorf("git update-ref %s: %s: %w", refName, detail, err)
	}
}

func refIsAbsent(repoRoot string, refName plumbing.ReferenceName) (bool, error) {
	repo, err := OpenPath(repoRoot)
	if err != nil {
		return false, fmt.Errorf("open repository to verify missing ref: %w", err)
	}
	defer repo.Close()
	// go-git falls back to packed refs after any loose-ref read error, so its
	// not-found result alone cannot distinguish a directory from absence.
	if storage, ok := repo.Storer.(*gitfilesystem.Storage); ok {
		info, statErr := storage.Filesystem().Lstat(refName.String())
		if statErr == nil {
			if info.IsDir() {
				return false, fmt.Errorf("ref %s is a directory", refName)
			}
			return false, nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return false, fmt.Errorf("inspect ref %s: %w", refName, statErr)
		}
	}
	_, err = repo.Reference(refName, false)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read ref %s: %w", refName, err)
	}
	return false, nil
}

func symbolicRefTarget(ctx context.Context, repoRoot string, refName plumbing.ReferenceName) (string, bool, error) {
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "-q", "--end-of-options", refName.String())
	cmd.Dir = repoRoot
	cmd.Env = gitPlumbingEnv()
	target, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(target)), true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", false, fmt.Errorf("probe symbolic ref %s: %w", refName, ctxErr)
	}
	var exitErr *exec.ExitError
	var stderr []byte
	if errors.As(err, &exitErr) {
		stderr = exitErr.Stderr
		if refLookupAbsent(err, stderr) {
			return "", false, nil
		}
	}
	detail := strings.TrimSpace(string(stderr))
	if detail == "" {
		return "", false, fmt.Errorf("probe symbolic ref %s: %w", refName, err)
	}
	return "", false, fmt.Errorf("probe symbolic ref %s: %s: %w", refName, detail, err)
}

// isRefCASConflict matches Git's expected-value diagnostics. An unresolved
// files-backend ref also needs an absence check to exclude a directory.
func isRefCASConflict(refName plumbing.ReferenceName, stderr []byte) bool {
	msg := strings.ToLower(strings.TrimSpace(string(stderr)))
	return strings.Contains(msg, "but expected") ||
		isUnresolvedRef(refName, stderr) ||
		strings.Contains(msg, "reference already exists")
}

func isUnresolvedRef(refName plumbing.ReferenceName, stderr []byte) bool {
	msg := strings.ToLower(strings.TrimSpace(string(stderr)))
	unresolved := fmt.Sprintf("unable to resolve reference '%s'", strings.ToLower(refName.String()))
	return strings.HasSuffix(msg, unresolved)
}

func isRefLockContention(stderr []byte) bool {
	msg := strings.ToLower(string(stderr))
	return strings.Contains(msg, "cannot lock references") ||
		strings.Contains(msg, ".lock") && strings.Contains(msg, "file exists")
}
