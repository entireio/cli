package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// ErrRefConflict identifies a native Git compare-and-swap failure.
var ErrRefConflict = errors.New("git ref moved during update")

// ErrRefMoveConflict identifies a failed source/destination ref move.
var ErrRefMoveConflict = errors.New("git ref move conflict")

const (
	refTransactionMaxAttempts = 16
	refTransactionMaxJitter   = 8 * time.Millisecond
)

// repositoryObjectLocks protects go-git's repository-local filesystem object
// cache. Native Git CAS remains the cross-process ref-publication boundary.
var repositoryObjectLocks sync.Map

func repositoryObjectLock(repo *git.Repository) *sync.Mutex {
	lock, _ := repositoryObjectLocks.LoadOrStore(repo, &sync.Mutex{})
	objectLock, ok := lock.(*sync.Mutex)
	if !ok {
		panic("checkpoint repository object lock has an unexpected type")
	}
	return objectLock
}

// RefConflictError reports the expected and observed tips for a failed ref CAS.
type RefConflictError struct {
	Ref      plumbing.ReferenceName
	Expected plumbing.Hash
	Actual   plumbing.Hash
}

// RefMoveConflictError reports a source or destination change that prevented
// an atomic ref move.
type RefMoveConflictError struct {
	Source            plumbing.ReferenceName
	Destination       plumbing.ReferenceName
	ExpectedSource    plumbing.Hash
	ActualSource      plumbing.Hash
	ActualDestination plumbing.Hash
}

func (e *RefMoveConflictError) Error() string {
	return fmt.Sprintf("move %s to %s: %v (source expected %s, found %s; destination found %s)",
		e.Source, e.Destination, ErrRefMoveConflict, e.ExpectedSource, e.ActualSource, e.ActualDestination)
}

func (e *RefMoveConflictError) Unwrap() error {
	return ErrRefMoveConflict
}

func (e *RefConflictError) Error() string {
	return fmt.Sprintf("%s: %v (expected %s, found %s)", e.Ref, ErrRefConflict, e.Expected, e.Actual)
}

func (e *RefConflictError) Unwrap() error {
	return ErrRefConflict
}

// RefMutation rebuilds a ref update from its current tip. changed=false is an
// idempotent no-op and returns current without invoking git update-ref.
type RefMutation func(current plumbing.Hash) (next plumbing.Hash, changed bool, err error)

// RefUpdate describes one compare-and-swap in a multi-ref transaction.
type RefUpdate struct {
	Ref      plumbing.ReferenceName
	New      plumbing.Hash
	Expected plumbing.Hash
}

// MultiRefMutation rebuilds a set of ref updates from their current tips.
// changed=false is an idempotent no-op.
type MultiRefMutation func(current map[plumbing.ReferenceName]plumbing.Hash) (next map[plumbing.ReferenceName]plumbing.Hash, changed bool, err error)

type beforeRefCASKey struct{}

func withBeforeRefCAS(ctx context.Context, hook func()) context.Context {
	return context.WithValue(ctx, beforeRefCASKey{}, hook)
}

// RunRefTransaction retries a logical ref mutation against the latest tip.
// The callback is invoked again after every CAS conflict, so it must rebuild
// trees and commits from current rather than reuse objects based on a stale tip.
func RunRefTransaction(
	ctx context.Context,
	repo *git.Repository,
	refName plumbing.ReferenceName,
	mutate RefMutation,
) (plumbing.Hash, error) {
	for attempt := range refTransactionMaxAttempts {
		if err := ctx.Err(); err != nil {
			return plumbing.ZeroHash, err //nolint:wrapcheck // canonical context cancellation
		}

		current, err := ReadRefHash(repo, refName)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		var next plumbing.Hash
		var changed bool
		objectLock := repositoryObjectLock(repo)
		func() {
			objectLock.Lock()
			defer objectLock.Unlock()
			next, changed, err = mutate(current)
		}()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if !changed {
			return current, nil
		}
		if next.IsZero() {
			return plumbing.ZeroHash, fmt.Errorf("ref transaction %s produced an empty target", refName)
		}

		if hook, ok := ctx.Value(beforeRefCASKey{}).(func()); ok {
			hook()
		}
		err = CompareAndSwapRef(ctx, repo, refName, next, current)
		if err == nil {
			return next, nil
		}
		if !errors.Is(err, ErrRefConflict) {
			return plumbing.ZeroHash, err
		}
		if attempt+1 == refTransactionMaxAttempts {
			return plumbing.ZeroHash, fmt.Errorf("update ref %s after %d attempts: %w", refName, refTransactionMaxAttempts, err)
		}
		if err := refTransactionBackoff(ctx, attempt); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	panic("unreachable")
}

// RunRefTransactions retries a logical mutation that publishes several refs
// atomically. The callback is invoked again after every CAS conflict, so it
// must rebuild all affected objects from the current tips.
func RunRefTransactions(
	ctx context.Context,
	repo *git.Repository,
	refNames []plumbing.ReferenceName,
	mutate MultiRefMutation,
) error {
	if len(refNames) == 0 {
		return errors.New("multi-ref transaction: at least one ref is required")
	}
	ordered := append([]plumbing.ReferenceName(nil), refNames...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for i := 1; i < len(ordered); i++ {
		if ordered[i] == ordered[i-1] {
			return fmt.Errorf("multi-ref transaction: duplicate ref %s", ordered[i])
		}
	}

	for attempt := range refTransactionMaxAttempts {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // canonical context cancellation
		}
		current := make(map[plumbing.ReferenceName]plumbing.Hash, len(ordered))
		for _, refName := range ordered {
			refHash, err := ReadRefHash(repo, refName)
			if err != nil {
				return err
			}
			current[refName] = refHash
		}

		var next map[plumbing.ReferenceName]plumbing.Hash
		var changed bool
		var err error
		objectLock := repositoryObjectLock(repo)
		func() {
			objectLock.Lock()
			defer objectLock.Unlock()
			next, changed, err = mutate(current)
		}()
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}

		updates := make([]RefUpdate, 0, len(ordered))
		for _, refName := range ordered {
			newHash, ok := next[refName]
			if !ok || newHash.IsZero() {
				return fmt.Errorf("multi-ref transaction %s produced an empty target", refName)
			}
			updates = append(updates, RefUpdate{Ref: refName, New: newHash, Expected: current[refName]})
		}
		if err := CompareAndSwapRefs(ctx, repo, updates); err == nil {
			return nil
		} else if !errors.Is(err, ErrRefConflict) {
			return err
		}
		if attempt+1 == refTransactionMaxAttempts {
			return fmt.Errorf("update %d refs after %d attempts: %w", len(ordered), refTransactionMaxAttempts, ErrRefConflict)
		}
		if err := refTransactionBackoff(ctx, attempt); err != nil {
			return err
		}
	}
	panic("unreachable")
}

// ReadRefHash returns a ref's current hash, or ZeroHash when it does not exist.
func ReadRefHash(repo *git.Repository, refName plumbing.ReferenceName) (plumbing.Hash, error) {
	ref, err := repo.Reference(refName, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, nil
	}
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read ref %s: %w", refName, err)
	}
	return ref.Hash(), nil
}

// CompareAndSwapRef atomically updates refName through native Git. expected
// ZeroHash means the ref must not exist. Native Git is the lock interoperability
// boundary across hooks, worktrees, and other Git clients.
func CompareAndSwapRef(
	ctx context.Context,
	repo *git.Repository,
	refName plumbing.ReferenceName,
	newHash, expected plumbing.Hash,
) error {
	return CompareAndSwapRefs(ctx, repo, []RefUpdate{{Ref: refName, New: newHash, Expected: expected}})
}

// CompareAndSwapRefs atomically updates all refs when every expected tip still
// matches. Native Git's update-ref transaction is the lock interoperability
// boundary across hooks, worktrees, and other Git clients.
func CompareAndSwapRefs(ctx context.Context, repo *git.Repository, updates []RefUpdate) error {
	if len(updates) == 0 {
		return errors.New("compare-and-swap refs: at least one update is required")
	}
	ordered := append([]RefUpdate(nil), updates...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Ref < ordered[j].Ref })
	for i, update := range ordered {
		if update.Ref == "" {
			return errors.New("compare-and-swap refs: ref name is required")
		}
		if update.New.IsZero() {
			return fmt.Errorf("compare-and-swap refs %s: new hash is required", update.Ref)
		}
		if i > 0 && ordered[i-1].Ref == update.Ref {
			return fmt.Errorf("compare-and-swap refs: duplicate ref %s", update.Ref)
		}
	}
	root, err := repositoryWorktreeRoot(repo)
	if err != nil {
		return err
	}

	commands := []string{"start"}
	for _, update := range ordered {
		oldValue := strings.Repeat("0", update.New.HexSize())
		if !update.Expected.IsZero() {
			oldValue = update.Expected.String()
		}
		commands = append(commands, fmt.Sprintf("update %s %s %s", update.Ref, update.New, oldValue))
	}
	commands = append(commands, "commit")
	if hook, ok := ctx.Value(beforeRefCASKey{}).(func()); ok {
		hook()
	}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "update-ref", "--stdin")
	cmd.Env = append(gitCommandEnv(), "LC_ALL=C", "LANG=C")
	cmd.Stdin = strings.NewReader(strings.Join(commands, "\n") + "\n")
	output, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // canonical context cancellation
	}

	detail := strings.TrimSpace(string(output))
	actuals := make(map[plumbing.ReferenceName]plumbing.Hash, len(ordered))
	for _, update := range ordered {
		actual, readErr := ReadRefHash(repo, update.Ref)
		if readErr != nil {
			return fmt.Errorf("git update-ref transaction failed (%s), then ref reread failed: %w", detail, readErr)
		}
		actuals[update.Ref] = actual
		if actual != update.Expected {
			return &RefConflictError{Ref: update.Ref, Expected: update.Expected, Actual: actual}
		}
	}
	if strings.Contains(detail, "cannot lock ref") || strings.Contains(detail, "but expected") {
		update := ordered[0]
		return &RefConflictError{Ref: update.Ref, Expected: update.Expected, Actual: actuals[update.Ref]}
	}
	return fmt.Errorf("git update-ref transaction: %s: %w", detail, runErr)
}

// MoveRefIfUnchanged atomically moves expectedSource from sourceRef to
// destinationRef. A missing source with the expected destination is treated as
// an idempotent completion. Any other source or destination mismatch leaves
// both refs unchanged and returns RefMoveConflictError.
func MoveRefIfUnchanged(
	ctx context.Context,
	repo *git.Repository,
	sourceRef, destinationRef plumbing.ReferenceName,
	expectedSource plumbing.Hash,
) error {
	if expectedSource.IsZero() {
		return errors.New("move ref: expected source hash is required")
	}
	if sourceRef == destinationRef {
		return errors.New("move ref: source and destination must differ")
	}

	source, err := ReadRefHash(repo, sourceRef)
	if err != nil {
		return err
	}
	destination, err := ReadRefHash(repo, destinationRef)
	if err != nil {
		return err
	}

	zero := strings.Repeat("0", expectedSource.HexSize())
	commands := []string{"start"}
	switch {
	case source.IsZero() && destination == expectedSource:
		commands = append(commands,
			fmt.Sprintf("verify %s %s", sourceRef, zero),
			fmt.Sprintf("verify %s %s", destinationRef, expectedSource),
		)
	case source == expectedSource && destination.IsZero():
		commands = append(commands,
			fmt.Sprintf("create %s %s", destinationRef, expectedSource),
			fmt.Sprintf("delete %s %s", sourceRef, expectedSource),
		)
	case source == expectedSource && destination == expectedSource:
		commands = append(commands,
			fmt.Sprintf("verify %s %s", destinationRef, expectedSource),
			fmt.Sprintf("delete %s %s", sourceRef, expectedSource),
		)
	default:
		return &RefMoveConflictError{
			Source:            sourceRef,
			Destination:       destinationRef,
			ExpectedSource:    expectedSource,
			ActualSource:      source,
			ActualDestination: destination,
		}
	}
	commands = append(commands, "commit")
	if hook, ok := ctx.Value(beforeRefCASKey{}).(func()); ok {
		hook()
	}

	root, err := repositoryWorktreeRoot(repo)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "update-ref", "--stdin")
	cmd.Env = append(gitCommandEnv(), "LC_ALL=C", "LANG=C")
	cmd.Stdin = strings.NewReader(strings.Join(commands, "\n") + "\n")
	output, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // canonical context cancellation
	}

	actualSource, sourceErr := ReadRefHash(repo, sourceRef)
	actualDestination, destinationErr := ReadRefHash(repo, destinationRef)
	if sourceErr == nil && destinationErr == nil &&
		(actualSource != source || actualDestination != destination) {
		return &RefMoveConflictError{
			Source:            sourceRef,
			Destination:       destinationRef,
			ExpectedSource:    expectedSource,
			ActualSource:      actualSource,
			ActualDestination: actualDestination,
		}
	}
	return fmt.Errorf("git update-ref move %s to %s: %s: %w",
		sourceRef, destinationRef, strings.TrimSpace(string(output)), runErr)
}

func gitCommandEnv() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, value := range env {
		if strings.HasPrefix(value, "GIT_DIR=") ||
			strings.HasPrefix(value, "GIT_WORK_TREE=") ||
			strings.HasPrefix(value, "GIT_INDEX_FILE=") {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}

func repositoryWorktreeRoot(repo *git.Repository) (string, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("open repository worktree: %w", err)
	}
	root := worktree.Filesystem().Root()
	if root == "" {
		return "", errors.New("repository worktree filesystem has no root path")
	}
	return root, nil
}

func refTransactionBackoff(ctx context.Context, attempt int) error {
	limit := refTransactionMaxJitter
	if attempt > 4 {
		limit *= 2
	}
	delay := time.Duration(rand.Int64N(int64(limit))) + time.Millisecond //nolint:gosec // scheduling jitter, not security-sensitive
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // canonical context cancellation
	}
}
