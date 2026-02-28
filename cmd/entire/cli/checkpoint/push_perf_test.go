//go:build pushperf

package checkpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	perfTestRepoOwner = "evisdren"
	perfTestRepoName  = "entire-push-perf-test"
)

// Size tier boundaries for bucketing real transcripts.
var sizeTiers = []struct {
	name      string
	targetKey int // matches scenario transcriptSize
	minBytes  int64
	maxBytes  int64
}{
	{"100KB", 100 * 1024, 50 * 1024, 200 * 1024},
	{"500KB", 500 * 1024, 200 * 1024, 1024 * 1024},
	{"2500KB", 2500 * 1024, 1024 * 1024, 5 * 1024 * 1024},
	{"10MB", 10 * 1024 * 1024, 5 * 1024 * 1024, 20 * 1024 * 1024},
	{"50MB", 50 * 1024 * 1024, 20 * 1024 * 1024, 100 * 1024 * 1024},
}

// TestPushPerformance measures real push times to GitHub for the entire/checkpoints/v1 branch
// using real transcript data from the source repo.
//
// Prerequisites:
//   - Must be run from a repo that has an entire/checkpoints/v1 branch with checkpoint data
//   - Requires `gh` CLI authenticated with delete_repo scope
//
// Run with: go test -v -run TestPushPerformance -tags pushperf -timeout 30m ./cmd/entire/cli/checkpoint/
func TestPushPerformance(t *testing.T) {
	fullName := perfTestRepoOwner + "/" + perfTestRepoName

	// Load real transcripts from source repo.
	src := newTranscriptSource(t)

	// Create the GitHub repo once for all subtests.
	repoURL := createTempGitHubRepo(t, fullName)
	t.Cleanup(func() {
		deleteTempGitHubRepo(t, fullName)
	})

	type scenario struct {
		name           string
		checkpoints    int
		transcriptSize int // used to pick the size tier
	}

	firstPush := []scenario{
		// Small sessions
		{"10cp_100KB", 10, 100 * 1024},
		{"50cp_500KB", 50, 500 * 1024},
		// Average-sized sessions (~2.8MB real-world avg)
		{"50cp_2500KB", 50, 2500 * 1024},
		{"100cp_2500KB", 100, 2500 * 1024},
		{"200cp_2500KB", 200, 2500 * 1024},
		// Heavy sessions
		{"50cp_10MB", 50, 10 * 1024 * 1024},
		// Near-max sessions
		{"10cp_50MB", 10, 50 * 1024 * 1024},
	}

	for _, sc := range firstPush {
		t.Run("FirstPush/"+sc.name, func(t *testing.T) {
			runFirstPushScenario(t, repoURL, src, sc.checkpoints, sc.transcriptSize)
		})
	}

	incremental := []scenario{
		{"50cp_base_500KB", 50, 500 * 1024},
		{"50cp_base_2500KB", 50, 2500 * 1024},
		{"200cp_base_2500KB", 200, 2500 * 1024},
		{"50cp_base_10MB", 50, 10 * 1024 * 1024},
	}

	for _, sc := range incremental {
		t.Run("Incremental/"+sc.name, func(t *testing.T) {
			runIncrementalScenario(t, repoURL, src, sc.checkpoints, sc.transcriptSize)
		})
	}
}

// runFirstPushScenario creates a local repo with N checkpoints and times the first push.
func runFirstPushScenario(t *testing.T, remoteURL string, src *transcriptSource, numCheckpoints, transcriptSize int) {
	t.Helper()

	dir := t.TempDir()
	repo := initLocalRepo(t, dir)
	store := NewGitStore(repo)

	seedDur, totalBytes := seedCheckpoints(t, store, src, numCheckpoints, transcriptSize)
	gcDur := timeGitGC(t, dir)

	looseKB, packKB, totalKB := measureRepoSize(t, dir)
	addRemote(t, dir, remoteURL)

	pushDur := timeForcePush(t, dir, "origin", "entire/checkpoints/v1")

	totalRawMB := float64(totalBytes) / (1024 * 1024)
	throughput := float64(0)
	if pushDur.Seconds() > 0 {
		throughput = totalRawMB / pushDur.Seconds()
	}

	t.Logf("\n=== %s ===", t.Name())
	t.Logf("  Checkpoints:    %d", numCheckpoints)
	t.Logf("  Tier:           %s (real transcripts)", tierNameForSize(transcriptSize))
	t.Logf("  Total raw:      %.1f MB", totalRawMB)
	t.Logf("  Loose objects:  %d KB", looseKB)
	t.Logf("  Pack size:      %d KB", packKB)
	t.Logf("  Total .git:     %d KB", totalKB)
	t.Logf("  Compression:    %.1f%% (pack/raw)", float64(packKB)*1024/float64(totalBytes)*100)
	t.Logf("  Seed duration:  %s", seedDur.Round(time.Millisecond))
	t.Logf("  GC duration:    %s", gcDur.Round(time.Millisecond))
	t.Logf("  Push duration:  %s", pushDur.Round(time.Millisecond))
	t.Logf("  Throughput:     %.1f MB/s (raw data / wall time)", throughput)
}

// runIncrementalScenario pushes a base set of checkpoints, then adds 1 and times the incremental push.
func runIncrementalScenario(t *testing.T, remoteURL string, src *transcriptSource, baseCheckpoints, transcriptSize int) {
	t.Helper()

	dir := t.TempDir()
	repo := initLocalRepo(t, dir)
	store := NewGitStore(repo)

	// Seed and push baseline.
	seedDur, totalBytes := seedCheckpoints(t, store, src, baseCheckpoints, transcriptSize)
	gcDur := timeGitGC(t, dir)
	addRemote(t, dir, remoteURL)

	basePushDur := timeForcePush(t, dir, "origin", "entire/checkpoints/v1")

	// Reopen repo after GC so go-git sees the repacked objects.
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen after gc: %v", err)
	}
	store = NewGitStore(repo)

	// Add 1 more checkpoint.
	_, incrBytes := seedCheckpoints(t, store, src, 1, transcriptSize)
	timeGitGC(t, dir)

	_, _, totalKB := measureRepoSize(t, dir)

	incDur := timePush(t, dir, "origin", "entire/checkpoints/v1")

	incrRawMB := float64(incrBytes) / (1024 * 1024)
	throughput := float64(0)
	if incDur.Seconds() > 0 {
		throughput = incrRawMB / incDur.Seconds()
	}

	t.Logf("\n=== %s ===", t.Name())
	t.Logf("  Base checkpoints:  %d", baseCheckpoints)
	t.Logf("  Tier:              %s (real transcripts)", tierNameForSize(transcriptSize))
	t.Logf("  Total raw:         %.1f MB (base)", float64(totalBytes)/(1024*1024))
	t.Logf("  Total .git:        %d KB", totalKB)
	t.Logf("  Seed duration:     %s (base)", seedDur.Round(time.Millisecond))
	t.Logf("  GC duration:       %s (base)", gcDur.Round(time.Millisecond))
	t.Logf("  Base push:         %s", basePushDur.Round(time.Millisecond))
	t.Logf("  Incremental push:  %s (1 new × %s)", incDur.Round(time.Millisecond), formatBytes(incrBytes))
	t.Logf("  Throughput:        %.1f MB/s (raw data / wall time)", throughput)
}

// --- Transcript source ---

// blobRef is a reference to a transcript blob in the source repo.
type blobRef struct {
	hash plumbing.Hash
	size int64
}

// transcriptSource provides real transcript data from the source repo's
// entire/checkpoints/v1 branch, bucketed by size tier and loaded lazily.
type transcriptSource struct {
	repo  *gogit.Repository
	tiers map[int][]blobRef // targetKey -> blob refs sorted by size
}

// newTranscriptSource scans the source repo's entire/checkpoints/v1 branch
// and indexes all full.jsonl blobs by size tier.
func newTranscriptSource(t *testing.T) *transcriptSource {
	t.Helper()

	// Find repo root.
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("not in a git repo: %v", err)
	}
	repoRoot := strings.TrimSpace(string(out))

	repo, err := gogit.PlainOpen(repoRoot)
	if err != nil {
		t.Fatalf("open source repo: %v", err)
	}

	// Resolve entire/checkpoints/v1 branch.
	ref, err := repo.Reference(plumbing.ReferenceName("refs/heads/entire/checkpoints/v1"), true)
	if err != nil {
		t.Fatalf("entire/checkpoints/v1 branch not found (is this repo using Entire?): %v", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("resolve commit: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("resolve tree: %v", err)
	}

	// Scan tree for full.jsonl entries and bucket by size tier.
	tiers := make(map[int][]blobRef)
	err = tree.Files().ForEach(func(f *object.File) error {
		if !strings.HasSuffix(f.Name, "full.jsonl") {
			return nil
		}
		for _, tier := range sizeTiers {
			if f.Size >= tier.minBytes && f.Size < tier.maxBytes {
				tiers[tier.targetKey] = append(tiers[tier.targetKey], blobRef{hash: f.Hash, size: f.Size})
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan tree: %v", err)
	}

	// Sort each tier by size for deterministic ordering.
	for key := range tiers {
		sort.Slice(tiers[key], func(i, j int) bool {
			return tiers[key][i].size < tiers[key][j].size
		})
	}

	// Log what we found.
	for _, tier := range sizeTiers {
		refs := tiers[tier.targetKey]
		if len(refs) > 0 {
			var totalSize int64
			for _, r := range refs {
				totalSize += r.size
			}
			t.Logf("  tier %-6s: %3d transcripts, avg %s", tier.name, len(refs), formatBytes(totalSize/int64(len(refs))))
		}
	}

	return &transcriptSource{repo: repo, tiers: tiers}
}

// get returns the content of a real transcript blob for the given size tier and index.
// Cycles through available blobs if index exceeds the pool size.
func (s *transcriptSource) get(t *testing.T, targetSize, index int) []byte {
	t.Helper()

	refs, ok := s.tiers[targetSize]
	if !ok || len(refs) == 0 {
		t.Fatalf("no real transcripts available for tier %s", tierNameForSize(targetSize))
	}

	ref := refs[index%len(refs)]
	blob, err := s.repo.BlobObject(ref.hash)
	if err != nil {
		t.Fatalf("read blob %s: %v", ref.hash, err)
	}

	reader, err := blob.Reader()
	if err != nil {
		t.Fatalf("blob reader %s: %v", ref.hash, err)
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read blob content %s: %v", ref.hash, err)
	}

	return content
}

// tierNameForSize returns the human-readable tier name for a target size.
func tierNameForSize(targetSize int) string {
	for _, tier := range sizeTiers {
		if tier.targetKey == targetSize {
			return tier.name
		}
	}
	return formatBytes(int64(targetSize))
}

// --- Checkpoint seeding ---

// seedCheckpoints writes N checkpoints using real transcripts from the source repo.
// Returns the seeding duration and total bytes written.
func seedCheckpoints(t *testing.T, store *GitStore, src *transcriptSource, count, transcriptSize int) (time.Duration, int64) {
	t.Helper()

	ctx := context.Background()
	start := time.Now()
	var totalBytes int64

	// Log progress for large scenarios.
	logInterval := progressInterval(count, transcriptSize)

	for i := range count {
		if logInterval > 0 && i > 0 && i%logInterval == 0 {
			t.Logf("  seeding: %d/%d checkpoints (%s elapsed)", i, count, time.Since(start).Round(time.Millisecond))
		}

		transcript := src.get(t, transcriptSize, i)
		totalBytes += int64(len(transcript))

		cpID, err := id.Generate()
		if err != nil {
			t.Fatalf("id.Generate: %v", err)
		}
		err = store.WriteCommitted(ctx, WriteCommittedOptions{
			CheckpointID:     cpID,
			SessionID:        fmt.Sprintf("perf-session-%d", i),
			Agent:            agent.AgentTypeClaudeCode,
			Transcript:       transcript,
			Prompts:          []string{"Implement the feature"},
			CheckpointsCount: 1,
			AuthorName:       "Push Perf Test",
			AuthorEmail:      "test@example.com",
		})
		if err != nil {
			t.Fatalf("WriteCommitted checkpoint %d: %v", i, err)
		}
	}

	return time.Since(start), totalBytes
}

// progressInterval returns how often to log seeding progress, or 0 to skip.
func progressInterval(count, transcriptSize int) int {
	totalBytes := int64(count) * int64(transcriptSize)
	switch {
	case totalBytes > 500*1024*1024: // >500MB total
		return 5
	case totalBytes > 100*1024*1024: // >100MB total
		return 10
	case totalBytes > 20*1024*1024: // >20MB total
		return 25
	default:
		return 0 // no progress logging for small scenarios
	}
}

// --- Helper functions ---

// createTempGitHubRepo creates a private GitHub repo and returns its clone URL.
func createTempGitHubRepo(t *testing.T, fullName string) string {
	t.Helper()

	// Delete any leftover repo from a previous failed run.
	//nolint:gosec // test-only helper, fullName is a constant
	cleanupCmd := exec.Command("gh", "repo", "delete", fullName, "--yes")
	_ = cleanupCmd.Run() // ignore error if repo doesn't exist

	//nolint:gosec // test-only helper, fullName is a constant
	cmd := exec.Command("gh", "repo", "create", fullName, "--private", "--clone=false")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gh repo create failed: %v\n%s", err, out)
	}

	return fmt.Sprintf("https://github.com/%s.git", fullName)
}

// deleteTempGitHubRepo deletes the GitHub repo.
func deleteTempGitHubRepo(t *testing.T, fullName string) {
	t.Helper()

	//nolint:gosec // test-only helper, fullName is a constant
	cmd := exec.Command("gh", "repo", "delete", fullName, "--yes")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("warning: gh repo delete failed (may need manual cleanup): %v\n%s", err, out)
	}
}

// initLocalRepo initializes a git repo with an initial commit.
func initLocalRepo(t *testing.T, dir string) *gogit.Repository {
	t.Helper()

	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	// Create a README so we have a valid initial commit.
	readmePath := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmePath, []byte("# push perf test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := wt.Commit("initial commit", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Push Perf Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	return repo
}

// addRemote adds a git remote to the local repo.
func addRemote(t *testing.T, dir, url string) {
	t.Helper()

	cmd := exec.Command("git", "remote", "add", "origin", url)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Remote may already exist from a previous call; try set-url instead.
		cmd2 := exec.Command("git", "remote", "set-url", "origin", url)
		cmd2.Dir = dir
		out2, err2 := cmd2.CombinedOutput()
		if err2 != nil {
			t.Fatalf("git remote add/set-url failed: %v\n%s\n%s", err, out, out2)
		}
	}
}

// timeGitGC runs git gc to pack objects and returns the duration.
func timeGitGC(t *testing.T, dir string) time.Duration {
	t.Helper()

	start := time.Now()
	cmd := exec.Command("git", "gc", "--aggressive", "--prune=now")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git gc: %v\n%s", err, out)
	}
	return time.Since(start)
}

// formatBytes returns a human-readable size string.
func formatBytes(b int64) string {
	switch {
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%d KB", b/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// measureRepoSize returns the loose object size, pack size, and total .git size in KB.
func measureRepoSize(t *testing.T, dir string) (looseKB, packKB, totalKB int64) {
	t.Helper()

	// Parse git count-objects -v for loose/pack sizes.
	cmd := exec.Command("git", "count-objects", "-v")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git count-objects: %v", err)
	}

	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "size": // loose object size in KB
			v, _ := strconv.ParseInt(val, 10, 64)
			looseKB = v
		case "size-pack": // pack size in KB
			v, _ := strconv.ParseInt(val, 10, 64)
			packKB = v
		}
	}

	// Total .git directory size via du.
	duCmd := exec.Command("du", "-sk", filepath.Join(dir, ".git"))
	duOut, duErr := duCmd.Output()
	if duErr == nil {
		parts := strings.Fields(string(duOut))
		if len(parts) > 0 {
			v, _ := strconv.ParseInt(parts[0], 10, 64)
			totalKB = v
		}
	}

	return looseKB, packKB, totalKB
}

// timePush times a regular (fast-forward) git push and returns the wall-clock duration.
func timePush(t *testing.T, dir, remote, branch string) time.Duration {
	t.Helper()
	return doTimedPush(t, dir, "git", "push", remote, "refs/heads/"+branch)
}

// timeForcePush times a force push and returns the wall-clock duration.
// Used when each subtest has independent history unrelated to what's already on the remote.
func timeForcePush(t *testing.T, dir, remote, branch string) time.Duration {
	t.Helper()
	return doTimedPush(t, dir, "git", "push", "--force", remote, "refs/heads/"+branch)
}

// doTimedPush runs a git push command and returns its wall-clock duration.
func doTimedPush(t *testing.T, dir string, args ...string) time.Duration {
	t.Helper()

	var stderr bytes.Buffer
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

	if err != nil {
		t.Fatalf("git push failed (%s): %v\n%s", dur, err, stderr.String())
	}

	return dur
}

// --- Push profiling ---

// pushPhase represents a timed phase within a git push operation.
type pushPhase struct {
	name     string
	duration time.Duration
}

// pushProfileResult holds the detailed timing breakdown of a git push.
type pushProfileResult struct {
	total          time.Duration
	phases         []pushPhase
	progressOutput string
}

// trace2Event represents a single GIT_TRACE2_EVENT JSON line.
type trace2Event struct {
	Event      string   `json:"event"`
	SID        string   `json:"sid"`
	Category   string   `json:"category"`
	Label      string   `json:"label"`
	TRel       float64  `json:"t_rel"`
	TAbs       float64  `json:"t_abs"`
	Nesting    int      `json:"nesting"`
	Argv       []string `json:"argv"`
	ChildID    int      `json:"child_id"`
	ChildClass string   `json:"child_class"`
	Code       int      `json:"code"`
}

// TestPushProfile runs a small number of scenarios with detailed push timing
// breakdown using GIT_TRACE2_EVENT to identify where time is spent.
//
// Run with: go test -v -run TestPushProfile -tags pushperf -timeout 10m ./cmd/entire/cli/checkpoint/
func TestPushProfile(t *testing.T) {
	fullName := perfTestRepoOwner + "/" + perfTestRepoName
	src := newTranscriptSource(t)
	repoURL := createTempGitHubRepo(t, fullName)
	t.Cleanup(func() { deleteTempGitHubRepo(t, fullName) })

	type scenario struct {
		name           string
		checkpoints    int
		transcriptSize int
	}

	firstPush := []scenario{
		{"10cp_100KB", 10, 100 * 1024},
		{"50cp_500KB", 50, 500 * 1024},
		{"50cp_2500KB", 50, 2500 * 1024},
		{"100cp_2500KB", 100, 2500 * 1024},
		{"200cp_2500KB", 200, 2500 * 1024},
		{"50cp_10MB", 50, 10 * 1024 * 1024},
		{"10cp_50MB", 10, 50 * 1024 * 1024},
	}

	incremental := []scenario{
		{"50cp_base_500KB", 50, 500 * 1024},
		{"50cp_base_2500KB", 50, 2500 * 1024},
		{"200cp_base_2500KB", 200, 2500 * 1024},
		{"50cp_base_10MB", 50, 10 * 1024 * 1024},
	}

	var results []profileResult

	for _, sc := range firstPush {
		t.Run("FirstPush/"+sc.name, func(t *testing.T) {
			dir := t.TempDir()
			repo := initLocalRepo(t, dir)
			store := NewGitStore(repo)

			seedDur, totalBytes := seedCheckpoints(t, store, src, sc.checkpoints, sc.transcriptSize)
			gcDur := timeGitGC(t, dir)
			_, pk, _ := measureRepoSize(t, dir)
			addRemote(t, dir, repoURL)

			profile := profiledForcePush(t, dir, "origin", "entire/checkpoints/v1")

			t.Logf("  Seed: %s, GC: %s", seedDur.Round(time.Millisecond), gcDur.Round(time.Millisecond))
			logPushProfile(t, profile)

			r := buildResult("FirstPush/"+sc.name, float64(totalBytes)/(1024*1024), pk, profile)
			results = append(results, r)
		})
	}

	for _, sc := range incremental {
		t.Run("Incremental/"+sc.name, func(t *testing.T) {
			dir := t.TempDir()
			repo := initLocalRepo(t, dir)
			store := NewGitStore(repo)

			// Seed and push baseline.
			_, _ = seedCheckpoints(t, store, src, sc.checkpoints, sc.transcriptSize)
			timeGitGC(t, dir)
			addRemote(t, dir, repoURL)
			timeForcePush(t, dir, "origin", "entire/checkpoints/v1")

			// Reopen repo after GC.
			repo, err := gogit.PlainOpen(dir)
			if err != nil {
				t.Fatalf("PlainOpen after gc: %v", err)
			}
			store = NewGitStore(repo)

			// Add 1 more checkpoint and profile the incremental push.
			_, incrBytes := seedCheckpoints(t, store, src, 1, sc.transcriptSize)
			timeGitGC(t, dir)
			_, pk, _ := measureRepoSize(t, dir)

			profile := profiledPush(t, dir, "origin", "entire/checkpoints/v1")

			logPushProfile(t, profile)

			r := buildResult("Incremental/"+sc.name, float64(incrBytes)/(1024*1024), pk, profile)
			results = append(results, r)
		})
	}

	// Print final summary table.
	t.Logf("\n\n========== PUSH PROFILE SUMMARY TABLE ==========")
	t.Logf("%-30s | %8s | %8s | %10s | %10s | %10s | %10s | %10s",
		"Scenario", "Raw MB", "Pack KB", "Negotiate", "Pack+Send", "Remote", "Overhead", "TOTAL")
	t.Logf("%s", strings.Repeat("-", 120))
	for _, r := range results {
		t.Logf("%-30s | %8.1f | %8d | %10s | %10s | %10s | %10s | %10s",
			r.scenario, r.rawMB, r.packKB,
			r.negotiate.Round(time.Millisecond),
			r.packSend.Round(time.Millisecond),
			r.remoteProc.Round(time.Millisecond),
			r.overhead.Round(time.Millisecond),
			r.totalPush.Round(time.Millisecond))
	}
	t.Logf("%s", strings.Repeat("-", 120))
}

// profileResult holds aggregated push phase timings for a single scenario.
type profileResult struct {
	scenario   string
	rawMB      float64
	packKB     int64
	negotiate  time.Duration
	packSend   time.Duration
	remoteProc time.Duration
	overhead   time.Duration
	totalPush  time.Duration
}

// buildResult extracts the key phase durations from a push profile.
func buildResult(name string, rawMB float64, packKB int64, profile pushProfileResult) profileResult {
	var negotiate, packSend, remoteProc time.Duration
	for _, p := range profile.phases {
		n := strings.TrimSpace(p.name)
		switch n {
		case "transport_push/get_refs_list":
			negotiate = p.duration
		case "send_pack/pack_objects":
			packSend = p.duration
		case "send_pack/receive_status":
			remoteProc = p.duration
		}
	}
	overhead := profile.total - negotiate - packSend - remoteProc
	if overhead < 0 {
		overhead = 0
	}
	return profileResult{
		scenario:   name,
		rawMB:      rawMB,
		packKB:     packKB,
		negotiate:  negotiate,
		packSend:   packSend,
		remoteProc: remoteProc,
		overhead:   overhead,
		totalPush:  profile.total,
	}
}

// profiledForcePush runs a force push with GIT_TRACE2_EVENT tracing enabled.
func profiledForcePush(t *testing.T, dir, remote, branch string) pushProfileResult {
	t.Helper()
	return doProfiledPush(t, dir, "git", "push", "--force", "--progress", remote, "refs/heads/"+branch)
}

// doProfiledPush runs a git push with GIT_TRACE2_EVENT tracing and returns the profile.
func doProfiledPush(t *testing.T, dir string, args ...string) pushProfileResult {
	t.Helper()

	traceDir := t.TempDir()
	traceFile := filepath.Join(traceDir, "trace2.jsonl")

	var stderr bytes.Buffer
	//nolint:gosec // test-only, args are test constants
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(),
		"GIT_TRACE2_EVENT="+traceFile,
	)

	start := time.Now()
	err := cmd.Run()
	total := time.Since(start)

	if err != nil {
		t.Fatalf("profiled push failed (%s): %v\n%s", total, err, stderr.String())
	}

	traceData, err := os.ReadFile(traceFile)
	if err != nil {
		t.Logf("warning: could not read trace file: %v", err)
		return pushProfileResult{total: total, progressOutput: stderr.String()}
	}

	phases := parseTrace2Phases(t, traceData)

	return pushProfileResult{
		total:          total,
		phases:         phases,
		progressOutput: stderr.String(),
	}
}

// profiledPush runs a regular (non-force) push with GIT_TRACE2_EVENT tracing.
func profiledPush(t *testing.T, dir, remote, branch string) pushProfileResult {
	t.Helper()
	return doProfiledPush(t, dir, "git", "push", "--progress", remote, "refs/heads/"+branch)
}

// parseTrace2Phases extracts timing phases from GIT_TRACE2_EVENT JSON lines.
// Returns two slices: a clean summary of the main push phases and a detailed
// breakdown of all significant trace events.
func parseTrace2Phases(t *testing.T, data []byte) []pushPhase {
	t.Helper()

	var phases []pushPhase
	childArgv := make(map[int][]string)

	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var ev trace2Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}

		switch ev.Event {
		case "child_start":
			childArgv[ev.ChildID] = ev.Argv

		case "child_exit":
			if ev.TRel < 0.001 {
				continue
			}
			name := childLabel(childArgv[ev.ChildID])
			dur := time.Duration(ev.TRel * float64(time.Second))
			phases = append(phases, pushPhase{name: name, duration: dur})

		case "region_leave":
			if ev.TRel < 0.001 {
				continue
			}
			name := ev.Category
			if ev.Label != "" {
				name += "/" + ev.Label
			}
			indent := strings.Repeat("  ", ev.Nesting)
			dur := time.Duration(ev.TRel * float64(time.Second))
			phases = append(phases, pushPhase{name: indent + name, duration: dur})
		}
	}

	return phases
}

// logPushProfile logs a clean summary of push phases followed by the detailed trace.
func logPushProfile(t *testing.T, profile pushProfileResult) {
	t.Helper()

	// Extract key durations from the detailed phases.
	var negotiate, packObjects, receiveStatus time.Duration
	for _, p := range profile.phases {
		name := strings.TrimSpace(p.name)
		switch name {
		case "transport_push/get_refs_list":
			negotiate = p.duration
		case "send_pack/pack_objects":
			packObjects = p.duration
		case "send_pack/receive_status":
			receiveStatus = p.duration
		}
	}

	overhead := profile.total - negotiate - packObjects - receiveStatus
	if overhead < 0 {
		overhead = 0
	}

	t.Logf("  --- Push Summary ---")
	t.Logf("  Total push wall time: %s", profile.total.Round(time.Millisecond))
	t.Logf("")
	logPhase := func(label string, d time.Duration) {
		pct := float64(d) / float64(profile.total) * 100
		t.Logf("    %-30s %10s  (%5.1f%%)", label, d.Round(time.Millisecond), pct)
	}
	logPhase("Ref negotiation (HTTPS)", negotiate)
	logPhase("Pack + send objects", packObjects)
	logPhase("Remote processing", receiveStatus)
	logPhase("Overhead", overhead)

	t.Logf("")
	t.Logf("  --- Detailed Trace ---")
	for _, p := range profile.phases {
		pct := float64(p.duration) / float64(profile.total) * 100
		t.Logf("    %-40s %10s  (%5.1f%%)", p.name, p.duration.Round(time.Millisecond), pct)
	}
}

// childLabel produces a human-readable label from a child process argv.
func childLabel(argv []string) string {
	if len(argv) == 0 {
		return "child: unknown"
	}
	base := filepath.Base(argv[0])
	// Identify well-known git subprocesses.
	switch {
	case strings.Contains(base, "credential"):
		return "child: credential-helper"
	case strings.Contains(base, "remote-https"):
		return "child: git-remote-https"
	case base == "git" && len(argv) > 1:
		return "child: git " + argv[1]
	default:
		// Use first 2 non-flag args.
		var parts []string
		for _, arg := range argv {
			if strings.HasPrefix(arg, "-") {
				break
			}
			parts = append(parts, filepath.Base(arg))
		}
		if len(parts) > 0 {
			return "child: " + strings.Join(parts, " ")
		}
		return "child: " + base
	}
}
