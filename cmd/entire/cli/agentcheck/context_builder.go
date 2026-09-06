package agentcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitops"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const defaultGraphDetailBytes = 64 * 1024

// Reader is the Entire checkpoint read surface AgentCheck needs.
type Reader interface {
	checkpoint.CheckpointReader
	checkpoint.SessionReader
}

// GraphProvider contributes optional graph evidence.
type GraphProvider interface {
	CollectGraphEvidence(ctx context.Context, req GraphRequest) (GraphContext, error)
}

// GraphRequest describes the changed files AgentCheck wants structural context for.
type GraphRequest struct {
	CheckpointID id.CheckpointID
	ChangedFiles []FileChange
	FilesTouched []string
}

// RepositoryBuildOptions configures production context construction.
type RepositoryBuildOptions struct {
	RepoRoot string
	Graph    GraphProvider
}

// Builder creates AgentCheck contexts from Entire checkpoint APIs.
type Builder struct {
	Reader   Reader
	Repo     *git.Repository
	RepoRoot string
	Graph    GraphProvider
}

// BuildFromRepository opens the real Entire checkpoint facade for repoRoot and
// adapts the checkpoint into AgentCheck's context boundary.
func BuildFromRepository(ctx context.Context, checkpointID id.CheckpointID, opts RepositoryBuildOptions) (*Context, error) {
	repoRoot := opts.RepoRoot
	if repoRoot == "" {
		repoRoot = "."
	}
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("agentcheck context: resolve repository root: %w", err)
	}
	absRoot = filepath.Clean(absRoot)

	repo, err := gitrepo.OpenPath(absRoot)
	if err != nil {
		return nil, fmt.Errorf("agentcheck context: open repository: %w", err)
	}
	defer repo.Close()

	repoCtx := settings.WithWorktreeRoot(ctx, absRoot)
	stores, err := checkpoint.Open(repoCtx, repo, checkpoint.OpenOptions{
		ReadRemotes: strategy.CheckpointReadRemotes(repoCtx),
	})
	if err != nil {
		return nil, fmt.Errorf("agentcheck context: open checkpoint store: %w", err)
	}

	return Builder{
		Reader:   stores.Persistent,
		Repo:     repo,
		RepoRoot: absRoot,
		Graph:    opts.Graph,
	}.Build(repoCtx, checkpointID)
}

// Build reads one checkpoint and adapts it into AgentCheck's context contract.
func (b Builder) Build(ctx context.Context, checkpointID id.CheckpointID) (*Context, error) {
	if checkpointID.IsEmpty() {
		return nil, errors.New("agentcheck context: checkpoint ID is required")
	}
	if b.Reader == nil {
		return nil, errors.New("agentcheck context: checkpoint reader is required")
	}

	summary, err := checkpoint.ReadCheckpoint(ctx, b.Reader, checkpointID)
	if err != nil {
		return nil, fmt.Errorf("agentcheck context: read checkpoint %s: %w", checkpointID, err)
	}

	ac := &Context{
		CheckpointID: checkpointID,
		Checkpoint: Checkpoint{
			ID:               summary.CheckpointID,
			Strategy:         summary.Strategy,
			Branch:           summary.Branch,
			CommitSHA:        summary.CommitSHA,
			CheckpointsCount: summary.CheckpointsCount,
			SessionCount:     len(summary.Sessions),
			Imported:         summary.Imported,
			HasReview:        summary.HasReview,
			HasInvestigation: summary.HasInvestigation,
		},
		FilesTouched: appendSortedUnique(nil, summary.FilesTouched...),
		TokenUsage:   summary.TokenUsage,
		Graph:        GraphContext{Available: false, UnavailableReason: "graph provider not configured"},
		Provenance: Provenance{
			CheckpointID: checkpointID,
			Sources: []EvidenceSource{
				{Kind: "checkpoint", ID: checkpointID.String(), Description: "Entire checkpoint metadata", Available: true},
			},
		},
	}

	if err := b.addSessions(ctx, ac, checkpointID, summary); err != nil {
		return nil, err
	}

	gitEvidence := b.collectGitEvidence(ctx, checkpointID, summary)
	ac.Git = gitEvidence
	ac.ChangedFiles = gitEvidence.ChangedFiles
	ac.Provenance.CommitSHAs = commitSHAs(gitEvidence.AssociatedCommits)
	ac.Provenance.Sources = append(ac.Provenance.Sources,
		EvidenceSource{Kind: "git", ID: strings.Join(ac.Provenance.CommitSHAs, ","), Description: "Commits and diff associated through Entire-Checkpoint trailers or checkpoint commit metadata", Available: len(gitEvidence.AssociatedCommits) > 0},
	)

	graph := GraphContext{Available: false, UnavailableReason: "graph provider not configured"}
	if b.Graph != nil {
		graph, err = b.Graph.CollectGraphEvidence(ctx, GraphRequest{
			CheckpointID: checkpointID,
			ChangedFiles: ac.ChangedFiles,
			FilesTouched: ac.FilesTouched,
		})
		if err != nil {
			graph = GraphContext{Available: false, UnavailableReason: err.Error()}
		} else if !graph.Available && graph.UnavailableReason == "" {
			graph.UnavailableReason = "graph provider returned no evidence"
		}
	}
	ac.Graph = graph
	ac.Provenance.Sources = append(ac.Provenance.Sources,
		EvidenceSource{Kind: "graph", Description: "Optional Entire Graph structural evidence", Available: graph.Available},
	)

	return ac, nil
}

func (b Builder) addSessions(ctx context.Context, ac *Context, checkpointID id.CheckpointID, summary *checkpoint.CheckpointSummary) error {
	for i := range summary.Sessions {
		content, err := b.Reader.ReadSessionContent(ctx, checkpointID, i)
		if err != nil {
			return fmt.Errorf("agentcheck context: read checkpoint %s session %d: %w", checkpointID, i, err)
		}
		if content == nil {
			return fmt.Errorf("agentcheck context: checkpoint %s session %d returned no content", checkpointID, i)
		}
		meta := content.Metadata
		prompts := checkpoint.SplitPromptContent(content.Prompts)
		session := Session{
			Index:                  i,
			SessionID:              meta.SessionID,
			CreatedAt:              meta.CreatedAt,
			Strategy:               meta.Strategy,
			Branch:                 meta.Branch,
			CommitSHA:              meta.CommitSHA,
			CheckpointsCount:       meta.CheckpointsCount,
			SaveStepCount:          meta.SaveStepCount,
			FilesTouched:           appendSortedUnique(nil, meta.FilesTouched...),
			AgentType:              string(meta.Agent),
			Model:                  meta.Model,
			TurnID:                 meta.TurnID,
			Kind:                   meta.Kind,
			IsTask:                 meta.IsTask,
			ToolUseID:              meta.ToolUseID,
			TokenUsage:             meta.TokenUsage,
			TranscriptStart:        meta.GetTranscriptStart(),
			CompactTranscriptStart: meta.CompactTranscriptStart,
			PromptCount:            len(prompts),
			SkillEventCount:        len(meta.SkillEvents),
		}
		if len(content.Transcript) == 0 {
			session.TranscriptUnavailable = true
			session.TranscriptUnavailableReason = "session transcript empty or unavailable"
		}
		for j, prompt := range prompts {
			p := Prompt{SessionIndex: i, PromptIndex: j, Text: prompt}
			session.Prompts = append(session.Prompts, p)
			ac.ScopedPrompts = append(ac.ScopedPrompts, p)
			if ac.DeveloperPrompt == "" {
				ac.DeveloperPrompt = prompt
			}
		}
		if ac.AgentType == "" {
			ac.AgentType = session.AgentType
		}
		if ac.Model == "" {
			ac.Model = session.Model
		}
		ac.Sessions = append(ac.Sessions, session)
		if session.SessionID != "" {
			ac.Provenance.SessionIDs = append(ac.Provenance.SessionIDs, session.SessionID)
		}
		if session.IsTask || session.ToolUseID != "" {
			ac.TaskRecords = append(ac.TaskRecords, TaskRecord{ToolUseID: session.ToolUseID})
		}
		if len(content.Transcript) > 0 && !ac.Transcript.Available {
			ac.Transcript = TranscriptRef{Available: true, SessionID: session.SessionID, SessionIndex: i, ByteLength: len(content.Transcript)}
		}
	}
	if !ac.Transcript.Available {
		ac.Transcript = TranscriptRef{Available: false, UnavailableReason: "no session transcript bytes available"}
	}
	ac.Provenance.SessionIDs = appendSortedUnique(nil, ac.Provenance.SessionIDs...)
	ac.Provenance.Sources = append(ac.Provenance.Sources,
		EvidenceSource{Kind: "session", ID: strings.Join(ac.Provenance.SessionIDs, ","), Description: "Entire checkpoint session metadata, prompts, and transcript availability", Available: len(ac.Sessions) > 0},
	)
	return nil
}

func (b Builder) collectGitEvidence(ctx context.Context, checkpointID id.CheckpointID, summary *checkpoint.CheckpointSummary) GitEvidence {
	if b.Repo == nil {
		return GitEvidence{DiffUnavailableReason: "git repository not configured", ChangedFilesUnavailable: "git repository not configured"}
	}
	commits, err := findAssociatedCommits(ctx, b.Repo, checkpointID, summary.CommitSHA)
	if err != nil {
		return GitEvidence{DiffUnavailableReason: err.Error(), ChangedFilesUnavailable: err.Error()}
	}
	if len(commits) == 0 {
		return GitEvidence{DiffUnavailableReason: "no associated commits found", ChangedFilesUnavailable: "no associated commits found"}
	}

	evidence := GitEvidence{AssociatedCommits: commits}
	if b.RepoRoot == "" {
		evidence.DiffUnavailableReason = "repository root not configured"
		evidence.ChangedFilesUnavailable = "repository root not configured"
		return evidence
	}
	var diff strings.Builder
	changed := map[string]struct{}{}
	for _, commit := range commits {
		parent := firstParentHash(ctx, b.Repo, commit.SHA)
		files, filesErr := gitops.DiffTreeFileList(ctx, b.RepoRoot, parent, commit.SHA)
		if filesErr != nil && evidence.ChangedFilesUnavailable == "" {
			evidence.ChangedFilesUnavailable = filesErr.Error()
		}
		for _, file := range files {
			changed[file] = struct{}{}
		}
		patch, patchErr := gitShowPatch(ctx, b.RepoRoot, commit.SHA)
		if patchErr != nil {
			if evidence.DiffUnavailableReason == "" {
				evidence.DiffUnavailableReason = patchErr.Error()
			}
			continue
		}
		if diff.Len() > 0 {
			diff.WriteString("\n")
		}
		diff.WriteString(patch)
	}
	evidence.ChangedFiles = fileChangesFromSet(changed)
	evidence.ChangedFilesUnavailable = emptyIfChanged(evidence.ChangedFilesUnavailable, changed)
	evidence.Diff = diff.String()
	if evidence.Diff != "" {
		evidence.DiffUnavailableReason = ""
	}
	return evidence
}

func findAssociatedCommits(ctx context.Context, repo *git.Repository, checkpointID id.CheckpointID, anchorSHA string) ([]AssociatedCommit, error) {
	seen := map[string]bool{}
	var result []AssociatedCommit
	if anchorSHA != "" {
		commit, err := repo.CommitObject(plumbing.NewHash(anchorSHA))
		if err == nil {
			result = append(result, associatedCommit(commit, "checkpoint_metadata"))
			seen[commit.Hash.String()] = true
		}
	}

	head, err := repo.Head()
	if err != nil {
		if len(result) > 0 {
			return result, nil
		}
		return nil, fmt.Errorf("resolve HEAD: %w", err)
	}
	iter, err := repo.Log(&git.LogOptions{From: head.Hash(), Order: git.LogOrderCommitterTime})
	if err != nil {
		if len(result) > 0 {
			return result, nil
		}
		return nil, fmt.Errorf("read git log: %w", err)
	}
	defer iter.Close()
	err = iter.ForEach(func(commit *object.Commit) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("agentcheck context: git log canceled: %w", err)
		}
		for _, cpID := range trailers.ParseAllCheckpoints(commit.Message) {
			if cpID == checkpointID && !seen[commit.Hash.String()] {
				result = append(result, associatedCommit(commit, "commit_trailer"))
				seen[commit.Hash.String()] = true
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate git log: %w", err)
	}
	return result, nil
}

func associatedCommit(commit *object.Commit, source string) AssociatedCommit {
	subject, _, _ := strings.Cut(commit.Message, "\n")
	return AssociatedCommit{
		SHA:     commit.Hash.String(),
		Subject: strings.TrimSpace(subject),
		Author:  commit.Author.Name,
		Date:    commit.Author.When,
		Source:  source,
	}
}

func firstParentHash(ctx context.Context, repo *git.Repository, sha string) string {
	commit, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil || commit.NumParents() == 0 {
		return ""
	}
	parent, err := commit.Parent(0)
	if err != nil {
		return ""
	}
	if err := ctx.Err(); err != nil {
		return ""
	}
	return parent.Hash.String()
}

func gitShowPatch(ctx context.Context, repoRoot, sha string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "show", "--format=commit %H%n%B", "--no-ext-diff", "--find-renames", sha)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git show %s: %w: %s", sha, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func fileChangesFromSet(paths map[string]struct{}) []FileChange {
	out := make([]FileChange, 0, len(paths))
	for path := range paths {
		out = append(out, FileChange{Path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func commitSHAs(commits []AssociatedCommit) []string {
	out := make([]string, 0, len(commits))
	for _, commit := range commits {
		out = append(out, commit.SHA)
	}
	return out
}

func appendSortedUnique(dst []string, values ...string) []string {
	seen := map[string]struct{}{}
	for _, existing := range dst {
		seen[existing] = struct{}{}
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		dst = append(dst, value)
	}
	sort.Strings(dst)
	return dst
}

func emptyIfChanged(reason string, changed map[string]struct{}) string {
	if len(changed) > 0 {
		return ""
	}
	return reason
}

// GraphCLIProvider shells out to the installed Entire Graph CLI. It is optional
// by design: failures are converted to unavailable GraphContext by Builder.
type GraphCLIProvider struct {
	Executable string
	RepoRoot   string
	MaxBytes   int
}

func (p GraphCLIProvider) CollectGraphEvidence(ctx context.Context, req GraphRequest) (GraphContext, error) {
	if req.CheckpointID.IsEmpty() {
		return GraphContext{}, errors.New("graph checkpoint ID is required")
	}
	executable := p.Executable
	if executable == "" {
		executable = "entire"
	}
	repoRoot := p.RepoRoot
	if repoRoot == "" {
		repoRoot = "."
	}

	cmd := exec.CommandContext(ctx, executable, "graph", "checkpoint", req.CheckpointID.String(), "--json", "--repo", repoRoot)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return GraphContext{}, fmt.Errorf("run entire graph checkpoint: %w", ctxErr)
		}
		return GraphContext{}, fmt.Errorf("run entire graph checkpoint: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	detail := stdout.String()
	if strings.TrimSpace(detail) == "" {
		return GraphContext{Available: false, UnavailableReason: "entire graph returned no evidence"}, nil
	}
	maxBytes := p.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultGraphDetailBytes
	}
	if len(detail) > maxBytes {
		detail = detail[:maxBytes] + "\n[agentcheck: graph evidence truncated]"
	}

	return GraphContext{
		Available: true,
		Evidence: []GraphEvidence{{
			Query:  "entire graph checkpoint " + req.CheckpointID.String() + " --json",
			Kind:   "checkpoint",
			Paths:  pathsFromFileChanges(req.ChangedFiles),
			Detail: detail,
		}},
	}, nil
}

func pathsFromFileChanges(files []FileChange) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		if file.Path != "" {
			paths = append(paths, file.Path)
		}
	}
	sort.Strings(paths)
	return paths
}
