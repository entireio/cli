package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The name and header IDs of a session file as `entire stream pi` makes it:
// it names the file, and Pi writes its own ID in the header.
const (
	testNameID   = "019a0000-0000-7000-8000-000000000001"
	testHeaderID = "019a0000-0000-7000-8000-0000000000ff"
)

// piSessionStoreForTest points Pi's session store at a temp dir and returns
// it, with a project folder inside as Pi makes one.
func piSessionStoreForTest(t *testing.T) (store, project string) {
	t.Helper()
	store = t.TempDir()
	t.Setenv(piSessionStoreEnvVar, store)
	t.Setenv(piSessionDirEnvVar, "")
	project = filepath.Join(store, "--repo--")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	return store, project
}

func writeSessionFile(t *testing.T, dir, firstLine string) string {
	t.Helper()
	path := filepath.Join(dir, "2026-11-26T12-00-00-000Z_"+testNameID+".jsonl")
	if err := os.WriteFile(path, []byte(firstLine+"\n{\"type\":\"message\",\"text\":\"SECRET\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHeaderSessionID_ReadsTheHeaderNotTheName(t *testing.T) { //nolint:paralleltest // t.Setenv
	_, project := piSessionStoreForTest(t)
	path := writeSessionFile(t, project, `{"type":"session","version":3,"id":"`+testHeaderID+`","timestamp":"2026-11-26T12:00:00.000Z","cwd":"/repo"}`)

	if got := (&PiAgent{}).HeaderSessionID(path); got != testHeaderID {
		t.Fatalf("HeaderSessionID = %q, want the header's %q", got, testHeaderID)
	}
	if got := extractSessionIDFromPath(path); got != testNameID {
		t.Fatalf("the hook's ID stays the name's: got %q", got)
	}
}

func TestHeaderSessionID_RefusesWhatItCannotTrust(t *testing.T) { //nolint:paralleltest // t.Setenv
	store, project := piSessionStoreForTest(t)
	header := `{"type":"session","id":"` + testHeaderID + `"}`

	outside := writeSessionFile(t, t.TempDir(), header)
	link := filepath.Join(project, "2026-11-26T12-00-00-000Z_link.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"empty path":          "",
		"outside the store":   outside,
		"a symbolic link":     link,
		"missing":             filepath.Join(project, "nope.jsonl"),
		"not a session line":  writeSessionFileNamed(t, project, "a.jsonl", `{"type":"message","id":"`+testHeaderID+`"}`),
		"not JSON":            writeSessionFileNamed(t, project, "b.jsonl", "hello"),
		"hostile id":          writeSessionFileNamed(t, project, "c.jsonl", `{"type":"session","id":"x\ny"}`),
		"traversal id":        writeSessionFileNamed(t, project, "d.jsonl", `{"type":"session","id":"../x"}`),
		"first line too long": writeSessionFileNamed(t, project, "e.jsonl", `{"type":"session","id":"`+testHeaderID+`","pad":"`+strings.Repeat("x", 128<<10)+`"}`),
		"the store itself":    store,
	}
	for name, path := range cases {
		if got := (&PiAgent{}).HeaderSessionID(path); got != "" {
			t.Errorf("%s: HeaderSessionID = %q, want none", name, got)
		}
	}
}

func writeSessionFileNamed(t *testing.T, dir, name, firstLine string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(firstLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
