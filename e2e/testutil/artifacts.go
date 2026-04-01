package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ArtifactRoot is the absolute path to the artifact output directory.
// Must be set in TestMain before any tests run.
var ArtifactRoot string

// ArtifactTimestamp is the timestamp subdirectory for this test run.
var ArtifactTimestamp = time.Now().Format("2006-01-02T15-04-05")

var runDirOverride string

// SetRunDir overrides the artifact run directory (e.g. from E2E_ARTIFACT_DIR).
func SetRunDir(dir string) {
	runDirOverride = dir
}

// ArtifactRunDir returns the directory for the current test run.
func ArtifactRunDir() string {
	if runDirOverride != "" {
		return runDirOverride
	}
	return filepath.Join(ArtifactRoot, ArtifactTimestamp)
}

func artifactDir(t *testing.T) string {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "-")
	dir := filepath.Join(ArtifactRunDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("warning: failed to create artifact dir: %v", err)
	}
	return dir
}

// CaptureArtifacts captures git state, checkpoint metadata, entire logs,
// and console output to the artifact directory.
func CaptureArtifacts(t *testing.T, s *RepoState) {
	t.Helper()
	dir := s.ArtifactDir

	writeArtifact(t, dir, "git-log.txt",
		gitOutputSafe(s.Dir, "log", "--decorate", "--graph", "--all"))

	writeArtifact(t, dir, "git-tree.txt",
		gitOutputSafe(s.Dir, "ls-tree", "-r", "HEAD")+
			"\n--- entire/checkpoints/v1 ---\n"+
			gitOutputSafe(s.Dir, "ls-tree", "-r", "entire/checkpoints/v1"))

	// console.log is written incrementally to disk via s.ConsoleLog (*os.File),
	// so it already exists in the artifact dir and survives global timeouts.

	// Capture final pane content from interactive sessions, then close.
	// Must happen here (not in a separate t.Cleanup) because cleanup
	// functions run LIFO — closing first would kill the tmux session
	// before we can capture.
	if s.session != nil {
		pane := s.session.Capture()
		writeArtifact(t, dir, "pane.txt", pane)
		_ = s.session.Close()
	}

	if t.Failed() {
		writeArtifact(t, dir, "FAIL", "")
	} else {
		writeArtifact(t, dir, "PASS", "")
	}

	captureCheckpointMetadata(t, s, dir)
	captureEntireLogs(t, s.Dir, dir)

	if os.Getenv("E2E_KEEP_REPOS") != "" {
		if err := linkRepo(s.Dir, dir); err != nil {
			t.Logf("warning: failed to link repo: %v", err)
		}
	}
}

func captureCheckpointMetadata(t *testing.T, s *RepoState, outDir string) {
	t.Helper()
	commits := NewCheckpointCommits(t, s)
	metaDir := filepath.Join(outDir, "checkpoint-metadata")

	cpIDRe := regexp.MustCompile(`(?m)^Checkpoint:\s+(\S+)`)
	for _, sha := range commits {
		msg := gitOutputSafe(s.Dir, "log", "-1", "--format=%B", sha)
		m := cpIDRe.FindStringSubmatch(msg)
		if m == nil || len(m[1]) < 3 {
			continue
		}
		id := m[1]
		cpPath := CheckpointPath(id)

		cpDir := filepath.Join(metaDir, cpPath)
		_ = os.MkdirAll(cpDir, 0o755)
		raw := gitOutputSafe(s.Dir, "show", sha+":"+cpPath+"/metadata.json")
		writeArtifact(t, cpDir, "metadata.json", raw)

		for i := 0; ; i++ {
			sessPath := fmt.Sprintf("%s/%d/metadata.json", cpPath, i)
			raw := gitOutputSafe(s.Dir, "show", sha+":"+sessPath)
			if raw == "" {
				break
			}
			sessDir := filepath.Join(cpDir, strconv.Itoa(i))
			_ = os.MkdirAll(sessDir, 0o755)
			writeArtifact(t, sessDir, "metadata.json", raw)
		}
	}
}

func captureEntireLogs(t *testing.T, repoDir, outDir string) {
	t.Helper()
	logDir := filepath.Join(repoDir, ".entire", "logs")
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return
	}
	dst := filepath.Join(outDir, "entire-logs")
	_ = os.MkdirAll(dst, 0o755)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(logDir, e.Name()))
		if err != nil {
			continue
		}
		writeArtifact(t, dst, e.Name(), string(data))
	}
}

func writeArtifact(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Logf("warning: failed to write artifact %s: %v", path, err)
	}
}

func gitOutputSafe(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}
