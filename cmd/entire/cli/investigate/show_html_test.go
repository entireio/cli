package investigate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunShow_HTMLWritesFileAndPrintsPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store := NewLocalManifestStoreWithDir(filepath.Join(root, "manifests"))
	t1 := time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC)
	writeShowManifest(t, store, "abcdef012345", "build perf", t1, "quorum", "",
		"## Findings\n\nThe answer is 42.\n",
		map[string]string{"claude-code": "agree"},
	)

	var out bytes.Buffer
	err := RunShow(context.Background(),
		ShowInput{RunID: "abcdef012345", Out: &out, HTML: true},
		ShowDeps{ManifestStore: store},
	)
	if err != nil {
		t.Fatalf("RunShow: %v", err)
	}

	htmlPath := filepath.Join(root, "abcdef012345", "findings.html")
	if !strings.Contains(out.String(), htmlPath) {
		t.Errorf("expected output to print the html path %q, got: %q", htmlPath, out.String())
	}

	data, readErr := os.ReadFile(htmlPath)
	if readErr != nil {
		t.Fatalf("expected findings.html written at %q: %v", htmlPath, readErr)
	}
	html := string(data)
	if !strings.Contains(html, "The answer is 42.") {
		t.Errorf("findings.html missing body, got: %q", html)
	}
	if !strings.Contains(html, "build perf") {
		t.Errorf("findings.html missing banner topic, got: %q", html)
	}
}

func TestRunShow_HTMLNoContentPrintsSoftNotice(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store := NewLocalManifestStoreWithDir(filepath.Join(root, "manifests"))
	t1 := time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC)
	writeShowManifest(t, store, "abcdef012345", "lost run", t1, "cancelled",
		"/tmp/does/not/exist.md", "", nil)

	var out bytes.Buffer
	err := RunShow(context.Background(),
		ShowInput{RunID: "abcdef012345", Out: &out, HTML: true},
		ShowDeps{ManifestStore: store},
	)
	if err != nil {
		t.Fatalf("RunShow: %v", err)
	}
	if !strings.Contains(out.String(), "No findings content available for run abcdef012345.") {
		t.Errorf("expected soft no-content notice, got: %q", out.String())
	}
	if _, statErr := os.Stat(filepath.Join(root, "abcdef012345", "findings.html")); !os.IsNotExist(statErr) {
		t.Errorf("expected no findings.html written when there is no content")
	}
}

func TestRunShow_HTMLPrintsAbsolutePath(t *testing.T) {
	// Not parallel: uses t.Chdir to exercise a relative common-dir path, which
	// is what session.GetGitCommonDir returns from the repo root.
	tmp := t.TempDir()
	t.Chdir(tmp)

	store := NewLocalManifestStoreWithDir(filepath.Join("rel-investigations", "manifests"))
	t1 := time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC)
	writeShowManifest(t, store, "abcdef012345", "rel path", t1, "quorum", "",
		"## Findings\n\nbody\n", nil)

	var out bytes.Buffer
	err := RunShow(context.Background(),
		ShowInput{RunID: "abcdef012345", Out: &out, HTML: true},
		ShowDeps{ManifestStore: store},
	)
	if err != nil {
		t.Fatalf("RunShow: %v", err)
	}

	printed := strings.TrimSpace(strings.TrimPrefix(out.String(), "Wrote "))
	if !filepath.IsAbs(printed) {
		t.Errorf("expected an absolute path to be printed, got: %q", printed)
	}
	if _, statErr := os.Stat(printed); statErr != nil {
		t.Errorf("printed path should exist: %v", statErr)
	}
}

func TestLocalManifestStore_RunDir(t *testing.T) {
	t.Parallel()

	store := NewLocalManifestStoreWithDir(filepath.Join("/repo/.git", InvestigationsDirName, "manifests"))
	got := store.RunDir("abcdef012345")
	want := filepath.Join("/repo/.git", InvestigationsDirName, "abcdef012345")
	if got != want {
		t.Errorf("RunDir = %q, want %q", got, want)
	}
}
