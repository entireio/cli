package repopolicy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
)

func routePolicy(repository Repository, source ActivationSource) RepoPolicy {
	layout := RuntimeWorktree
	if source == ActivationGlobal {
		layout = RuntimeGitCommon
	}
	return RepoPolicy{
		Active:           true,
		ActivationSource: source,
		WorktreeRoot:     repository.WorktreeRoot,
		GitCommonDir:     repository.GitCommonDir,
		WorktreeKey:      repository.WorktreeKey,
		Route:            proposedRoute(repository, layout),
	}
}

func TestEnsureRuntimeRoute_GlobalFirstSelectsGitCommonAndStaysThere(t *testing.T) {
	t.Parallel()
	_, repository := newPolicyRepo(t)
	global, err := EnsureRuntimeRoute(t.Context(), routePolicy(repository, ActivationGlobal))
	if err != nil {
		t.Fatal(err)
	}
	if global.Route.Layout != RuntimeGitCommon {
		t.Fatalf("first route = %+v, want git-common", global.Route)
	}
	local, err := EnsureRuntimeRoute(t.Context(), routePolicy(repository, ActivationLocal))
	if err != nil {
		t.Fatal(err)
	}
	if local.Route != global.Route {
		t.Fatalf("route changed after local activation: first=%+v later=%+v", global.Route, local.Route)
	}
	assertRecordModes(t, repository)
}

func TestEnsureRuntimeRoute_ExplicitFirstSelectsWorktreeAndStaysThere(t *testing.T) {
	t.Parallel()
	_, repository := newPolicyRepo(t)
	local, err := EnsureRuntimeRoute(t.Context(), routePolicy(repository, ActivationLocal))
	if err != nil {
		t.Fatal(err)
	}
	if local.Route.Layout != RuntimeWorktree {
		t.Fatalf("first route = %+v, want worktree", local.Route)
	}
	global, err := EnsureRuntimeRoute(t.Context(), routePolicy(repository, ActivationGlobal))
	if err != nil {
		t.Fatal(err)
	}
	if global.Route != local.Route {
		t.Fatalf("route changed after global activation: first=%+v later=%+v", local.Route, global.Route)
	}
}

func TestEnsureRuntimeRoute_ConcurrentWritersUseOneCompleteWinner(t *testing.T) {
	t.Parallel()
	_, repository := newPolicyRepo(t)
	proposals := []RepoPolicy{routePolicy(repository, ActivationGlobal), routePolicy(repository, ActivationLocal)}
	const writers = 24
	start := make(chan struct{})
	results := make(chan RuntimeRoute, writers)
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(proposal RepoPolicy) {
			defer wg.Done()
			<-start
			policy, err := EnsureRuntimeRoute(t.Context(), proposal)
			if err != nil {
				errs <- err
				return
			}
			results <- policy.Route
		}(proposals[i%len(proposals)])
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("EnsureRuntimeRoute: %v", err)
	}
	var winner RuntimeRoute
	for route := range results {
		if winner.Layout == "" {
			winner = route
		}
		if route != winner {
			t.Errorf("writers observed different routes: winner=%+v got=%+v", winner, route)
		}
	}
	data, err := os.ReadFile(routePath(repository))
	if err != nil {
		t.Fatal(err)
	}
	var persisted RuntimeRoute
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("winner was not complete JSON: %v\n%s", err, data)
	}
	if persisted != winner || persisted.CanonicalWorktree == "" || persisted.CanonicalGitCommon == "" {
		t.Fatalf("persisted winner = %+v, observed %+v", persisted, winner)
	}
}

func TestEnsureRuntimeRoute_CanceledWaiterDoesNotPoisonWinner(t *testing.T) {
	t.Parallel()
	_, repository := newPolicyRepo(t)
	if err := ensureRegistryDir(repository); err != nil {
		t.Fatal(err)
	}
	release, err := flock.Acquire(routePath(repository) + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { release() }()

	waiterCtx, cancel := context.WithCancel(t.Context())
	waiterStarted := make(chan struct{})
	waiterDone := make(chan error, 1)
	go func() {
		close(waiterStarted)
		_, waitErr := EnsureRuntimeRoute(waiterCtx, routePolicy(repository, ActivationGlobal))
		waiterDone <- waitErr
	}()
	<-waiterStarted
	select {
	case waitErr := <-waiterDone:
		t.Fatalf("waiter returned before cancellation while lock was held: %v", waitErr)
	case <-time.After(75 * time.Millisecond):
		// Three lock polling intervals establish that the waiter is contending.
	}
	cancel()
	if waitErr := <-waiterDone; !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("canceled EnsureRuntimeRoute = %v, want context.Canceled", waitErr)
	}
	if _, found, readErr := ReadRuntimeRoute(repository); readErr != nil || found {
		t.Fatalf("canceled waiter published route: found=%v err=%v", found, readErr)
	}
	release()
	release = func() {}
	winner, err := EnsureRuntimeRoute(t.Context(), routePolicy(repository, ActivationLocal))
	if err != nil {
		t.Fatal(err)
	}
	if winner.Route.Layout != RuntimeWorktree {
		t.Fatalf("winner = %+v, canceled proposal must not publish", winner.Route)
	}
}

func TestEnsureRuntimeRoute_LinkedWorktreesAreIndependent(t *testing.T) {
	t.Parallel()
	root, mainRepo := newPolicyRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	runPolicyGit(t, root, "worktree", "add", "-b", "linked-route", linked)
	linkedRepo, err := ResolveRepositoryAt(t.Context(), linked)
	if err != nil {
		t.Fatal(err)
	}
	mainPolicy, err := EnsureRuntimeRoute(t.Context(), routePolicy(mainRepo, ActivationGlobal))
	if err != nil {
		t.Fatal(err)
	}
	linkedPolicy, err := EnsureRuntimeRoute(t.Context(), routePolicy(linkedRepo, ActivationLocal))
	if err != nil {
		t.Fatal(err)
	}
	if mainPolicy.Route.Layout != RuntimeGitCommon || linkedPolicy.Route.Layout != RuntimeWorktree {
		t.Fatalf("main=%+v linked=%+v", mainPolicy.Route, linkedPolicy.Route)
	}
	if routePath(mainRepo) == routePath(linkedRepo) {
		t.Fatal("linked worktrees share a route path")
	}
}

func TestEnsureRuntimeRoute_CorruptRecordFailsClosed(t *testing.T) {
	t.Parallel()
	_, repository := newPolicyRepo(t)
	if err := ensureRegistryDir(repository); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(routePath(repository), []byte(`{"version":1,"layout":"git_common"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureRuntimeRoute(t.Context(), routePolicy(repository, ActivationGlobal)); err == nil {
		t.Fatal("corrupt route record must fail closed")
	}
}

func assertRecordModes(t *testing.T, repository Repository) {
	t.Helper()
	if runtime.GOOS == windowsGOOS {
		return
	}
	dirInfo, err := os.Stat(registryDir(repository))
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(routePath(repository))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("registry mode = %o, want 700", got)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("route mode = %o, want 600", got)
	}
}
