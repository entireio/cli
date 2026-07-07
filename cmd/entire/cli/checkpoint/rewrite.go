package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
)

// RewriteResult summarizes a git-branch → git-refs checkpoint rewrite.
type RewriteResult struct {
	// Total is the number of checkpoints found on the v1 branch.
	Total int
	// Rewritten lists the checkpoints re-materialized as refs.
	Rewritten []id.CheckpointID
	// Skipped counts checkpoints left alone because their ref already exists.
	Skipped int
}

// RewriteBranchToRefs re-materializes every checkpoint on the git-branch v1
// branch as a per-checkpoint ref by re-driving the git-refs store's write path.
//
// Unlike a byte-identical copy, this reads each checkpoint's data and writes it
// fresh: for every session it replays the transcript (so the compact
// transcript.jsonl is regenerated), prompts, and metadata at the ref's tree root
// (no shard folders), then replays the per-session summaries and the combined
// attribution. Any tasks/ subtree is grafted in unchanged. The checkpoint id is
// preserved, and each commit keeps the original author — read from the v1-branch
// commit that wrote the session (metadata carries no author), falling back to
// the repo's git author.
//
// Idempotent: a checkpoint whose ref already exists is skipped unless force is
// set (which re-materializes it from scratch). When dryRun is true it reports
// what would be rewritten without writing anything.
func RewriteBranchToRefs(ctx context.Context, repo *git.Repository, dryRun, force bool) (RewriteResult, error) {
	var result RewriteResult

	branch := NewGitStore(repo, DefaultV1Refs())
	tree, err := branch.getSessionsBranchTree()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return result, nil // no v1 branch → nothing to rewrite
		}
		return result, fmt.Errorf("read v1 checkpoint branch: %w", err)
	}

	authors := branchSessionAuthors(ctx, repo)
	fbName, fbEmail := GetGitAuthorFromRepo(repo)
	fallback := commitAuthor{Name: fbName, Email: fbEmail}
	refsStore := newGitRefsStore(repo)

	walkErr := WalkCheckpointShards(ctx, repo, tree, func(cid id.CheckpointID, cpTreeHash plumbing.Hash) error {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // propagate context cancellation
		}
		result.Total++

		refName, err := RefName(cid)
		if err != nil {
			logging.Warn(ctx, "rewrite: skipping checkpoint with unmappable id",
				slog.String("id", cid.String()), slog.String("error", err.Error()))
			return nil
		}
		if _, err := repo.Reference(refName, true); err == nil && !force {
			result.Skipped++
			return nil
		}

		if dryRun {
			result.Rewritten = append(result.Rewritten, cid)
			return nil
		}

		if err := rewriteCheckpoint(ctx, branch, refsStore, repo, cid, refName, cpTreeHash, authors, fallback, force); err != nil {
			return fmt.Errorf("rewrite checkpoint %s: %w", cid, err)
		}
		result.Rewritten = append(result.Rewritten, cid)
		return nil
	})
	if walkErr != nil {
		return result, fmt.Errorf("walk v1 checkpoints: %w", walkErr)
	}
	return result, nil
}

// tasksDirName is the checkpoint subtree that holds subagent task steps.
const tasksDirName = "tasks"

// commitAuthor is a git author identity.
type commitAuthor struct {
	Name  string
	Email string
}

// resolveAuthor returns the recorded author for a session, or the fallback when
// the session has no mapped commit author.
func resolveAuthor(sessionID string, authors map[string]commitAuthor, fallback commitAuthor) commitAuthor {
	if a, ok := authors[sessionID]; ok && a.Name != "" {
		return a
	}
	return fallback
}

// branchSessionAuthors maps each session id to the author of the earliest
// v1-branch commit that wrote it, so a rewrite can preserve the original author.
// The map is best-effort: a session with no resolvable commit falls back to the
// repo author at the call site.
func branchSessionAuthors(ctx context.Context, repo *git.Repository) map[string]commitAuthor {
	authors := make(map[string]commitAuthor)
	ref, err := repo.Reference(DefaultV1Refs().Read, true)
	if err != nil {
		return authors
	}
	iter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return authors
	}
	defer iter.Close()
	// Walk tip → root and overwrite, so the root-most (earliest) commit for a
	// session — its original write — wins over later backfill commits. The map is
	// best-effort: a cancellation mid-walk just yields a partial map, and callers
	// fall back to the repo author for any unmapped session.
	_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // best-effort; partial map is acceptable
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // stop the walk on cancellation
		}
		if sessionID, ok := trailers.ParseSession(c.Message); ok {
			authors[sessionID] = commitAuthor{Name: c.Author.Name, Email: c.Author.Email}
		}
		return nil
	})
	return authors
}

// rewriteCheckpoint re-materializes one checkpoint as a ref by replaying its
// sessions, summaries, attribution, and tasks through the git-refs write path.
func rewriteCheckpoint(
	ctx context.Context,
	branch *GitStore,
	refsStore *gitRefsStore,
	repo *git.Repository,
	cid id.CheckpointID,
	refName plumbing.ReferenceName,
	cpTreeHash plumbing.Hash,
	authors map[string]commitAuthor,
	fallback commitAuthor,
	force bool,
) error {
	summary, err := branch.Read(ctx, cid)
	if err != nil {
		return fmt.Errorf("read checkpoint summary: %w", err)
	}
	if summary == nil {
		return errors.New("checkpoint summary not found on branch")
	}

	// Under --force, drop the existing ref so the replay builds a fresh history
	// rooted at an orphan commit. Without --force the caller already skipped an
	// existing ref, so there's nothing to reset.
	if force {
		if _, err := repo.Reference(refName, true); err == nil {
			if err := repo.Storer.RemoveReference(refName); err != nil {
				return fmt.Errorf("reset existing ref: %w", err)
			}
		}
	}

	// Replay each session in order. Writing session N then its summary keeps
	// SessionSummary (which targets the latest session) pointed at the right one.
	firstAuthor := fallback
	for idx := range summary.Sessions {
		content, err := branch.ReadSessionContent(ctx, cid, idx)
		if err != nil {
			return fmt.Errorf("read session %d: %w", idx, err)
		}
		m := content.Metadata
		author := resolveAuthor(m.SessionID, authors, fallback)
		if idx == 0 {
			firstAuthor = author
		}

		if err := refsStore.Write(ctx, Session(WriteOptions{
			CheckpointID:                cid,
			SessionID:                   m.SessionID,
			CreatedAt:                   m.CreatedAt,
			Strategy:                    m.Strategy,
			Branch:                      m.Branch,
			Transcript:                  redact.AlreadyRedacted(content.Transcript),
			Prompts:                     SplitPromptContent(content.Prompts),
			FilesTouched:                m.FilesTouched,
			CheckpointsCount:            m.CheckpointsCount,
			SaveStepCount:               m.SaveStepCount,
			AuthorName:                  author.Name,
			AuthorEmail:                 author.Email,
			Agent:                       m.Agent,
			Model:                       m.Model,
			TurnID:                      m.TurnID,
			TranscriptIdentifierAtStart: m.TranscriptIdentifierAtStart,
			CheckpointTranscriptStart:   m.CheckpointTranscriptStart,
			TokenUsage:                  m.TokenUsage,
			SkillEvents:                 m.SkillEvents,
			// Forward the remaining session-level fields so a rewritten ref
			// matches the branch checkpoint the tool is meant to evaluate.
			Attribution:            m.Attribution,
			PromptAttributionsJSON: m.PromptAttributions,
			SessionMetrics:         m.SessionMetrics,
			Kind:                   m.Kind,
			ReviewPrompt:           m.ReviewPrompt,
			ReviewSkills:           m.ReviewSkills,
			InvestigateRunID:       m.InvestigateRunID,
			InvestigateTopic:       m.InvestigateTopic,
			HasReview:              summary.HasReview,
			HasInvestigation:       summary.HasInvestigation,
		})); err != nil {
			return fmt.Errorf("write session %d: %w", idx, err)
		}

		if m.Summary != nil {
			if err := refsStore.Write(ctx, SessionSummary{CheckpointID: cid, Summary: m.Summary}); err != nil {
				return fmt.Errorf("write session %d summary: %w", idx, err)
			}
		}
	}

	if summary.CombinedAttribution != nil {
		if err := refsStore.Write(ctx, CheckpointAttribution{CheckpointID: cid, Attribution: summary.CombinedAttribution}); err != nil {
			return fmt.Errorf("write combined attribution: %w", err)
		}
	}

	// Graft the tasks/ subtree unchanged if the checkpoint has one — task steps
	// aren't reconstructable through the write union, so the existing tree is
	// carried over verbatim.
	if tasksHash, ok := subtreeEntryHash(repo, cpTreeHash, tasksDirName); ok {
		if err := graftTasksSubtree(ctx, repo, refsStore, cid, refName, tasksHash, firstAuthor); err != nil {
			return fmt.Errorf("graft tasks: %w", err)
		}
	}
	return nil
}

// subtreeEntryHash returns the hash of a named directory entry in a tree.
func subtreeEntryHash(repo *git.Repository, treeHash plumbing.Hash, name string) (plumbing.Hash, bool) {
	tree, err := repo.TreeObject(treeHash)
	if err != nil {
		return plumbing.ZeroHash, false
	}
	for _, e := range tree.Entries {
		if e.Name == name && e.Mode == filemode.Dir {
			return e.Hash, true
		}
	}
	return plumbing.ZeroHash, false
}

// graftTasksSubtree adds the tasks/ subtree to the checkpoint's current ref tree
// via one commit on top, reusing the existing task objects unchanged. The tasks/
// entry is spliced into the root tree with UpdateSubtree, so every sibling
// session subtree keeps its hash — no whole-tree rebuild.
func graftTasksSubtree(ctx context.Context, repo *git.Repository, refsStore *gitRefsStore, cid id.CheckpointID, refName plumbing.ReferenceName, tasksHash plumbing.Hash, author commitAuthor) error {
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return fmt.Errorf("resolve ref: %w", err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return fmt.Errorf("read ref commit: %w", err)
	}
	newTree, err := UpdateSubtree(repo, commit.TreeHash, nil,
		[]object.TreeEntry{{Name: tasksDirName, Mode: filemode.Dir, Hash: tasksHash}},
		UpdateSubtreeOptions{MergeMode: MergeKeepExisting})
	if err != nil {
		return fmt.Errorf("graft tasks subtree: %w", err)
	}
	if newTree == commit.TreeHash {
		return nil // tasks already present, nothing to graft
	}
	msg := fmt.Sprintf("Graft tasks for checkpoint %s (migrated from git-branch)", cid)
	graftCommit, err := CreateCommit(ctx, repo, newTree, ref.Hash(), msg, author.Name, author.Email)
	if err != nil {
		return fmt.Errorf("commit grafted tasks: %w", err)
	}
	return refsStore.setRef(ctx, cid, graftCommit)
}
