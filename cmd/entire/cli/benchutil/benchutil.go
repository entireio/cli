// Package benchutil provides test fixture helpers for CLI benchmarks.
//
// It creates realistic git repositories, transcripts, session states,
// and checkpoint data for benchmarking the hot paths (SaveStep, PostCommit/Condense).
package benchutil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// BenchRepo is a fully initialized git repository with Entire configured,
// ready for checkpoint benchmarks.
type BenchRepo struct {
	// Dir is the absolute path to the repository root.
	Dir string

	// Repo is the go-git repository handle.
	Repo *git.Repository

	// Store is the checkpoint GitStore for this repo.
	Store *checkpoint.GitStore

	// HeadHash is the current HEAD commit hash string.
	HeadHash string

	// WorktreeID is the worktree identifier (empty for main worktree).
	WorktreeID string

	// Strategy is the strategy name used in .entire/settings.json.
	Strategy string
}

// RepoOpts configures how NewBenchRepo creates the test repository.
type RepoOpts struct {
	// FileCount is the number of tracked files to create in the initial commit.
	// Each file is ~100 lines of Go code. Defaults to 10.
	FileCount int

	// FileSizeLines is the number of lines per file. Defaults to 100.
	FileSizeLines int

	// CommitCount is the number of commits to create. Defaults to 1.
	CommitCount int

	// Strategy is the strategy name for .entire/settings.json.
	// Defaults to "manual-commit".
	Strategy string

	// FeatureBranch, if non-empty, creates and checks out this branch
	// after the initial commits.
	FeatureBranch string
}

func (o *RepoOpts) withDefaults() RepoOpts {
	out := *o
	if out.FileCount == 0 {
		out.FileCount = 10
	}
	if out.FileSizeLines == 0 {
		out.FileSizeLines = 100
	}
	if out.CommitCount == 0 {
		out.CommitCount = 1
	}
	if out.Strategy == "" {
		out.Strategy = "manual-commit"
	}
	return out
}

// NewBenchRepo creates an isolated git repository for benchmarks.
// The repo has an initial commit with the configured number of files,
// a .gitignore excluding .entire/, and Entire settings initialized.
//
// Uses b.TempDir() so cleanup is automatic.
func NewBenchRepo(b *testing.B, opts RepoOpts) *BenchRepo {
	b.Helper()
	opts = opts.withDefaults()

	dir := b.TempDir()
	// Resolve symlinks (macOS /var -> /private/var)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	// Init repo
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		b.Fatalf("git init: %v", err)
	}

	// Create .gitignore and .entire settings
	writeFile(b, dir, ".gitignore", ".entire/\n")
	initEntireSettings(b, dir, opts.Strategy)

	// Generate initial files
	wt, err := repo.Worktree()
	if err != nil {
		b.Fatalf("worktree: %v", err)
	}

	for i := range opts.FileCount {
		name := fmt.Sprintf("src/file_%03d.go", i)
		content := GenerateGoFile(i, opts.FileSizeLines)
		writeFile(b, dir, name, content)
		if _, err := wt.Add(name); err != nil {
			b.Fatalf("add %s: %v", name, err)
		}
	}
	if _, err := wt.Add(".gitignore"); err != nil {
		b.Fatalf("add .gitignore: %v", err)
	}

	// Create commits
	var headHash plumbing.Hash
	for c := range opts.CommitCount {
		if c > 0 {
			// Modify a file for subsequent commits
			name := fmt.Sprintf("src/file_%03d.go", c%opts.FileCount)
			content := GenerateGoFile(c*1000, opts.FileSizeLines)
			writeFile(b, dir, name, content)
			if _, err := wt.Add(name); err != nil {
				b.Fatalf("add %s: %v", name, err)
			}
		}
		headHash, err = wt.Commit(fmt.Sprintf("Commit %d", c+1), &git.CommitOptions{
			Author: &object.Signature{
				Name:  "Bench User",
				Email: "bench@example.com",
				When:  time.Now(),
			},
		})
		if err != nil {
			b.Fatalf("commit %d: %v", c+1, err)
		}
	}

	// Optionally create feature branch
	if opts.FeatureBranch != "" {
		ref := plumbing.NewHashReference(
			plumbing.NewBranchReferenceName(opts.FeatureBranch), headHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			b.Fatalf("create branch: %v", err)
		}
		// Checkout via git CLI (go-git v5 checkout bug)
		checkoutBranch(b, dir, opts.FeatureBranch)
	}

	br := &BenchRepo{
		Dir:      dir,
		Repo:     repo,
		Store:    checkpoint.NewGitStore(repo),
		HeadHash: headHash.String(),
		Strategy: opts.Strategy,
	}

	// Determine worktree ID
	wtID, err := paths.GetWorktreeID(dir)
	if err == nil {
		br.WorktreeID = wtID
	}

	return br
}

// WriteFile creates or overwrites a file relative to the repo root.
func (br *BenchRepo) WriteFile(b *testing.B, relPath, content string) {
	b.Helper()
	writeFile(b, br.Dir, relPath, content)
}

// AddAndCommit stages the given files and creates a commit.
// Returns the new HEAD hash.
func (br *BenchRepo) AddAndCommit(b *testing.B, message string, files ...string) string {
	b.Helper()
	wt, err := br.Repo.Worktree()
	if err != nil {
		b.Fatalf("worktree: %v", err)
	}
	for _, f := range files {
		if _, err := wt.Add(f); err != nil {
			b.Fatalf("add %s: %v", f, err)
		}
	}
	hash, err := wt.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Bench User",
			Email: "bench@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		b.Fatalf("commit: %v", err)
	}
	br.HeadHash = hash.String()
	return hash.String()
}

// SessionOpts configures how CreateSessionState creates a session state file.
type SessionOpts struct {
	// SessionID is the session identifier. Auto-generated if empty.
	SessionID string

	// Phase is the session phase. Defaults to session.PhaseActive.
	Phase session.Phase

	// StepCount is the number of prior checkpoints. Defaults to 0.
	StepCount int

	// FilesTouched is the list of files tracked by this session.
	FilesTouched []string

	// TranscriptPath is the path to the live transcript file.
	TranscriptPath string

	// AgentType is the agent type. Defaults to agent.AgentTypeClaudeCode.
	AgentType types.AgentType
}

// CreateSessionState writes a session state file to .git/entire-sessions/.
// Returns the session ID used.
func (br *BenchRepo) CreateSessionState(b *testing.B, opts SessionOpts) string {
	b.Helper()

	if opts.SessionID == "" {
		cpID, err := id.Generate()
		if err != nil {
			b.Fatalf("generate session ID: %v", err)
		}
		opts.SessionID = fmt.Sprintf("bench-%s", cpID)
	}
	if opts.Phase == "" {
		opts.Phase = session.PhaseActive
	}

	if opts.AgentType == "" {
		opts.AgentType = agent.AgentTypeClaudeCode
	}

	now := time.Now()
	state := &session.State{
		SessionID:      opts.SessionID,
		BaseCommit:     br.HeadHash,
		WorktreePath:   br.Dir,
		WorktreeID:     br.WorktreeID,
		StartedAt:      now,
		Phase:          opts.Phase,
		StepCount:      opts.StepCount,
		FilesTouched:   opts.FilesTouched,
		TranscriptPath: opts.TranscriptPath,
		AgentType:      opts.AgentType,
	}

	// Write to .git/entire-sessions/<session-id>.json
	gitDir := filepath.Join(br.Dir, ".git")
	sessDir := filepath.Join(gitDir, session.SessionStateDirName)
	if err := os.MkdirAll(sessDir, 0o750); err != nil {
		b.Fatalf("mkdir sessions: %v", err)
	}

	data, err := jsonutil.MarshalIndentWithNewline(state, "", "  ")
	if err != nil {
		b.Fatalf("marshal state: %v", err)
	}

	statePath := filepath.Join(sessDir, opts.SessionID+".json")
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		b.Fatalf("write state: %v", err)
	}

	return opts.SessionID
}

// TranscriptOpts configures how GenerateTranscript creates JSONL data.
type TranscriptOpts struct {
	// MessageCount is the number of JSONL messages to generate.
	MessageCount int

	// AvgMessageBytes is the approximate size of each message's content field.
	// Defaults to 500.
	AvgMessageBytes int

	// IncludeToolUse adds realistic tool_use messages (file edits, bash commands).
	IncludeToolUse bool

	// FilesTouched is the list of files to reference in tool_use messages.
	// Only used when IncludeToolUse is true.
	FilesTouched []string
}

// GenerateTranscript creates realistic Claude Code JSONL transcript data.
// Returns the raw bytes suitable for writing to full.jsonl.
func GenerateTranscript(opts TranscriptOpts) []byte {
	if opts.AvgMessageBytes == 0 {
		opts.AvgMessageBytes = 500
	}

	var buf strings.Builder
	for i := range opts.MessageCount {
		msg := generateTranscriptMessage(i, opts)
		data, err := json.Marshal(msg)
		if err != nil {
			// Should never happen with map[string]any, but satisfy errcheck
			continue
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	return []byte(buf.String())
}

// WriteTranscriptFile writes transcript data to a file and returns the path.
func (br *BenchRepo) WriteTranscriptFile(b *testing.B, sessionID string, data []byte) string {
	b.Helper()
	// Write to .entire/metadata/<session-id>/full.jsonl (matching real layout)
	relDir := filepath.Join(".entire", "metadata", sessionID)
	relPath := filepath.Join(relDir, "full.jsonl")
	absDir := filepath.Join(br.Dir, relDir)
	if err := os.MkdirAll(absDir, 0o750); err != nil {
		b.Fatalf("mkdir transcript dir: %v", err)
	}
	absPath := filepath.Join(br.Dir, relPath)
	if err := os.WriteFile(absPath, data, 0o600); err != nil {
		b.Fatalf("write transcript: %v", err)
	}
	return absPath
}

// SeedShadowBranch creates N checkpoint commits on the shadow branch
// for the current HEAD. This simulates a session that already has
// prior checkpoints saved.
//
// Temporarily changes cwd to br.Dir because WriteTemporary uses
// paths.WorktreeRoot() which depends on os.Getwd().
func (br *BenchRepo) SeedShadowBranch(b *testing.B, sessionID string, checkpointCount int, filesPerCheckpoint int) {
	b.Helper()

	// WriteTemporary internally calls paths.WorktreeRoot() which uses os.Getwd().
	// Switch cwd so it resolves to the bench repo.
	b.Chdir(br.Dir)
	paths.ClearWorktreeRootCache()

	for i := range checkpointCount {
		var modified []string
		for j := range filesPerCheckpoint {
			name := fmt.Sprintf("src/file_%03d.go", j)
			content := GenerateGoFile(i*1000+j, 100)
			writeFile(b, br.Dir, name, content)
			modified = append(modified, name)
		}

		metadataDir := paths.SessionMetadataDirFromSessionID(sessionID)
		metadataDirAbs := filepath.Join(br.Dir, metadataDir)
		if err := os.MkdirAll(metadataDirAbs, 0o750); err != nil {
			b.Fatalf("mkdir metadata: %v", err)
		}

		// Write a minimal transcript to the metadata dir
		transcriptPath := filepath.Join(metadataDirAbs, "full.jsonl")
		transcript := GenerateTranscript(TranscriptOpts{MessageCount: 5, AvgMessageBytes: 200})
		if err := os.WriteFile(transcriptPath, transcript, 0o600); err != nil {
			b.Fatalf("write transcript: %v", err)
		}

		_, err := br.Store.WriteTemporary(context.Background(), checkpoint.WriteTemporaryOptions{
			SessionID:         sessionID,
			BaseCommit:        br.HeadHash,
			WorktreeID:        br.WorktreeID,
			ModifiedFiles:     modified,
			MetadataDir:       metadataDir,
			MetadataDirAbs:    metadataDirAbs,
			CommitMessage:     fmt.Sprintf("Checkpoint %d", i+1),
			AuthorName:        "Bench User",
			AuthorEmail:       "bench@example.com",
			IsFirstCheckpoint: i == 0,
		})
		if err != nil {
			b.Fatalf("write temporary checkpoint %d: %v", i+1, err)
		}
	}
}

// SeedMetadataBranch creates N committed checkpoints on the entire/checkpoints/v1
// branch. This simulates a repository with prior checkpoint history.
func (br *BenchRepo) SeedMetadataBranch(b *testing.B, checkpointCount int) {
	b.Helper()

	for i := range checkpointCount {
		cpID, err := id.Generate()
		if err != nil {
			b.Fatalf("generate checkpoint ID: %v", err)
		}
		sessionID := fmt.Sprintf("seed-session-%04d", i)
		transcript := GenerateTranscript(TranscriptOpts{
			MessageCount:    20,
			AvgMessageBytes: 300,
		})

		files := make([]string, 0, 5)
		for j := range 5 {
			files = append(files, fmt.Sprintf("src/file_%03d.go", (i*5+j)%100))
		}

		err = br.Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
			CheckpointID:     cpID,
			SessionID:        sessionID,
			Strategy:         br.Strategy,
			Transcript:       transcript,
			Prompts:          []string{fmt.Sprintf("Implement feature %d", i)},
			FilesTouched:     files,
			CheckpointsCount: 3,
			AuthorName:       "Bench User",
			AuthorEmail:      "bench@example.com",
			Agent:            agent.AgentTypeClaudeCode,
		})
		if err != nil {
			b.Fatalf("write committed checkpoint %d: %v", i+1, err)
		}
	}
}

// GenerateGoFile creates a synthetic Go source file with the given number of lines.
// The seed value ensures unique content for each file.
func GenerateGoFile(seed, lines int) string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "package pkg%d\n\n", seed%100)

	lineNum := 2
	funcNum := 0
	for lineNum < lines {
		funcName := fmt.Sprintf("func%d_%d", seed, funcNum)
		fmt.Fprintf(&buf, "func %s(ctx context.Context, input string) (string, error) {\n", funcName)
		lineNum++

		bodyLines := min(8, lines-lineNum-1)
		for j := range bodyLines {
			fmt.Fprintf(&buf, "\tv%d := fmt.Sprintf(\"processing %%s step %d seed %d\", input)\n", j, j, seed)
			lineNum++
		}
		buf.WriteString("\treturn \"\", nil\n}\n\n")
		lineNum += 2
		funcNum++
	}
	return buf.String()
}

// GenerateFileContent creates generic file content of approximately the given byte size.
func GenerateFileContent(seed, sizeBytes int) string {
	var buf strings.Builder
	line := fmt.Sprintf("// Line content seed=%d ", seed)
	padding := strings.Repeat("x", max(1, 80-len(line)))
	fullLine := line + padding + "\n"

	for buf.Len() < sizeBytes {
		buf.WriteString(fullLine)
	}
	return buf.String()
}

//nolint:gosec // G301/G306: benchmark fixtures use standard permissions in temp dirs
func writeFile(b *testing.B, dir, relPath, content string) {
	b.Helper()
	abs := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		b.Fatalf("mkdir %s: %v", filepath.Dir(relPath), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		b.Fatalf("write %s: %v", relPath, err)
	}
}

//nolint:gosec // G301/G306: benchmark fixtures use standard permissions in temp dirs
func initEntireSettings(b *testing.B, dir, strategy string) {
	b.Helper()
	entireDir := filepath.Join(dir, ".entire")
	if err := os.MkdirAll(filepath.Join(entireDir, "tmp"), 0o755); err != nil {
		b.Fatalf("mkdir .entire: %v", err)
	}

	settings := map[string]any{
		"strategy":  strategy,
		"local_dev": true,
	}
	data, err := jsonutil.MarshalIndentWithNewline(settings, "", "  ")
	if err != nil {
		b.Fatalf("marshal settings: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, paths.SettingsFileName), data, 0o644); err != nil {
		b.Fatalf("write settings: %v", err)
	}
}

func checkoutBranch(b *testing.B, dir, branch string) {
	b.Helper()
	c := exec.CommandContext(context.Background(), "git", "checkout", branch)
	c.Dir = dir
	if output, err := c.CombinedOutput(); err != nil {
		b.Fatalf("git checkout %s: %v\n%s", branch, err, output)
	}
}

// generateTranscriptMessage creates a single JSONL message for a Claude Code transcript.
func generateTranscriptMessage(index int, opts TranscriptOpts) map[string]any {
	msg := map[string]any{
		"uuid":        fmt.Sprintf("msg_%06d", index),
		"timestamp":   time.Now().Add(time.Duration(index) * time.Second).Format(time.RFC3339),
		"parent_uuid": fmt.Sprintf("msg_%06d", max(0, index-1)),
	}

	switch {
	case opts.IncludeToolUse && index%3 == 2 && len(opts.FilesTouched) > 0:
		// Tool use message (every 3rd message)
		file := opts.FilesTouched[index%len(opts.FilesTouched)]
		msg["type"] = "tool_use"
		msg["tool_name"] = "write_to_file"
		msg["tool_input"] = map[string]any{
			"path":    file,
			"content": GenerateFileContent(index, opts.AvgMessageBytes/2),
		}
	case index%2 == 0:
		// Assistant message
		msg["type"] = "assistant"
		msg["content"] = generatePadding("I'll help you implement this feature. ", opts.AvgMessageBytes)
	default:
		// Human message
		msg["type"] = "human"
		msg["content"] = generatePadding("Please update the implementation. ", opts.AvgMessageBytes/3)
	}

	return msg
}

// SeedBranches creates N branches pointing at the current HEAD.
// The branches are named with the given prefix (e.g., "feature/bench-" → "feature/bench-000").
// This simulates a repo with many refs, which affects go-git ref scanning performance.
func (br *BenchRepo) SeedBranches(b *testing.B, prefix string, count int) {
	b.Helper()
	headHash := plumbing.NewHash(br.HeadHash)
	for i := range count {
		name := fmt.Sprintf("%s%03d", prefix, i)
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), headHash)
		if err := br.Repo.Storer.SetReference(ref); err != nil {
			b.Fatalf("create branch %s: %v", name, err)
		}
	}
}

// PackRefs runs `git pack-refs --all` to simulate a real repo where most refs
// are in the packed-refs file. Large repos almost always have packed refs.
func (br *BenchRepo) PackRefs(b *testing.B) {
	b.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "pack-refs", "--all")
	cmd.Dir = br.Dir
	if output, err := cmd.CombinedOutput(); err != nil {
		b.Fatalf("git pack-refs: %v\n%s", err, output)
	}
}

// SeedGitObjects creates loose git objects to bloat .git/objects/.
// Each call creates N blob objects via `git hash-object -w`.
// After seeding, runs `git gc` to pack them into a packfile (realistic).
func (br *BenchRepo) SeedGitObjects(b *testing.B, count int) {
	b.Helper()

	for i := range count {
		content := GenerateFileContent(i, 4096)
		cmd := exec.CommandContext(context.Background(), "git", "hash-object", "-w", "--stdin")
		cmd.Dir = br.Dir
		cmd.Stdin = strings.NewReader(content)
		if output, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("git hash-object %d: %v\n%s", i, err, output)
		}
	}

	// Pack into a packfile like a real repo
	gc := exec.CommandContext(context.Background(), "git", "gc", "--quiet")
	gc.Dir = br.Dir
	if output, err := gc.CombinedOutput(); err != nil {
		b.Fatalf("git gc: %v\n%s", err, output)
	}
}

func generatePadding(prefix string, targetBytes int) string {
	if len(prefix) >= targetBytes {
		return prefix[:targetBytes]
	}
	padding := strings.Repeat("Lorem ipsum dolor sit amet. ", (targetBytes-len(prefix))/28+1)
	result := prefix + padding
	if len(result) > targetBytes {
		return result[:targetBytes]
	}
	return result
}
