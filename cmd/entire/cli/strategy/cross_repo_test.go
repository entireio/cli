package strategy

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// crossRepoAncestry returns this process's ancestry, skipping where proclive
// cannot introspect processes.
func crossRepoAncestry(t *testing.T, minDepth int) []proclive.Identity {
	t.Helper()
	ancestry, ok := proclive.CurrentAncestry()
	if !ok || len(ancestry.Chain()) < minDepth {
		t.Skip("process ancestry is not available on this platform")
	}
	return ancestry.Chain()
}

func TestCrossRepoPointer_NearestOwnerInAncestryWins(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	chain := crossRepoAncestry(t, 2)
	ctx := context.Background()

	if p := findCrossRepoPointer(ctx); p != nil {
		t.Fatalf("no pointer written, found %+v", p)
	}
	for _, p := range []crossRepoPointer{
		{SessionID: "outer", Owner: chain[1], Worktree: "/c"},
		{SessionID: "inner", Owner: chain[0], Worktree: "/c"},
	} {
		if err := writeCrossRepoPointer(p); err != nil {
			t.Fatal(err)
		}
	}
	p := findCrossRepoPointer(ctx)
	if p == nil || p.SessionID != "inner" {
		t.Fatalf("got %+v, want the nearest owner's session", p)
	}
}

func TestCrossRepoPointer_IgnoresAPointerFiledUnderAnotherOwner(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	chain := crossRepoAncestry(t, 1)
	// Filed under the ancestor's name, but naming a different process: a
	// pointer counts only for the owner it records.
	forged := crossRepoPointer{SessionID: "forged", Owner: proclive.Identity{PID: chain[0].PID, Start: "other", Host: chain[0].Host}}
	if err := writeCrossRepoPointerAs(crossRepoPointerName(chain[0]), forged); err != nil {
		t.Fatal(err)
	}
	if p := findCrossRepoPointer(context.Background()); p != nil {
		t.Fatalf("got %+v, want nothing", p)
	}
}

func TestCrossRepoPointer_GCDropsExitedOwners(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	self, ok := proclive.IdentityOf(os.Getpid())
	if !ok {
		t.Skip("process identity is not available on this platform")
	}
	dead := self
	dead.Start = "exited-long-ago"
	for _, p := range []crossRepoPointer{{SessionID: "live", Owner: self}, {SessionID: "dead", Owner: dead}} {
		if err := writeCrossRepoPointer(p); err != nil {
			t.Fatal(err)
		}
	}
	gcCrossRepoPointers()

	root, err := openCrossRepoPointers(false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := root.Lstat(crossRepoPointerName(self)); err != nil {
		t.Errorf("live owner's pointer removed: %v", err)
	}
	if _, err := root.Lstat(crossRepoPointerName(dead)); !os.IsNotExist(err) {
		t.Errorf("exited owner's pointer kept: %v", err)
	}
}

func TestClearCrossRepoPointers_RemovesTheDirectory(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	if err := writeCrossRepoPointer(crossRepoPointer{SessionID: "s", Owner: proclive.Identity{PID: 1, Start: "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := ClearCrossRepoPointers(); err != nil {
		t.Fatal(err)
	}
	if _, err := openCrossRepoPointers(false); !os.IsNotExist(err) {
		t.Fatalf("pointer directory still there: %v", err)
	}
	if err := ClearCrossRepoPointers(); err != nil {
		t.Fatalf("clearing nothing: %v", err)
	}
}

func TestCrossRepoInstalls_RecordedOnceEach(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	for _, repo := range []string{"/d1", "/d2", "/d1"} {
		if err := recordCrossRepoInstall(repo); err != nil {
			t.Fatal(err)
		}
	}
	got := CrossRepoInstalls()
	if len(got) != 2 || got[0] != "/d1" || got[1] != "/d2" {
		t.Fatalf("got %v", got)
	}
}

func TestLoadCrossRepoHome_SameRepositoryIsNotCrossRepo(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	home, err := loadCrossRepoHome(context.Background(), "s1", dir)
	if !errors.Is(err, errNotCrossRepo) || home != nil {
		t.Fatalf("got %v, %v; want nothing for this repository's own session", home, err)
	}
}

func TestCrossRepoEditedOverlap(t *testing.T) {
	t.Parallel()
	committed := map[string]struct{}{"a.go": {}, "b.go": {}}
	if !editedOverlap(committed, []string{"x.go", "b.go"}) {
		t.Error("overlap missed")
	}
	if editedOverlap(committed, []string{"x.go"}) || editedOverlap(committed, nil) {
		t.Error("overlap without a shared path")
	}
}
