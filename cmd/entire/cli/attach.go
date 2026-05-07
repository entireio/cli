package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/perf"
	"github.com/entireio/cli/redact"

	"charm.land/huh/v2"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/spf13/cobra"
)

// attachOptions carries optional flags for runAttach. Force is the original
// flag; Review opts the attach into recording the session as an
// agent_review in the checkpoint metadata.
type attachOptions struct {
	Force bool
	// Review, when true, tags the attached session as a review. Skills are
	// resolved inside runAttach after the real agent is known (via session
	// state or transcript auto-detection), not at the cobra layer — the
	// --agent flag's default points at claude-code, which would otherwise
	// make a Gemini session incorrectly look up review.claude-code config.
	Review bool
	// ReviewSkillsOverride, when non-empty, declares which review skills were
	// run. Empty is valid: the session is still tagged as a review, with no
	// structured skills list. Ignored when Review=false.
	ReviewSkillsOverride []string
	// ReviewPromptOverride, when non-empty, is recorded instead of the
	// transcript's first user prompt. Used by `entire review attach` when a
	// pending-review marker has the exact prompt the user was asked to run.
	ReviewPromptOverride string
}

func newAttachCmd() *cobra.Command {
	var (
		force      bool
		agentFlag  string
		reviewFlag bool
		skillsFlag []string
	)
	cmd := &cobra.Command{
		Use:   "attach <session-id>",
		Short: "Attach an existing agent session",
		Long: `Attach an existing agent session that wasn't captured by hooks.

This creates a checkpoint from the session's transcript and links it to the
last commit. Use this when hooks failed to fire or weren't installed when
the session started, or to attach a research session.

If the last commit already has a checkpoint, the session is added to it.
Otherwise a new checkpoint is created.

Use --review to tag the attached session as an agent review. The
first user prompt in the transcript is recorded as the review prompt.
Pass --skills to declare which skills were actually run; omit to
attach a review without a declared skills list.

Works with any registered agent, including external agents enabled via
external_agents in settings. Run 'entire agent list' to see the full list.`,
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
			agentName := types.AgentName(agentFlag)
			opts := attachOptions{
				Force:                force,
				Review:               reviewFlag,
				ReviewSkillsOverride: skillsFlag,
			}
			return runAttachSurfaceReviewErrors(cmd, args[0], agentName, opts)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Skip confirmation and amend the last commit with the checkpoint trailer")
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
	err := runAttach(cmd.Context(), cmd.OutOrStdout(), sessionID, agentName, opts)
	if err != nil && opts.Review {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return NewSilentError(err)
	}
	return err
}

func runAttach(ctx context.Context, w io.Writer, sessionID string, agentName types.AgentName, opts attachOptions) error {
	// Initialize structured logger so logging.Warn/Info write to .entire/logs/ not stderr.
	if err := logging.Init(ctx, sessionID); err != nil {
		// Init failed — logging will use stderr fallback, non-fatal.
		_ = err
	}
	// Flush the 8KB buffered log writer on exit. Without this, any
	// Warn/Info calls during attach (including the overwrite tripwire)
	// get silently dropped when the process exits, matching the pattern
	// already used by resume/clean/reset/rewind/migrate/explain.
	defer logging.Close()

	logCtx := logging.WithComponent(ctx, "attach")

	// Open repository once — shared across all operations.
	repo, err := openRepository(ctx)
	if err != nil {
		return err
	}

	existingState, err := validateAttachPreconditions(ctx, repo, sessionID)
	if err != nil {
		return err
	}

	headCommit, err := getHeadCommit(repo)
	if err != nil {
		return err
	}

	// If session already has a checkpoint, just offer to link it.
	if existingState != nil && !existingState.LastCheckpointID.IsEmpty() {
		// Review-upgrade isn't supported yet: the existing checkpoint's
		// metadata tree would need to be rewritten with Kind/ReviewSkills/
		// ReviewPrompt set, and a new commit pushed onto entire/checkpoints/v1.
		// Error out with a concrete message rather than silently linking the
		// checkpoint without the review metadata.
		if opts.Review {
			return fmt.Errorf(
				"session %s already has checkpoint %s; rewriting an existing checkpoint as a review is not supported yet",
				sessionID, existingState.LastCheckpointID.String(),
			)
		}
		cpID := existingState.LastCheckpointID.String()
		fmt.Fprintf(w, "Session %s already has checkpoint %s\n", sessionID, cpID)
		if err := promptAmendCommit(logCtx, w, headCommit, cpID, opts.Force); err != nil {
			logging.Warn(logCtx, "failed to amend commit", "error", err)
			fmt.Fprintf(w, "\nCopy to your commit message to attach:\n\n  Entire-Checkpoint: %s\n", cpID)
		}
		return nil
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

	// Normalize Gemini transcripts for storage.
	storedTranscript := transcriptData
	if ag.Type() == agent.AgentTypeGemini {
		if normalized, normErr := geminicli.NormalizeTranscript(transcriptData); normErr == nil {
			storedTranscript = normalized
		} else {
			logging.Warn(logCtx, "failed to normalize Gemini transcript, storing raw", "error", normErr)
		}
	}

	meta := extractTranscriptMetadata(transcriptData)

	// Determine checkpoint ID: reuse from HEAD if one exists, otherwise generate new.
	checkpointID, isExistingCheckpoint := resolveCheckpointID(headCommit)

	// If HEAD references an existing checkpoint, make sure we have it locally
	// before writing — otherwise we'd create a fresh session 0 under the same
	// ID and overwrite the original on push.
	repo, err = ensureCheckpointAvailable(ctx, logCtx, repo, checkpointID, isExistingCheckpoint)
	if err != nil {
		return err
	}

	// Write directly to entire/checkpoints/v1.
	store := cpkg.NewGitStore(repo)

	// Defense-in-depth guard: the earlier existingState.LastCheckpointID
	// check only fires when the session's state file records its
	// checkpoint. A session already stored in the HEAD checkpoint but
	// whose state is missing/stale (state file deleted, never written,
	// condensed without LastCheckpointID update, or pulled from a remote
	// that wasn't reflected locally) would bypass that guard.
	// findSessionIndex matches by SessionID — without this check, a
	// review-attach on such a session silently overwrites the existing
	// session's metadata in the checkpoint.
	if opts.Review && isExistingCheckpoint {
		if existing, readErr := store.ReadSessionContentByID(ctx, checkpointID, sessionID); readErr == nil && existing != nil {
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

	var prompts []string
	if meta.FirstPrompt != "" {
		prompts = []string{meta.FirstPrompt}
	}

	tokenUsage := agent.CalculateTokenUsage(logCtx, ag, transcriptData, 0, "")

	_, redactSpan := perf.Start(ctx, "redact_transcript")
	redactedTranscript, redactErr := redact.JSONLBytes(storedTranscript)
	redactSpan.End()
	if redactErr != nil {
		return fmt.Errorf("failed to redact transcript: %w", redactErr)
	}

	writeOpts := cpkg.WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		Strategy:     strategy.StrategyNameManualCommit,
		Transcript:   redactedTranscript,
		Prompts:      prompts,
		AuthorName:   author.Name,
		AuthorEmail:  author.Email,
		Agent:        ag.Type(),
		Model:        meta.Model,
		TokenUsage:   tokenUsage,
	}
	if opts.Review {
		writeOpts.Kind = string(session.KindAgentReview)
		writeOpts.ReviewSkills = reviewSkills
		writeOpts.ReviewPrompt = reviewPromptForAttach(meta, opts)
		writeOpts.HasReview = true
	}

	if compacted := compactTranscriptForStartLine(logCtx, redactedTranscript.Bytes(), cpkg.CommittedMetadata{
		CheckpointID: checkpointID,
		Agent:        ag.Type(),
	}, 0); compacted != nil {
		writeOpts.CompactTranscript = compacted
	}

	v2 := settings.CheckpointsVersion(logCtx) == 2
	if !v2 {
		if err := store.WriteCommitted(ctx, writeOpts); err != nil {
			return fmt.Errorf("failed to write checkpoint: %w", err)
		}
	}
	// IsCheckpointsV2Enabled is true whenever v2 writes are enabled, including
	// both v2-only mode (checkpoints_version == 2) and dual-write mode. Only
	// v2-only mode propagates the error.
	if settings.IsCheckpointsV2Enabled(logCtx) {
		if err := writeAttachCheckpointV2(logCtx, repo, writeOpts); err != nil {
			if v2 {
				return fmt.Errorf("failed to write checkpoint to v2: %w", err)
			}
			logging.Warn(logCtx, "attach v2 dual-write failed", "error", err)
		}
	}

	// Create or update session state.
	if err := saveAttachSessionState(logCtx, repo, existingState, sessionID, ag.Type(), transcriptPath, checkpointID, meta, tokenUsage, opts, reviewSkills); err != nil {
		logging.Warn(logCtx, "failed to save session state", "error", err)
	}

	fmt.Fprintf(w, "Attached session %s\n", sessionID)
	if isExistingCheckpoint {
		fmt.Fprintf(w, "  Added to existing checkpoint %s\n", checkpointID)
		return nil
	}

	fmt.Fprintf(w, "  Created checkpoint %s\n", checkpointID)
	cpIDStr := checkpointID.String()
	if err := promptAmendCommit(logCtx, w, headCommit, cpIDStr, opts.Force); err != nil {
		logging.Warn(logCtx, "failed to amend commit", "error", err)
		fmt.Fprintf(w, "\nCopy to your commit message to attach:\n\n  Entire-Checkpoint: %s\n", cpIDStr)
	}

	return nil
}

// writeAttachCheckpointV2 writes attach-created checkpoints into the v2 refs.
func writeAttachCheckpointV2(ctx context.Context, repo *git.Repository, opts cpkg.WriteCommittedOptions) error {
	v2URL, err := remote.FetchURL(ctx)
	if err != nil {
		logging.Debug(ctx, "attach: using origin for v2 store fetch remote",
			slog.String("error", err.Error()),
		)
	}
	v2Store := cpkg.NewV2GitStore(repo, v2URL)
	if err := v2Store.WriteCommitted(ctx, opts); err != nil {
		return fmt.Errorf("v2 write committed: %w", err)
	}
	return nil
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

// ensureCheckpointAvailable makes sure the checkpoint referenced by HEAD is
// present locally before the attach writes to it. Without this guard, attach
// would create a fresh session 0 under the same ID and overwrite the original
// session data on push.
//
// Only the local branch counts — remote-tracking presence is not enough.
// If only the remote-tracking ref exists, a subsequent WriteCommitted creates
// a brand-new orphan local branch with an empty tree, which would clobber
// the remote on push.
//
// Fast path: check local refs directly — no network. If missing, trigger the
// metadata fetch fallback chain used by `entire resume` (which advances the
// local ref on success) and re-check. Returns a possibly-freshly-opened repo
// handle so go-git sees any newly fetched packfiles.
func ensureCheckpointAvailable(ctx, logCtx context.Context, repo *git.Repository, checkpointID id.CheckpointID, isExistingCheckpoint bool) (*git.Repository, error) {
	if !isExistingCheckpoint {
		return repo, nil
	}

	v2Only := settings.CheckpointsVersion(logCtx) == 2

	present, readErr := checkpointPresentLocally(ctx, repo, checkpointID, v2Only)
	if readErr != nil {
		return repo, fmt.Errorf("failed to read checkpoint %s: %w", checkpointID, readErr)
	}
	if present {
		return repo, nil
	}

	// Missing locally — try to refresh, then re-check. Use the same fetch
	// chain `entire resume` uses for the active storage version (v2 refs live
	// under refs/entire/, not refs/heads/, so v1 and v2 need different
	// refspecs).
	freshRepo, fetchErr := refreshCheckpointRefs(ctx, v2Only)
	if fetchErr != nil {
		logging.Warn(logCtx, "failed to refresh metadata branch before attach; proceeding with local state",
			slog.String("error", fetchErr.Error()))
	} else {
		repo = freshRepo
		present, readErr = checkpointPresentLocally(ctx, repo, checkpointID, v2Only)
		if readErr != nil {
			return repo, fmt.Errorf("failed to read checkpoint %s after refresh: %w", checkpointID, readErr)
		}
		if present {
			return repo, nil
		}
	}

	branchDescription := "entire/checkpoints/v1 branch"
	if v2Only {
		branchDescription = "v2 /main ref"
	}
	return repo, fmt.Errorf(
		"checkpoint %s referenced by HEAD is missing from the local %s after a refresh attempt. Creating a fresh checkpoint here would overwrite the original session data on push. Run:\n\n    %s\n\nthen re-run attach. If the colleague who made this commit hasn't pushed their checkpoint metadata yet, ask them to do so first",
		checkpointID.String(), branchDescription, suggestCheckpointFetchCommand(logCtx, v2Only),
	)
}

// refreshCheckpointRefs runs the resume-equivalent fetch chain for the storage
// version we're about to write to. Returns a freshly-opened repo so go-git
// sees any newly-fetched packfiles and ref updates.
func refreshCheckpointRefs(ctx context.Context, v2Only bool) (*git.Repository, error) {
	if v2Only {
		_, repo, err := getV2MetadataTree(ctx)
		return repo, err
	}
	_, repo, err := getMetadataTree(ctx)
	return repo, err
}

// checkpointPresentLocally reports whether the checkpoint already exists on
// the local ref we would write to. For v1 / dual-write, that's the local
// entire/checkpoints/v1 branch (remote-tracking alone is not enough — see
// ensureCheckpointAvailable). For v2-only mode, it's the v2 /main ref, which
// has no remote-tracking analog and is therefore already local-only by
// construction.
func checkpointPresentLocally(ctx context.Context, repo *git.Repository, checkpointID id.CheckpointID, v2Only bool) (bool, error) {
	if v2Only {
		v2URL, urlErr := remote.FetchURL(ctx)
		if urlErr != nil {
			logging.Debug(ctx, "attach: using origin for v2 store fetch remote",
				slog.String("error", urlErr.Error()),
			)
		}
		summary, err := cpkg.NewV2GitStore(repo, v2URL).ReadCommitted(ctx, checkpointID)
		if err != nil {
			return false, err //nolint:wrapcheck // Caller wraps with checkpoint ID context
		}
		return summary != nil, nil
	}

	localRef := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	if _, err := repo.Reference(localRef, true); err != nil {
		// Local branch ref doesn't exist — treat as "not present locally".
		// We deliberately do not fall back to remote-tracking: see
		// ensureCheckpointAvailable's docstring.
		return false, nil //nolint:nilerr // Missing ref is the "absent" signal, not an error.
	}
	summary, err := cpkg.NewGitStore(repo).ReadCommitted(ctx, checkpointID)
	if err != nil {
		return false, err //nolint:wrapcheck // Caller wraps with checkpoint ID context
	}
	return summary != nil, nil
}

// suggestCheckpointFetchCommand returns a git fetch command the user can
// paste to pull the missing metadata ref. v2 refs live under refs/entire/
// (not refs/heads/), so they need an explicit fully-qualified refspec;
// v1 lives on a regular branch and its short name is enough.
func suggestCheckpointFetchCommand(ctx context.Context, v2Only bool) string {
	ref := "entire/checkpoints/v1:entire/checkpoints/v1"
	if v2Only {
		ref = paths.V2MainRefName + ":" + paths.V2MainRefName
	}
	if remote.Configured(ctx) {
		if url, err := remote.FetchURL(ctx); err == nil && url != "" {
			return fmt.Sprintf("git fetch %s %s", url, ref)
		}
	}
	return "git fetch origin " + ref
}

func resolveCheckpointID(headCommit *object.Commit) (id.CheckpointID, bool) {
	existing := trailers.ParseAllCheckpoints(headCommit.Message)
	if len(existing) > 0 {
		return existing[len(existing)-1], true
	}

	cpID, err := id.Generate()
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
func saveAttachSessionState(ctx context.Context, repo *git.Repository, existingState *session.State, sessionID string, agentType types.AgentType, transcriptPath string, checkpointID id.CheckpointID, meta transcriptMetadata, tokenUsage *agent.TokenUsage, opts attachOptions, reviewSkills []string) error {
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
	if state.BaseCommit == "" {
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

	transcriptPath, err := resolveAndValidateTranscript(ctx, sessionID, ag)
	if err != nil {
		// Auto-detect: try all other agents.
		detectedAg, detectedPath, detectErr := detectAgentByTranscript(ctx, sessionID, agentName)
		if detectErr != nil {
			return nil, "", fmt.Errorf("%w (also tried auto-detecting other agents: %w)", err, detectErr)
		}
		ag = detectedAg
		transcriptPath = detectedPath
		logging.Info(ctx, "auto-detected agent from transcript", "agent", ag.Name())
		fmt.Fprintf(w, "Auto-detected agent: %s\n", ag.Name())
	}

	return ag, transcriptPath, nil
}

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

// resolveAndValidateTranscript finds the transcript file for a session, searching alternative
// project directories if needed.
func resolveAndValidateTranscript(ctx context.Context, sessionID string, ag agent.Agent) (string, error) {
	transcriptPath, err := resolveTranscriptPath(ctx, sessionID, ag)
	if err != nil {
		return "", fmt.Errorf("failed to resolve transcript path: %w", err)
	}
	// Only call PrepareTranscript when the file already exists — it flushes
	// in-progress writes, but can't conjure a file that was never started.
	// This avoids agents like Cursor polling for 3s on non-existent files
	// during auto-detection.
	if _, statErr := os.Stat(transcriptPath); statErr == nil {
		if preparer, ok := agent.AsTranscriptPreparer(ag); ok {
			if prepErr := preparer.PrepareTranscript(ctx, transcriptPath); prepErr != nil {
				logging.Debug(ctx, "PrepareTranscript failed (best-effort)", "error", prepErr)
			}
		}
		return transcriptPath, nil
	}
	found, searchErr := searchTranscriptInProjectDirs(sessionID, ag)
	if searchErr == nil {
		logging.Info(ctx, "found transcript in alternative project directory", "path", found)
		return found, nil
	}
	logging.Debug(ctx, "fallback transcript search failed", "error", searchErr)
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
		path, resolveErr := resolveAndValidateTranscript(ctx, sessionID, ag)
		if resolveErr != nil {
			logging.Debug(ctx, "auto-detect: agent did not match", "agent", string(name), "error", resolveErr)
			continue
		}
		return ag, path, nil
	}
	return nil, "", errors.New("transcript not found for any registered agent")
}

// promptAmendCommit shows the last commit and asks whether to amend it with the checkpoint trailer.
// When force is true, it amends without prompting.
func promptAmendCommit(ctx context.Context, w io.Writer, headCommit *object.Commit, checkpointIDStr string, force bool) error {
	shortHash := headCommit.Hash.String()[:7]
	subject := strings.SplitN(headCommit.Message, "\n", 2)[0]

	// Skip amending if this exact checkpoint ID is already in the commit.
	for _, existing := range trailers.ParseAllCheckpoints(headCommit.Message) {
		if existing.String() == checkpointIDStr {
			fmt.Fprintf(w, "Commit %s already has Entire-Checkpoint: %s\n", shortHash, checkpointIDStr)
			return nil
		}
	}

	fmt.Fprintf(w, "\nLast commit: %s %s\n", shortHash, subject)

	amend := true
	if !force {
		if !interactive.CanPromptInteractively() {
			// Non-interactive: can't prompt, print trailer for manual use.
			fmt.Fprintf(w, "\nCopy to your commit message to attach:\n\n  Entire-Checkpoint: %s\n", checkpointIDStr)
			return nil
		}
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Amend the last commit in this branch?").
					Affirmative("Y").
					Negative("n").
					Value(&amend),
			),
		)
		if err := form.Run(); err != nil {
			return fmt.Errorf("prompt failed: %w", err)
		}
	}

	if !amend {
		fmt.Fprintf(w, "\nCopy to your commit message to attach:\n\n  Entire-Checkpoint: %s\n", checkpointIDStr)
		return nil
	}

	newMessage := trailers.AppendCheckpointTrailer(headCommit.Message, checkpointIDStr)

	cmd := exec.CommandContext(ctx, "git", "commit", "--amend", "--only", "-m", newMessage)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to amend commit: %w\n%s", err, output)
	}

	fmt.Fprintf(w, "Amended commit %s with Entire-Checkpoint: %s\n", shortHash, checkpointIDStr)
	return nil
}
