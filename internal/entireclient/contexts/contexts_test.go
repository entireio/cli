package contexts_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/internal/entireclient/contexts"
)

func TestLoad_MissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	f, err := contexts.Load(dir)
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if f == nil {
		t.Fatal("Load returned nil File")
	}
	if f.CurrentContext != "" || len(f.Contexts) != 0 {
		t.Errorf("expected zero-valued File, got %+v", f)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := &contexts.File{
		CurrentContext: "eu-paul",
		Contexts: []*contexts.Context{
			{Name: "eu-paul", CoreURL: "https://eu.example", Handle: "paul", KeychainService: "entire-core:eu-paul"},
			{Name: "us-superadmin", CoreURL: "https://us.example", Handle: "superadmin", KeychainService: "entire-core:us-superadmin"},
		},
	}
	if err := contexts.Save(dir, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := contexts.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("round-trip mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

func TestSave_WritesAtomically_AndChmod600(t *testing.T) {
	dir := t.TempDir()
	if err := contexts.Save(dir, &contexts.File{CurrentContext: "x"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := filepath.Join(dir, "contexts.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file perm = %#o, want 0600", perm)
	}

	// No temp file left behind on success.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "contexts.json" || e.Name() == "contexts.json.lock" {
			continue
		}
		t.Errorf("leftover file in config dir: %s", e.Name())
	}
}

func TestUpsert_FirstAddBecomesCurrent(t *testing.T) {
	f := &contexts.File{}
	c := &contexts.Context{Name: "first", CoreURL: "https://a.example", Handle: "alice"}
	f.Upsert(c)
	if f.CurrentContext != "first" {
		t.Errorf("CurrentContext = %q, want %q", f.CurrentContext, "first")
	}
	if len(f.Contexts) != 1 || f.Contexts[0] != c {
		t.Errorf("Contexts = %v, want one entry", f.Contexts)
	}
}

func TestUpsert_SecondAddDoesNotChangeCurrent(t *testing.T) {
	f := &contexts.File{}
	f.Upsert(&contexts.Context{Name: "first"})
	f.Upsert(&contexts.Context{Name: "second"})
	if f.CurrentContext != "first" {
		t.Errorf("CurrentContext = %q, want %q (a later upsert must not steal current)", f.CurrentContext, "first")
	}
	if len(f.Contexts) != 2 {
		t.Errorf("len(Contexts) = %d, want 2", len(f.Contexts))
	}
}

func TestUpsert_ReplacesByName(t *testing.T) {
	f := &contexts.File{}
	f.Upsert(&contexts.Context{Name: "x", Handle: "old"})
	f.Upsert(&contexts.Context{Name: "x", Handle: "new"})
	if len(f.Contexts) != 1 {
		t.Errorf("len(Contexts) = %d, want 1 (upsert must replace, not append)", len(f.Contexts))
	}
	if f.Contexts[0].Handle != "new" {
		t.Errorf("Handle = %q, want %q", f.Contexts[0].Handle, "new")
	}
}

func TestDelete_DropsContext(t *testing.T) {
	f := &contexts.File{
		CurrentContext: "stays",
		Contexts: []*contexts.Context{
			{Name: "stays"},
			{Name: "doomed"},
		},
	}
	f.Delete("doomed")
	if f.Find("doomed") != nil {
		t.Error("Delete left context behind")
	}
	if f.Find("stays") == nil {
		t.Error("Delete dropped an unrelated context")
	}
	if f.CurrentContext != "stays" {
		t.Errorf("CurrentContext = %q, want %q (untouched)", f.CurrentContext, "stays")
	}
}

func TestDelete_OfCurrentClearsCurrent(t *testing.T) {
	f := &contexts.File{
		CurrentContext: "first",
		Contexts: []*contexts.Context{
			{Name: "first"},
			{Name: "second"},
		},
	}
	f.Delete("first")
	if f.CurrentContext != "" {
		t.Errorf("after deleting current, CurrentContext = %q, want empty (no fallback to another identity)", f.CurrentContext)
	}
	if f.Find("second") == nil {
		t.Error("Delete dropped the unrelated remaining context")
	}
}

func TestDelete_OfLastClearsCurrent(t *testing.T) {
	f := &contexts.File{
		CurrentContext: "only",
		Contexts:       []*contexts.Context{{Name: "only"}},
	}
	f.Delete("only")
	if f.CurrentContext != "" {
		t.Errorf("CurrentContext = %q, want empty after deleting the last context", f.CurrentContext)
	}
	if len(f.Contexts) != 0 {
		t.Errorf("len(Contexts) = %d, want 0", len(f.Contexts))
	}
}

func TestModify_AppliesAndPersists(t *testing.T) {
	dir := t.TempDir()

	if err := contexts.Modify(dir, func(f *contexts.File) (bool, error) {
		f.Upsert(&contexts.Context{Name: "alice"})
		f.CurrentContext = "alice"
		return true, nil
	}); err != nil {
		t.Fatalf("Modify: %v", err)
	}

	got, err := contexts.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CurrentContext != "alice" || got.Find("alice") == nil {
		t.Errorf("Modify did not persist: %+v", got)
	}
}

func TestModify_NoChangeSkipsWrite(t *testing.T) {
	dir := t.TempDir()
	// Seed a baseline file.
	if err := contexts.Save(dir, &contexts.File{CurrentContext: "x"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := filepath.Join(dir, "contexts.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Sleep enough for mtime to tick on filesystems with coarse resolution.
	time.Sleep(10 * time.Millisecond)

	if err := contexts.Modify(dir, func(_ *contexts.File) (bool, error) {
		return false, nil
	}); err != nil {
		t.Fatalf("Modify: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("Modify rewrote file despite changed=false (mtime before=%v after=%v)", before.ModTime(), after.ModTime())
	}
}

func TestModify_ErrorDiscardsChange(t *testing.T) {
	dir := t.TempDir()
	// Seed: a baseline current_context the test will try to mutate-and-fail.
	if err := contexts.Save(dir, &contexts.File{CurrentContext: "original"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	wantErr := errors.New("nope")
	err := contexts.Modify(dir, func(f *contexts.File) (bool, error) {
		f.CurrentContext = "mutated"
		return true, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Modify error = %v, want %v", err, wantErr)
	}

	got, err := contexts.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CurrentContext != "original" {
		t.Errorf("CurrentContext = %q, want %q (error must roll back)", got.CurrentContext, "original")
	}
}

func TestModify_HoldsLockAcrossLoadAndSave(t *testing.T) {
	// Concurrent Modify calls each increment a counter into a custom field
	// (here: stash count in CurrentContext as a string). With proper
	// locking the final value equals the number of Modify calls.
	// Without locking, lost updates leave it lower.
	const n = 25
	dir := t.TempDir()
	if err := contexts.Save(dir, &contexts.File{CurrentContext: "0"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			if err := contexts.Modify(dir, func(f *contexts.File) (bool, error) {
				cur, err := strconv.Atoi(f.CurrentContext)
				if err != nil {
					return false, err
				}
				f.CurrentContext = strconv.Itoa(cur + 1)
				return true, nil
			}); err != nil {
				t.Errorf("Modify: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := contexts.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CurrentContext != strconv.Itoa(n) {
		t.Errorf("CurrentContext = %q, want %q (lost updates indicate non-atomic RMW)", got.CurrentContext, strconv.Itoa(n))
	}
}

// The roots refuse a relative config dir, but only at the read. FilePath runs
// several steps earlier and used to create the directory and let lockFile drop a
// .lock inside it on the way to reporting the refusal, which is the mistake
// itself: for a CLI run from a repository, ./<value> is inside the repository.
func TestLoad_RefusesRelativeConfigDirBeforeCreatingIt(t *testing.T) {
	t.Chdir(t.TempDir())

	if _, err := contexts.Load("relative-config"); err == nil {
		t.Fatal("Load() = nil error, want a rejected override")
	}
	if _, err := os.Stat("relative-config"); err == nil {
		t.Error("Load() created the directory it was refusing")
	}
}

func TestSave_RefusesRelativeConfigDirBeforeCreatingIt(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := contexts.Save("relative-config", &contexts.File{}); err == nil {
		t.Fatal("Save() = nil error, want a rejected override")
	}
	if _, err := os.Stat("relative-config"); err == nil {
		t.Error("Save() created the directory it was refusing")
	}
}

// TestEveryEntryPointRefusesRelativeConfigDirWithoutSideEffects covers all four
// public entry points, not just the two that had tests.
//
// The refusal has to land before EnsurePrivateDir and before lockFile: configRoot
// refuses a relative directory too, but only at the READ, by which point
// ./<value> exists with a .lock inside it. For a CLI run from a repository that
// is a directory inside the repository, which is the mistake the check exists to
// report, so it must not happen on the way to reporting it.
func TestEveryEntryPointRefusesRelativeConfigDirWithoutSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(dir string) error
	}{
		{"FilePath", func(d string) error { _, err := contexts.FilePath(d); return err }},
		{"Load", func(d string) error { _, err := contexts.Load(d); return err }},
		{"Save", func(d string) error { return contexts.Save(d, &contexts.File{}) }},
		{"Modify", func(d string) error {
			return contexts.Modify(d, func(*contexts.File) (bool, error) { return true, nil })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			t.Chdir(cwd)

			err := tc.call("relative-config")
			if err == nil {
				t.Fatalf("%s(%q) = nil error, want a rejected override", tc.name, "relative-config")
			}
			if !strings.Contains(err.Error(), "absolute") {
				t.Errorf("%s error = %q, want it to name the absolute-path requirement", tc.name, err)
			}

			// Nothing at all, not merely no target directory: the lock file is
			// created beside the target and would land in the cwd too.
			entries, readErr := os.ReadDir(cwd)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, e := range entries {
				t.Errorf("%s left %q behind in the working directory", tc.name, e.Name())
			}
		})
	}
}

// Entire's OWN fallback must stay usable. When os.UserHomeDir fails, configDir
// returns its home-relative default, and rejecting that alongside a genuine
// user override broke every command that touches a saved login on a machine
// with no resolvable home. userdirs absolutizes it instead; this pins that the
// consumer accepts what the resolver now produces.
func TestLoad_AcceptsTheResolversOwnFallback(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	dir := filepath.Join(cwd, ".config", "entire")
	if _, err := contexts.Load(dir); err != nil {
		t.Fatalf("Load(%q) = %v, want the resolver's own fallback honored", dir, err)
	}
}
