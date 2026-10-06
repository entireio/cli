package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	cliReview "github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/perf"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/spf13/cobra"
)

// attachOptions carries optional flags for runAttach. Review opts the attach
// into recording the session as an agent_review in the checkpoint metadata.
type attachOptions struct {
	// Review, when true, tags the attached session as a review. Skills are
	// resolved inside runAttach after the real agent is known (via session
	// state or transcript auto-detection), not at the cobra layer — the
	// --agent flag's default points at claude-code, which would otherwise
	// make a Codex session incorrectly look up review.claude-code config.
	Review bool
	// ReviewSkillsOverride, when non-empty, declares which review skills were
	// run. Empty is valid: the session is still tagged as a review, with no
	// structured skills list. Ignored when Review=false.
	ReviewSkillsOverride []string
	// ReviewPromptOverride, when non-empty, is recorded instead of the
	// transcript's first user prompt. Set from a pending-review marker when
	// `entire session attach --review` adopts the prompt the user was asked to run.
	ReviewPromptOverride string
	// Commit, when set, names the commit to link instead of HEAD. How the link
	// is written follows from the commit, never from a flag: see
	// attachLinkMode.
	Commit string
}

// committedRefs resolves the committed metadata topology.
func (opts attachOptions) committedRefs(ctx context.Context) cpkg.PersistentRefs {
	return cpkg.ResolveRefs(ctx)
}

// openAttachStore opens the committed store for the resolved topology. refs is
// passed explicitly so attach preserves its pinning: PrimaryAsLocalRead for
// the local-presence gates (a checkpoint present only on a remote-tracking
// ref must read as absent, or attach clobbers the remote on push), plain
// refs with the read chain for ordinary reads (an existing checkpoint's
// summary may live on the elected sync remote rather than origin).
func openAttachStore(ctx context.Context, repo *git.Repository, refs cpkg.PersistentRefs) (cpkg.PersistentStore, error) {
	stores, err := cpkg.Open(ctx, repo, cpkg.OpenOptions{Refs: &refs, ReadRemotes: strategy.CheckpointReadRemotes(ctx)})
	if err != nil {
		return nil, fmt.Errorf("open checkpoint store: %w", err)
	}
	return stores.Persistent, nil
}

func newAttachCmd() *cobra.Command {
	var (
		force      bool
		commitFlag string
		agentFlag  string
		reviewFlag bool
		skillsFlag []string
	)
	cmd := &cobra.Command{
		Use:   "attach <session-id>",
		Short: "Attach an existing agent session",
		Long: `Attach an existing agent session that wasn't captured by hooks.

This creates a checkpoint from the session's transcript and links it to the
last commit, or to the commit named by --commit. Use this when hooks failed
to fire or weren't installed when the session started, or to attach a
research session.

How the session is linked follows from the commit, so there is nothing to
choose and no prompt:
  - The commit already has a checkpoint: the session is added to it.
  - The commit is HEAD and not pushed yet: the Entire-Checkpoint trailer is
    amended in. Nobody else has the commit yet, and a trailer survives a
    later rebase.
  - The commit is already pushed: the link is recorded in the checkpoint and
    the commit is left unchanged, so nothing needs a force-push. It counts
    once the commit's author attaches it.
  - An older commit that isn't pushed is refused: push it first.

Use --review to tag the attached session as an agent review. The
first user prompt in the transcript is recorded as the review prompt.
Pass --skills to declare which skills were actually run; omit to
attach a review without a declared skills list.

Works with any registered agent, including external agents enabled via
external_agents in .entire/settings.local.json (that file only, and only
when it is untracked). Run 'entire agent list' to see the full list.

If --agent doesn't locate a transcript, Entire auto-detects the agent from
the transcript and prints the detected agent name.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return cmd.Help()
			}
			if checkDisabledGuard(cmd.Context(), cmd.OutOrStdout()) {
				return nil
			}
			// Discover external agents so --agent <external-name> is recognized
			// and so auto-detection can find transcripts from external agents.
			external.DiscoverAndRegister(cmd.Context())
			opts := attachOptions{
				Commit:               commitFlag,
				Review:               reviewFlag,
				ReviewSkillsOverride: skillsFlag,
			}
			// When tagging as a review, consume any pending-review marker left
			// by `entire review` for an agent it could not launch itself: adopt
			// its agent / skills / prompt so the manual attach matches what the
			// user was asked to run, then clear it after a successful attach.
			useMarker := false
			if reviewFlag {
				marker, ok, markerErr := matchingPendingReviewMarker(cmd.Context(), agentFlag, cmd.Flags().Changed("agent"))
				if markerErr != nil {
					return markerErr
				}
				useMarker = ok
				if useMarker {
					if !cmd.Flags().Changed("agent") && marker.AgentName != "" {
						agentFlag = marker.AgentName
					}
					if !cmd.Flags().Changed("skills") {
						opts.ReviewSkillsOverride = marker.Skills
					}
					opts.ReviewPromptOverride = marker.Prompt
				}
			}
			err := runAttachSurfaceReviewErrors(cmd, args[0], types.AgentName(agentFlag), opts)
			if err == nil && useMarker {
				if clearErr := cliReview.ClearPendingReviewMarker(cmd.Context()); clearErr != nil {
					logging.Debug(cmd.Context(), "clear pending review marker after attach", slog.String("error", clearErr.Error()))
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&commitFlag, "commit", "", "Link the session to this commit (hash, ref, or HEAD~n) instead of the last one. The link is recorded in the checkpoint, so the commit is never rewritten and no prompt is shown")
	// --force once skipped the amend prompt. attach no longer prompts; the flag
	// is kept, hidden, so existing scripts keep working.
	cmd.Flags().BoolVarP(&force, "force", "f", false, "No longer needed; attach decides how to link without prompting")
	if err := cmd.Flags().MarkHidden("force"); err != nil {
		panic(err)
	}
	cmd.Flags().StringVarP(&agentFlag, "agent", "a", string(agent.DefaultAgentName), "Agent that created the session (see 'entire agent list' for registered agents, including external)")
	cmd.Flags().BoolVar(&reviewFlag, "review", false, "Tag the attached session as an agent review")
	cmd.Flags().StringSliceVar(&skillsFlag, "skills", nil, "Optional: declare which review skills were run in this session. Only used with --review")
	return cmd
}

// resolveReviewSkills returns the skills list to record on an
// attach-as-review. Only the user's --skills flag counts: configured
// settings.Review[agent] is the spawn-path default ("what I'd run if I
// used 'entire review'"), not a claim about what actually happened in a
// given manual session. Silently attaching configured skills would
// misrepresent the session as having run skills it may not have.
//
// Empty is a valid result — the attach still tags the session as a
// review via Kind + ReviewPrompt (the session's first user prompt). The
// skills list is a queryable convenience, not the source of truth.
func resolveReviewSkills(flagSkills []string) []string {
	if len(flagSkills) == 0 {
		return nil
	}
	return flagSkills
}

// runAttachSurfaceReviewErrors wraps runAttach so review-mode errors reach
// the user as clear stderr messages rather than generic cobra error output.
// The non-review path preserves the existing runAttach return-err behavior.
func runAttachSurfaceReviewErrors(cmd *cobra.Command, sessionID string, agentName types.AgentName, opts attachOptions) error {
	err := runAttach(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), sessionID, agentName, opts)
	if err != nil && opts.Review {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return NewSilentError(err)
	}
	return err
}

// attachStepCount returns the displayed "steps" count for an attached session:
// the number of user prompts (turns) in the attached transcript, as counted by
// extractTranscriptMetadata. Floored at 1 so it never renders as "0 steps" for an
// empty/unparseable transcript. SaveStepCount stays 0 (no SaveStep ran), keeping
// the combined-attribution gate conservative for this fallback session.
func attachStepCount(turnCount int) int {
	return max(turnCount, 1)
}

// attachPrompts returns the prompts recorded on an attached checkpoint. Attach
// records only the first user prompt (used for the display title); the full
// per-turn list isn't reconstructed for post-hoc imports.
func attachPrompts(meta transcriptMetadata) []string {
	if meta.FirstPrompt == "" {
		return nil
	}
	return []string{meta.FirstPrompt}
}

func runAttach(ctx context.Context, w, errW io.Writer, sessionID string, agentName types.AgentName, opts attachOptions) error {
	// Restores and looks up agent transcripts from the user's shell, where a
	// home an agent reads from its own settings is invisible to the environment.
	agent.EnableHomeProbes()
	// The logger arrives in ctx from the root PersistentPreRun, and main.go
	// closes it — the only close site, covering every path ExecuteContextC
	// returns from — so attach neither builds nor closes one, the way
	// resume/clean/reset/explain do not either. Only the session is attach's to
	// add, so its lines are filterable.
	ctx = logging.WithSessionID(ctx, sessionID)

	logCtx := logging.WithComponent(ctx, "attach")

	// Open repository once — shared across all operations.
	repo, err := openRepository(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := repo.Close(); closeErr != nil {
			logging.Warn(logCtx, "failed to close repository", slog.String("error", closeErr.Error()))
		}
	}()

	existingState, err := validateAttachPreconditions(ctx, repo, sessionID)
	if err != nil {
		return err
	}

	headCommit, err := getHeadCommit(repo)
	if err != nil {
		return err
	}
	// Decide how to link before writing anything, so a commit that can't be
	// linked is refused with nothing left behind.
	plan, err := planAttachLink(ctx, repo, headCommit, opts)
	if err != nil {
		return err
	}
	target := plan.target

	// If session already has a checkpoint, link that one.
	if existingState != nil && !existingState.LastCheckpointID.IsEmpty() {
		return linkExistingCheckpoint(logCtx, w, errW, sessionID, existingState, plan, opts)
	}

	// Resolve agent and transcript path.
	ag, transcriptPath, err := resolveAgentAndTranscript(logCtx, w, sessionID, agentName, existingState)
	if err != nil {
		return err
	}

	var reviewSkills []string
	if opts.Review {
		reviewSkills = resolveReviewSkills(opts.ReviewSkillsOverride)
	}

	transcriptData, err := ag.ReadTranscript(transcriptPath)
	if err != nil {
		return fmt.Errorf("failed to read transcript: %w", err)
	}

	meta := extractTranscriptMetadataForAgent(ag, transcriptPath, transcriptData)
	warnEmptyTranscriptMetadata(errW, ag.Name(), meta, opts)

	// Determine checkpoint ID: reuse the target commit's if it has one, otherwise generate new.
	checkpointID, isExistingCheckpoint := resolveCheckpointID(ctx, target)

	// If the target commit references an existing checkpoint, make sure we have it locally
	// before writing — otherwise we'd create a fresh session 0 under the same
	// ID and overwrite the original on push.
	refs := opts.committedRefs(ctx)
	refreshedRepo, err := ensureCheckpointAvailable(ctx, logCtx, repo, refs, checkpointID, isExistingCheckpoint)
	if refreshedRepo != nil && refreshedRepo != repo {
		oldRepo := repo
		repo = refreshedRepo
		if closeErr := oldRepo.Close(); closeErr != nil {
			logging.Warn(logCtx, "failed to close stale repository handle after checkpoint refresh",
				slog.String("error", closeErr.Error()))
		}
	}
	if err != nil {
		return err
	}

	store, err := openAttachStore(ctx, repo, refs)
	if err != nil {
		return err
	}

	// Defense-in-depth guard: the earlier existingState.LastCheckpointID
	// check only fires when the session's state file records its
	// checkpoint. A session already stored in the target commit's checkpoint but
	// whose state is missing/stale (state file deleted, never written,
	// condensed without LastCheckpointID update, or pulled from a remote
	// that wasn't reflected locally) would bypass that guard.
	// findSessionIndex matches by SessionID — without this check, a
	// review-attach on such a session silently overwrites the existing
	// session's metadata in the checkpoint.
	if opts.Review && isExistingCheckpoint {
		exists, readErr := checkpointHasSessionMetadata(ctx, repo, refs, checkpointID, sessionID)
		if readErr != nil {
			return fmt.Errorf("failed to check checkpoint %s for session %s: %w", checkpointID.String(), sessionID, readErr)
		}
		if exists {
			return fmt.Errorf(
				"session %s is already recorded in checkpoint %s; rewriting an existing checkpoint as a review is not supported yet",
				sessionID, checkpointID.String(),
			)
		}
	}

	author, err := GetGitAuthor(ctx)
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	tokenUsage := agent.CalculateTokenUsage(logCtx, ag, transcriptData, 0, "")

	// attach writes checkpoints and historically never configured
	// redaction; a scanner-config failure must fail the attach.
	if err := strategy.EnsureRedactionConfigured(ctx); err != nil {
		return fmt.Errorf("configuring redaction: %w", err)
	}

	_, redactSpan := perf.Start(ctx, "redact_transcript")
	redactedTranscript, redactErr := redact.JSONLBytes(transcriptData)
	redactSpan.End()
	if redactErr != nil {
		return fmt.Errorf("failed to redact transcript: %w", redactErr)
	}

	writeOpts := cpkg.WriteOptions{
		CheckpointID:     checkpointID,
		SessionID:        sessionID,
		Strategy:         strategy.StrategyNameManualCommit,
		Transcript:       redactedTranscript,
		Prompts:          attachPrompts(meta),
		CheckpointsCount: attachStepCount(meta.TurnCount),
		AuthorName:       author.Name,
		AuthorEmail:      author.Email,
		Agent:            ag.Type(),
		Model:            meta.Model,
		TokenUsage:       tokenUsage,
	}
	if plan.mode == attachRecordLink && !isExistingCheckpoint {
		writeOpts.LinkedCommits = []cpkg.LinkedCommit{{SHA: target.Hash.String(), Repo: attachLinkRepo(ctx, plan.remote)}}
	}
	if opts.Review {
		writeOpts.Kind = string(session.KindAgentReview)
		writeOpts.ReviewSkills = reviewSkills
		writeOpts.ReviewPrompt = reviewPromptForAttach(meta, opts)
		writeOpts.HasReview = true
	}

	// ReservedSession routes by the checkpoint ID's backend, as condensation
	// does: appending to an existing ULID checkpoint under the git-branch
	// primary must land in its ref, not on the v1 branch where reads never
	// look for a ULID. A freshly minted ID already matches the primary.
	if err := store.Write(ctx, cpkg.ReservedSession(writeOpts)); err != nil {
		return fmt.Errorf("failed to write checkpoint: %w", err)
	}

	// Create or update session state.
	// Seeding BaseCommit makes the session link future commits on HEAD; an
	// attach to an older commit is about that commit only.
	seedBase := target.Hash.Equal(headCommit.Hash)
	if err := saveAttachSessionState(logCtx, repo, existingState, sessionID, ag.Type(), transcriptPath, checkpointID, meta, tokenUsage, opts, reviewSkills, seedBase); err != nil {
		logging.Warn(logCtx, "failed to save session state", "error", err)
	}

	fmt.Fprintf(w, "Attached session %s\n", sessionID)
	printAttachFooter(w, meta, tokenUsage)
	if isExistingCheckpoint {
		fmt.Fprintf(w, "  Added to existing checkpoint %s\n", checkpointID)
		return nil
	}

	fmt.Fprintf(w, "  Created checkpoint %s\n", checkpointID)
	if plan.mode == attachRecordLink {
		reportLinkedCommit(ctx, w, errW, plan)
		return nil
	}
	amendOrPrintTrailer(logCtx, w, errW, headCommit, checkpointID.String())

	return nil
}

// linkExistingCheckpoint handles a session that already has a checkpoint: it
// links that checkpoint to HEAD by the same rule as a new one. Rewriting it as a
// review, linking it to another commit with --commit, or recording a link to a
// pushed HEAD in an existing checkpoint is refused.
func linkExistingCheckpoint(logCtx context.Context, w, errW io.Writer, sessionID string, existingState *session.State, plan attachLinkPlan, opts attachOptions) error {
	cpID := existingState.LastCheckpointID.String()
	if opts.Commit != "" {
		return fmt.Errorf(
			"session %s already has checkpoint %s; linking an existing checkpoint to another commit is not supported yet",
			sessionID, cpID,
		)
	}
	// Review-upgrade isn't supported yet: the existing checkpoint's
	// metadata tree would need to be rewritten with Kind/ReviewSkills/
	// ReviewPrompt set, and a new commit pushed onto entire/checkpoints/v1.
	// Error out with a concrete message rather than silently linking the
	// checkpoint without the review metadata.
	if opts.Review {
		return fmt.Errorf(
			"session %s already has checkpoint %s; rewriting an existing checkpoint as a review is not supported yet",
			sessionID, cpID,
		)
	}
	if plan.mode == attachRecordLink {
		return fmt.Errorf(
			"session %s already has checkpoint %s, and HEAD %s is already pushed, so it won't be rewritten; recording a link to it in an existing checkpoint is not supported yet",
			sessionID, cpID, plan.target.Hash.String()[:12],
		)
	}
	fmt.Fprintf(w, "Session %s already has checkpoint %s\n", sessionID, cpID)
	amendOrPrintTrailer(logCtx, w, errW, plan.target, cpID)
	return nil
}

// attachLinkMode is how attach links a checkpoint to its target commit. It
// follows from the commit, never from a flag, so people and agents learn one
// command.
type attachLinkMode int

const (
	// attachJoinExisting: the commit already carries an Entire-Checkpoint
	// trailer; the session joins that checkpoint and nothing else is written.
	attachJoinExisting attachLinkMode = iota
	// attachAmendHead: the commit is HEAD and no remote branch holds it. The
	// trailer is amended in: nobody else has the commit, and a trailer
	// survives a later rebase, where a recorded link would be left behind.
	attachAmendHead
	// attachRecordLink: a remote branch already holds the commit, so amending
	// it would mean a force-push. The link is recorded in the checkpoint
	// (LinkedCommits) and the commit is left alone.
	attachRecordLink
)

// attachLinkPlan is the commit an attach links to and how.
type attachLinkPlan struct {
	target *object.Commit
	mode   attachLinkMode
	// remote is a remote whose branches hold target (attachRecordLink only).
	remote string
}

// planAttachLink resolves the target (HEAD unless --commit names another) and
// decides how to link it. An older commit no remote holds is refused: amending
// it means a rebase, and a recorded link wouldn't survive the rebase still
// likely to come.
func planAttachLink(ctx context.Context, repo *git.Repository, headCommit *object.Commit, opts attachOptions) (attachLinkPlan, error) {
	target := headCommit
	if opts.Commit != "" {
		var err error
		if target, err = resolveAttachCommit(repo, opts.Commit); err != nil {
			return attachLinkPlan{}, err
		}
	}
	if len(trailers.ParseAllCheckpoints(target.Message)) > 0 {
		return attachLinkPlan{target: target, mode: attachJoinExisting}, nil
	}
	remote, err := remoteHoldingCommit(ctx, target)
	if err != nil {
		return attachLinkPlan{}, err
	}
	switch {
	case remote != "":
		return attachLinkPlan{target: target, mode: attachRecordLink, remote: remote}, nil
	case target.Hash.Equal(headCommit.Hash):
		return attachLinkPlan{target: target, mode: attachAmendHead}, nil
	default:
		return attachLinkPlan{}, fmt.Errorf("commit %s is not pushed and is not HEAD, so it can't be linked: amending it would mean a rebase, and a recorded link wouldn't survive one. Push it first, or attach while it is HEAD", target.Hash.String()[:12])
	}
}

// remoteHoldingCommit returns a remote whose branches contain target, or "" when
// none does.
func remoteHoldingCommit(ctx context.Context, target *object.Commit) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "branch", "-r", "--contains", target.Hash.String(), "--format=%(refname:short)").Output()
	if err != nil {
		return "", fmt.Errorf("failed to check which remote branches contain %s: %w", target.Hash.String()[:12], err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if remote, _, ok := strings.Cut(strings.TrimSpace(line), "/"); ok && remote != "" {
			return remote, nil
		}
	}
	return "", nil
}

// reportLinkedCommit tells the user the commit was linked in the checkpoint,
// warns when the link won't count because they didn't author the commit, and
// pushes the checkpoint.
func reportLinkedCommit(ctx context.Context, w, errW io.Writer, plan attachLinkPlan) {
	fmt.Fprintf(w, "  Linked to commit %s in the checkpoint; the commit is unchanged (already pushed)\n", plan.target.Hash.String()[:12])
	if author, err := GetGitAuthor(ctx); err == nil && !strings.EqualFold(author.Email, plan.target.Author.Email) {
		fmt.Fprintf(errW, "Note: commit %s was authored by %s. A link recorded in the checkpoint counts only when the commit's author attaches it.\n",
			plan.target.Hash.String()[:12], plan.target.Author.Email)
	}
	pushAttachedCheckpoint(ctx, w, errW, plan.remote)
}

// attachLinkRepo names the code repository for a link as
// <forge>/<owner>/<repo>, from remote. Empty when it cannot be resolved; the
// server then finds the repository from the commit alone.
func attachLinkRepo(ctx context.Context, remote string) string {
	if remote == "" {
		return ""
	}
	forge, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, remote)
	if err != nil || forge == "" || owner == "" || repo == "" {
		return ""
	}
	return forge + "/" + owner + "/" + repo
}

// pushAttachedCheckpoint pushes the checkpoint metadata now: a --commit link
// usually names a commit that is already pushed, so no later git push would
// carry it. Uses the same path as the pre-push hook, which honors
// push_sessions, checkpoint_remote and the privacy filter.
func pushAttachedCheckpoint(ctx context.Context, w, errW io.Writer, remote string) {
	if err := strategy.NewManualCommitStrategy().PrePush(ctx, remote); err != nil {
		fmt.Fprintf(errW, "Could not push the checkpoint to %s (%s). It is pushed on your next git push to %s.\n", remote, firstLine(err.Error()), remote)
		return
	}
	fmt.Fprintf(w, "  Pushed checkpoint metadata to %s\n", remote)
}

// resolveAttachCommit resolves --commit to a commit in this repository,
// refusing an ambiguous short hash rather than picking one.
func resolveAttachCommit(repo *git.Repository, rev string) (*object.Commit, error) {
	hash, matches, err := resolveCommitUnambiguous(repo, rev)
	if errors.Is(err, errAmbiguousCommitPrefix) {
		return nil, fmt.Errorf("--commit %q matches %d commits; use a longer hash", rev, len(matches))
	}
	if err != nil {
		return nil, fmt.Errorf("--commit %q does not name a commit in this repository: %w", rev, err)
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return nil, fmt.Errorf("--commit %q does not name a commit in this repository: %w", rev, err)
	}
	return commit, nil
}

// amendOrPrintTrailer amends HEAD with the checkpoint trailer (best-effort).
// If the amend fails, it logs the full error, prints a brief reason to stderr,
// and falls back to printing the trailer for manual paste. The recovery path is
// non-fatal: attach still succeeds with a created checkpoint.
func amendOrPrintTrailer(logCtx context.Context, w, errW io.Writer, headCommit *object.Commit, checkpointIDStr string) {
	if err := amendHeadWithTrailer(logCtx, w, headCommit, checkpointIDStr); err != nil {
		logging.Warn(logCtx, "failed to amend commit", "error", err)
		// amendHeadWithTrailer wraps the full multi-line `git commit --amend`
		// output into the error; keep the stderr note to the first line so it
		// stays brief. The full error is preserved in the debug log above.
		fmt.Fprintf(errW, "Could not amend the commit automatically (%s).\n", firstLine(err.Error()))
		fmt.Fprintf(w, "\nCopy to your commit message to attach:\n\n  Entire-Checkpoint: %s\n", checkpointIDStr)
	}
}

// warnEmptyTranscriptMetadata warns (without failing) when nothing parsed out
// of the transcript: the checkpoint is still written and useful (code + token
// usage), but it carries no prompt or title. extractTranscriptMetadata only
// understands generic JSONL, so agents with other user-content
// shapes (codex/copilot/pi/factory) can legitimately yield empty meta from a
// valid transcript — a hard error would regress attach for them.
func warnEmptyTranscriptMetadata(errW io.Writer, agentName types.AgentName, meta transcriptMetadata, opts attachOptions) {
	if meta.FirstPrompt != "" || meta.TurnCount != 0 {
		return
	}
	fmt.Fprintf(errW, "warning: no user prompts were parsed from this transcript; the checkpoint will have no recorded prompt. Verify the --agent value (got %q) and session ID.\n", agentName)
	// Only warn about an empty review prompt when nothing will supply one. A
	// pending-review marker's ReviewPromptOverride is still recorded as the
	// review prompt via reviewPromptForAttach even with no parsed transcript prompt.
	if opts.Review && opts.ReviewPromptOverride == "" {
		fmt.Fprintln(errW, "warning: --review was set, but with no parsed prompt the review prompt will be empty.")
	}
}

// printAttachFooter writes the post-attach "Captured: …" footer when there is
// anything to report. Skipped silently when nothing is known.
func printAttachFooter(w io.Writer, meta transcriptMetadata, tokenUsage *agent.TokenUsage) {
	if summary := attachSummaryLine(meta, tokenUsage); summary != "" {
		fmt.Fprintf(w, "  Captured: %s\n", summary)
	}
}

// attachSummaryLine builds the post-attach "Captured: …" footer from data
// already in scope. Each segment is omitted when its value is absent so the
// line never renders an empty or zero field.
func attachSummaryLine(meta transcriptMetadata, tokenUsage *agent.TokenUsage) string {
	var parts []string
	if meta.TurnCount > 0 {
		noun := "turns"
		if meta.TurnCount == 1 {
			noun = "turn"
		}
		parts = append(parts, fmt.Sprintf("%d %s", meta.TurnCount, noun))
	}
	if meta.Model != "" {
		parts = append(parts, meta.Model)
	}
	if total := totalTokens(tokenUsage); total > 0 {
		parts = append(parts, formatTokenCount(total)+" tokens")
	}
	return strings.Join(parts, " · ")
}

// checkpointHasSessionMetadata reports whether sessionID has existing metadata
// at Primary. Reads target Primary directly, not refs.Read, because this guard
// must reflect what the next write would target.
func checkpointHasSessionMetadata(ctx context.Context, repo *git.Repository, refs cpkg.PersistentRefs, checkpointID id.CheckpointID, sessionID string) (bool, error) {
	store, err := openAttachStore(ctx, repo, refs.PrimaryAsLocalRead())
	if err != nil {
		return false, err
	}
	summary, err := store.Read(ctx, checkpointID)
	if err != nil {
		return false, fmt.Errorf("read checkpoint summary: %w", err)
	}
	if summary == nil {
		return false, nil
	}
	for i := range summary.Sessions {
		metadata, err := store.ReadSessionMetadata(ctx, checkpointID, i)
		if err != nil {
			return false, fmt.Errorf("read session %d metadata: %w", i, err)
		}
		if metadata != nil && metadata.SessionID == sessionID {
			return true, nil
		}
	}
	return false, nil
}

// getHeadCommit returns the HEAD commit object.
func getHeadCommit(repo *git.Repository) (*object.Commit, error) {
	headRef, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD: %w", err)
	}
	commit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD commit: %w", err)
	}
	return commit, nil
}

// ensureCheckpointAvailable makes sure the checkpoint referenced by the target commit is
// present locally before the attach writes to it. Without this guard, attach
// would create a fresh session 0 under the same ID and overwrite the original
// session data on push.
//
// Only local presence counts — remote-tracking presence is not enough. For the
// git-branch backend, if only the remote-tracking ref exists, a subsequent
// WriteCommitted creates a brand-new orphan local branch with an empty tree,
// which would clobber the remote on push.
//
// Fast path: check local storage directly — no network. If missing, fetch from
// the remote (the whole v1 branch for a branch-stored checkpoint, or just this
// checkpoint's ref for a ref-stored one; see checkpointStorageRefs) and
// re-check. Returns a possibly-freshly-opened repo handle so
// go-git sees any newly fetched refs/packfiles.
func ensureCheckpointAvailable(ctx, logCtx context.Context, repo *git.Repository, refs cpkg.PersistentRefs, checkpointID id.CheckpointID, isExistingCheckpoint bool) (*git.Repository, error) {
	if !isExistingCheckpoint {
		return repo, nil
	}

	cfg, err := settings.LoadCheckpointsConfig(ctx)
	if err != nil {
		return repo, fmt.Errorf("resolve checkpoints config: %w", err)
	}
	storedInRef := isCheckpointRef(checkpointStorageRefsFor(cfg, checkpointID)[0])

	present, readErr := checkpointPresentLocally(ctx, repo, refs, checkpointID, storedInRef)
	if readErr != nil {
		return repo, fmt.Errorf("failed to read checkpoint %s: %w", checkpointID, readErr)
	}
	if present {
		return repo, nil
	}

	// Missing locally — fetch from the remote, then re-check.
	freshRepo, fetchErr := refreshCheckpoint(ctx, checkpointID, storedInRef)
	if fetchErr != nil {
		logging.Warn(logCtx, "failed to refresh checkpoint metadata before attach; proceeding with local state",
			slog.String("error", fetchErr.Error()))
	} else {
		repo = freshRepo
		present, readErr = checkpointPresentLocally(ctx, repo, refs, checkpointID, storedInRef)
		if readErr != nil {
			return repo, fmt.Errorf("failed to read checkpoint %s after refresh: %w", checkpointID, readErr)
		}
		if present {
			return repo, nil
		}
	}

	return repo, missingCheckpointError(logCtx, checkpointID)
}

// refreshCheckpoint fetches the checkpoint referenced by the target commit from the remote and
// returns a freshly-opened repo so go-git sees the newly-fetched refs/packfiles.
// The fetch follows where the checkpoint is stored: a ref-stored checkpoint
// fetches just its ref, while a branch-stored one fetches the whole v1 metadata
// branch (the resume-equivalent chain).
func refreshCheckpoint(ctx context.Context, checkpointID id.CheckpointID, storedInRef bool) (*git.Repository, error) {
	if !storedInRef {
		_, repo, err := getMetadataTree(ctx)
		return repo, err
	}
	refName, err := cpkg.RefName(checkpointID)
	if err != nil {
		return nil, fmt.Errorf("resolve checkpoint ref for %s: %w", checkpointID, err)
	}
	if err := FetchCheckpointRef(ctx, refName); err != nil {
		return nil, err
	}
	repo, err := openRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("reopen repository after checkpoint ref fetch: %w", err)
	}
	return repo, nil
}

// checkpointPresentLocally reports whether the checkpoint already exists locally
// where it is stored (storedInRef: its own ref rather than the v1 branch). It reads local-only; the caller's refresh
// path is responsible for any remote fetch.
//
// For a branch-stored checkpoint the data lives in the v1 branch tree, and the
// store would bootstrap a missing local branch from origin's remote-tracking ref
// (PrimaryAsRead makes reads origin-bootstrappable). Counting that would let a
// WriteCommitted create a fresh orphan local branch and clobber the remote on
// push, so gate on the local Primary ref existing first. A ref-stored checkpoint
// lives at its own ref (the v1 branch is irrelevant) and the store
// read here is already local-only — attach wires no ref fetcher — so read it
// directly.
func checkpointPresentLocally(ctx context.Context, repo *git.Repository, refs cpkg.PersistentRefs, checkpointID id.CheckpointID, storedInRef bool) (bool, error) {
	if !storedInRef {
		if _, err := repo.Reference(refs.Primary, true); err != nil {
			return false, nil //nolint:nilerr // Missing local branch is the "absent" signal, not an error.
		}
	}
	store, err := openAttachStore(ctx, repo, refs.PrimaryAsLocalRead())
	if err != nil {
		return false, err
	}
	summary, err := store.Read(ctx, checkpointID)
	if err != nil {
		return false, err //nolint:wrapcheck // Caller wraps with checkpoint ID context
	}
	return summary != nil, nil
}

// missingCheckpointError builds the refuse error shown when a commit-referenced
// checkpoint is still absent locally after a refresh attempt. The storage it
// names and the fetch commands it suggests follow checkpointStorageRefs.
func missingCheckpointError(ctx context.Context, checkpointID id.CheckpointID) error {
	return fmt.Errorf(
		"checkpoint %s referenced by the commit is missing from the local %s after a refresh attempt. Creating a fresh checkpoint here would overwrite the original session data on push. Run:\n\n    %s\n\nthen re-run attach. If the colleague who made this commit hasn't pushed their checkpoint metadata yet, ask them to do so first",
		checkpointID.String(),
		describeCheckpointStorage(checkpointStorageRefs(ctx, checkpointID), "and"),
		strings.Join(suggestCheckpointStorageFetchCommands(ctx, checkpointID), "\n    "),
	)
}

// checkpointStorageRefs returns the refs that can hold checkpointID's committed
// data, in the order the store reads them (checkpoint.kindRoutingStore
// readOrder): a ULID only ever lives in its own ref; a hex ID under a git-refs
// primary is read from its ref, then from the v1 branch it may predate
// migration on; any other hex ID lives on the v1 branch. An ID that cannot form
// a ref (e.g. empty) names the v1 branch. An unreadable checkpoints config
// resolves to the git-branch default, as the store does.
func checkpointStorageRefs(ctx context.Context, checkpointID id.CheckpointID) []string {
	cfg, err := settings.LoadCheckpointsConfig(ctx)
	if err != nil {
		cfg = nil
	}
	return checkpointStorageRefsFor(cfg, checkpointID)
}

func checkpointStorageRefsFor(cfg *settings.CheckpointsConfig, checkpointID id.CheckpointID) []string {
	refName, err := cpkg.RefName(checkpointID)
	switch {
	case err != nil:
		return []string{paths.MetadataBranchName}
	case checkpointID.Kind() == id.KindULID:
		return []string{refName.String()}
	case cpkg.PrimaryIsRefs(cfg):
		return []string{refName.String(), paths.MetadataBranchName}
	default:
		return []string{paths.MetadataBranchName}
	}
}

func isCheckpointRef(ref string) bool { return strings.HasPrefix(ref, cpkg.CheckpointRefPrefix) }

// describeCheckpointStorage names refs for a message, joined by conjunction
// ("or" / "and"): "checkpoint ref <ref>" or "<branch> branch".
func describeCheckpointStorage(refs []string, conjunction string) string {
	parts := make([]string, len(refs))
	for i, ref := range refs {
		if isCheckpointRef(ref) {
			parts[i] = "checkpoint ref " + ref
		} else {
			parts[i] = ref + " branch"
		}
	}
	return strings.Join(parts, " "+conjunction+" ")
}

// suggestCheckpointStorageFetchCommands returns one git fetch command per ref
// checkpointStorageRefs names for checkpointID. They are separate commands
// because a fetch naming a ref the remote lacks fails as a whole.
func suggestCheckpointStorageFetchCommands(ctx context.Context, checkpointID id.CheckpointID) []string {
	refs := checkpointStorageRefs(ctx, checkpointID)
	cmds := make([]string, len(refs))
	for i, ref := range refs {
		cmds[i] = suggestFetchCommand(ctx, ref+":"+ref)
	}
	return cmds
}

// suggestFetchCommand builds a "git fetch <target> <refspec>" hint via
// resolveCheckpointFetchTarget (the checkpoint-remote/token URL if any, else
// origin) so the command works in a token-only environment with an SSH origin.
// Known residual: attach's own fetch iterates the read-candidate chain, so
// with elected≠origin and no dedicated store this hint can name origin while
// the data lives on the elected remote — cosmetic only, candidate for a
// follow-up.
func suggestFetchCommand(ctx context.Context, refspec string) string {
	return fmt.Sprintf("git fetch %s %s", resolveCheckpointFetchTarget(ctx), refspec)
}

func resolveCheckpointID(ctx context.Context, headCommit *object.Commit) (id.CheckpointID, bool) {
	existing := trailers.ParseAllCheckpoints(headCommit.Message)
	if len(existing) > 0 {
		return existing[len(existing)-1], true
	}

	cpID, err := cpkg.GenerateCheckpointID(ctx)
	if err != nil {
		// Generation only fails if crypto/rand fails — extremely unlikely.
		// Fall back to empty which will cause WriteCommitted to fail with a clear error.
		return id.EmptyCheckpointID, false
	}
	return cpID, false
}

// saveAttachSessionState creates or updates the session state file for the attached session.
// If existingState is non-nil, it is updated in place (avoids a redundant disk load).
// reviewSkills is the resolved skills list when opts.Review is true; ignored otherwise.
func saveAttachSessionState(ctx context.Context, repo *git.Repository, existingState *session.State, sessionID string, agentType types.AgentType, transcriptPath string, checkpointID id.CheckpointID, meta transcriptMetadata, tokenUsage *agent.TokenUsage, opts attachOptions, reviewSkills []string, seedBase bool) error {
	stateStore, err := session.NewStateStore(ctx)
	if err != nil {
		return fmt.Errorf("failed to open session store: %w", err)
	}

	now := time.Now()
	state := existingState
	if state == nil {
		state = &session.State{
			SessionID: sessionID,
			StartedAt: now,
		}
	}

	// Populate BaseCommit from HEAD if not already set, so the session becomes
	// active and future commits in the same session receive Entire-Checkpoint trailers.
	if seedBase && state.BaseCommit == "" {
		if head, headErr := repo.Head(); headErr == nil {
			headHash := head.Hash().String()
			state.BaseCommit = headHash
			state.AttributionBaseCommit = headHash
		}
	}

	state.CLIVersion = versioninfo.Version
	state.AttachedManually = true
	state.AgentType = agentType
	state.TranscriptPath = transcriptPath
	state.LastCheckpointID = checkpointID
	// Only transition to Ended if the session is not already active — avoid
	// breaking an ongoing session whose BaseCommit has just been restored above.
	if !state.Phase.IsActive() {
		state.Phase = session.PhaseEnded
	}
	state.LastInteractionTime = &now
	if meta.TurnCount > 0 {
		state.SessionTurnCount = meta.TurnCount
	}
	if meta.Model != "" {
		state.ModelName = meta.Model
	}
	if meta.FirstPrompt != "" {
		state.LastPrompt = meta.FirstPrompt
	}
	if tokenUsage != nil {
		state.TokenUsage = tokenUsage
	}
	if opts.Review {
		state.Kind = session.KindAgentReview
		state.ReviewSkills = reviewSkills
		state.ReviewPrompt = reviewPromptForAttach(meta, opts)
	}

	if err := stateStore.Save(ctx, state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}
	return nil
}

func reviewPromptForAttach(meta transcriptMetadata, opts attachOptions) string {
	if opts.ReviewPromptOverride != "" {
		return opts.ReviewPromptOverride
	}
	return meta.FirstPrompt
}

// validateAttachPreconditions checks session ID format and git repo state.
// Returns the existing session state if the session is already tracked (nil if new).
func validateAttachPreconditions(ctx context.Context, repo *git.Repository, sessionID string) (*session.State, error) {
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return nil, fmt.Errorf("invalid session ID: %w", err)
	}

	if strategy.IsEmptyRepository(repo) {
		return nil, errors.New("repository has no commits yet — make an initial commit before running attach")
	}

	store, err := session.NewStateStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open session store: %w", err)
	}
	existing, err := store.Load(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to check existing session: %w", err)
	}

	return existing, nil
}

// resolveAgentAndTranscript resolves the agent and transcript path.
// For existing sessions, resolves the agent from session state's AgentType.
// For new sessions, uses the --agent flag with auto-detection fallback.
func resolveAgentAndTranscript(ctx context.Context, w io.Writer, sessionID string, agentName types.AgentName, existingState *session.State) (agent.Agent, string, error) {
	ag, err := resolveAgent(existingState, agentName)
	if err != nil {
		return nil, "", err
	}

	transcriptPath, err := resolveAndValidateTranscript(ctx, sessionID, ag, lookupAllowFetch)
	if err != nil {
		// Auto-detect: try all other agents.
		detectedAg, detectedPath, detectErr := detectAgentByTranscript(ctx, sessionID, ag.Name())
		if detectErr != nil {
			var fetchFailure *transcriptFetchError
			if errors.As(err, &fetchFailure) {
				logging.Debug(ctx, "auto-detection also failed after transcript fetch", "error", detectErr)
				return nil, "", err
			}
			// Auto-detection never asks an agent to materialize a transcript, so
			// name the agents that could have, rather than leaving the user with
			// "is the session ID correct?" for a session that is simply owned by
			// an agent they did not name.
			return nil, "", fmt.Errorf("%w (also tried auto-detecting other agents: %w)%s",
				err, detectErr, unprobedFetcherHint(ag.Name()))
		}
		ag = detectedAg
		transcriptPath = detectedPath
		logging.Info(ctx, "auto-detected agent from transcript", "agent", ag.Name())
		fmt.Fprintf(w, "Auto-detected agent: %s\n", ag.Name())
	}

	return ag, transcriptPath, nil
}

// unprobedFetcherHint names the registered agents that can materialize a
// transcript on demand but were not asked to during auto-detection, so a user
// who named the wrong agent learns the one-flag fix. Returns "" when the only
// such agent is the one already tried.
func unprobedFetcherHint(tried types.AgentName) string {
	var names []string
	for _, name := range agent.List() {
		if name == tried {
			continue
		}
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		if _, ok := agent.AsTranscriptFetcher(ag); ok {
			names = append(names, string(name))
		}
	}
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("; %s can export transcripts on demand — retry with --agent %s",
		strings.Join(names, " and "), names[0])
}

// transcriptFetchError keeps secondary auto-detection failures from obscuring
// the fetch failure that the selected agent can explain.
type transcriptFetchError struct {
	cause error
}

func (e *transcriptFetchError) Error() string { return e.cause.Error() }
func (e *transcriptFetchError) Unwrap() error { return e.cause }

// resolveAgent resolves the agent to use. For existing sessions with an AgentType,
// uses agent.GetByAgentType. Otherwise falls back to the --agent flag.
func resolveAgent(existingState *session.State, agentName types.AgentName) (agent.Agent, error) {
	if existingState != nil && existingState.AgentType != "" {
		ag, err := agent.GetByAgentType(existingState.AgentType)
		if err == nil {
			return ag, nil
		}
		// Fall through to flag-based resolution.
	}
	ag, err := agent.Get(agentName)
	if err != nil {
		return nil, fmt.Errorf("agent %q not available: %w", agentName, err)
	}
	return ag, nil
}

// transcriptLookup says how hard resolveAndValidateTranscript may work to produce
// a transcript.
//
// Auto-detection probes every registered agent, so it must stay cheap and free of
// side effects — the same reason PrepareTranscript below is gated behind an
// os.Stat. Agent-side materialization is neither: OpenCode's FetchTranscript
// spawns `opencode export` (up to openCodeCommandTimeout) and creates
// <repo>/.entire/tmp, which is only warranted for the agent the user named.
type transcriptLookup int

const (
	// lookupLocalOnly reads what is already on disk. Used while probing agents
	// the user did not ask for.
	lookupLocalOnly transcriptLookup = iota
	// lookupAllowFetch may ask the agent to materialize the transcript.
	lookupAllowFetch
)

// resolveAndValidateTranscript finds the transcript file for a session, searching alternative
// project directories if needed.
func resolveAndValidateTranscript(ctx context.Context, sessionID string, ag agent.Agent, lookup transcriptLookup) (string, error) {
	transcriptPath, err := resolveTranscriptPath(ctx, sessionID, ag)
	if err != nil {
		return "", fmt.Errorf("failed to resolve transcript path: %w", err)
	}
	// Only call PrepareTranscript when the file already exists — it flushes
	// in-progress writes, but can't conjure a file that was never started.
	// This avoids agents like Cursor polling for 3s on non-existent files
	// during auto-detection.
	if _, statErr := agent.StatTranscriptFile(transcriptPath); statErr == nil {
		if preparer, ok := agent.AsTranscriptPreparer(ag); ok {
			if prepErr := preparer.PrepareTranscript(ctx, transcriptPath); prepErr != nil {
				logging.Debug(ctx, "PrepareTranscript failed (best-effort)", "error", prepErr)
			}
		}
		return transcriptPath, nil
	}
	// Agents that can materialize a transcript on demand (e.g. OpenCode via
	// `opencode export`) can conjure one even when no hook-cached file exists,
	// e.g. sessions spawned by an external host rather than a hooked terminal.
	var fetchErr error
	if fetcher, ok := agent.AsTranscriptFetcher(ag); ok && lookup == lookupAllowFetch {
		var fetched string
		fetched, fetchErr = fetcher.FetchTranscript(ctx, sessionID)
		if fetchErr == nil {
			return fetched, nil
		}
		if errors.Is(fetchErr, context.Canceled) {
			return "", fmt.Errorf("fetch transcript: %w", fetchErr)
		}
		logging.Debug(ctx, "FetchTranscript failed, falling back to project-dir search", "error", fetchErr)
	}
	found, searchErr := searchTranscriptInProjectDirs(sessionID, ag)
	if searchErr == nil {
		logging.Info(ctx, "found transcript in alternative project directory", "path", found)
		return found, nil
	}
	logging.Debug(ctx, "fallback transcript search failed", "error", searchErr)
	if fetchErr != nil {
		return "", &transcriptFetchError{cause: fetchErr}
	}
	return "", fmt.Errorf("transcript not found for agent %q with session %s; is the session ID correct?", ag.Name(), sessionID)
}

// detectAgentByTranscript tries all registered agents (except skip) to find one whose
// transcript resolution succeeds for the given session ID.
func detectAgentByTranscript(ctx context.Context, sessionID string, skip types.AgentName) (agent.Agent, string, error) {
	for _, name := range agent.List() {
		if name == skip {
			continue
		}
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		path, resolveErr := resolveAndValidateTranscript(ctx, sessionID, ag, lookupLocalOnly)
		if resolveErr != nil {
			logging.Debug(ctx, "auto-detect: agent did not match", "agent", string(name), "error", resolveErr)
			continue
		}
		return ag, path, nil
	}
	return nil, "", errors.New("transcript not found for any registered agent")
}

// amendHeadWithTrailer amends an unpushed HEAD with the checkpoint trailer. It
// doesn't prompt: attach only amends a commit no remote holds yet.
func amendHeadWithTrailer(ctx context.Context, w io.Writer, headCommit *object.Commit, checkpointIDStr string) error {
	shortHash := headCommit.Hash.String()[:7]

	// Skip amending if this exact checkpoint ID is already in the commit.
	for _, existing := range trailers.ParseAllCheckpoints(headCommit.Message) {
		if existing.String() == checkpointIDStr {
			fmt.Fprintf(w, "Commit %s already has Entire-Checkpoint: %s\n", shortHash, checkpointIDStr)
			return nil
		}
	}

	newMessage := trailers.AppendCheckpointTrailer(headCommit.Message, checkpointIDStr)
	cmd := exec.CommandContext(ctx, "git", "commit", "--amend", "--only", "-m", newMessage)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to amend commit: %w\n%s", err, output)
	}

	fmt.Fprintf(w, "Amended commit %s with Entire-Checkpoint: %s (not yet pushed)\n", shortHash, checkpointIDStr)
	return nil
}
