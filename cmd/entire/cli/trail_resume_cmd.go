package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	sessionpkg "github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
)

const (
	trailResumeNoPrompt = "(no prompt)"
)

type trailResumeOptions struct {
	Selector       string
	ExpectedRepo   string
	ExpectedBranch string
	SessionID      string
	CheckpointID   string
	Force          bool
	JSON           bool
	NoResume       bool
}

type trailResumeContext struct {
	Trail               trailResumeTrailContext     `json:"trail"`
	Sessions            []trailResumeSessionContext `json:"sessions"`
	SessionsUnavailable string                      `json:"sessions_unavailable,omitempty"`
	SessionsSkipped     int                         `json:"sessions_skipped,omitempty"`
	Findings            trailResumeFindingsContext  `json:"-"`
	DefaultResume       *trailResumeDefaultContext  `json:"default_resume,omitempty"`
	Commands            []string                    `json:"commands"`
}

type trailResumeTrailContext struct {
	ID     string `json:"id,omitempty"`
	Number int    `json:"number,omitempty"`
	Title  string `json:"title,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch"`
	Base   string `json:"base,omitempty"`
	Status string `json:"status,omitempty"`
	Phase  string `json:"phase,omitempty"`
	URL    string `json:"url,omitempty"`
}

type trailResumeRepository struct {
	Forge string
	Owner string
	Repo  string
}

type trailResumeSessionContext struct {
	SessionID    string    `json:"session_id"`
	Agent        string    `json:"agent,omitempty"`
	LastPrompt   string    `json:"last_prompt,omitempty"`
	LastActive   time.Time `json:"-"`
	CheckpointID string    `json:"checkpoint_id"`
}

func (s trailResumeSessionContext) MarshalJSON() ([]byte, error) {
	type trailResumeSessionContextJSON struct {
		SessionID    string     `json:"session_id"`
		Agent        string     `json:"agent,omitempty"`
		LastPrompt   string     `json:"last_prompt,omitempty"`
		LastActive   *time.Time `json:"last_active,omitempty"`
		CheckpointID string     `json:"checkpoint_id"`
	}

	var lastActive *time.Time
	if !s.LastActive.IsZero() {
		active := s.LastActive
		lastActive = &active
	}

	payload := trailResumeSessionContextJSON{
		SessionID:    s.SessionID,
		Agent:        s.Agent,
		LastPrompt:   s.LastPrompt,
		LastActive:   lastActive,
		CheckpointID: s.CheckpointID,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal trail resume session context: %w", err)
	}
	return data, nil
}

type trailResumeDefaultContext struct {
	Branch       string `json:"branch"`
	SessionID    string `json:"session_id,omitempty"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
}

type trailResumeFindingsContext struct {
	Counts      trailReviewCommentCounts `json:"counts"`
	Top         []api.TrailReviewComment `json:"top"`
	HasMore     bool                     `json:"has_more,omitempty"`
	Unavailable string                   `json:"unavailable,omitempty"`
}

type trailResumeFindingCounts struct {
	Open       int `json:"open"`
	OpenHigh   int `json:"open_high"`
	OpenMedium int `json:"open_medium"`
	OpenLow    int `json:"open_low"`
	Resolved   int `json:"resolved"`
	Dismissed  int `json:"dismissed"`
	Stale      int `json:"stale"`
}

func newTrailResumeCmd() *cobra.Command {
	var opts trailResumeOptions

	cmd := &cobra.Command{
		Use:   "resume [<trail>]",
		Short: "Resume a trail's agent session",
		Long: `Resume an agent session for a trail.

The trail may be given as the first argument or via --trail, as a number, id, or
branch. Without one, the trail for the current branch is used.

By default, interactive terminals show the trail context, restore the checkpoint
sessions on the trail branch, and ask whether Entire should start the agent. If
there are multiple sessions in the checkpoint, you can choose which one to
start. Non-interactive runs show the same context and print resume commands for
the latest checkpoint on the trail branch. Use --session or --checkpoint to
resume an exact session or checkpoint.

Use --repo to assert the GitHub repository for copied commands, and --branch
with a trail number or id to assert the branch you expect the trail to be
attached to. If either assertion does not match the current checkout or trail,
resume stops before checking anything out.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector, err := parseOptionalTrailSelector(args, opts.Selector)
			if err != nil {
				return err
			}
			opts.Selector = selector
			if err := validateTrailResumeOptions(opts); err != nil {
				return err
			}
			external.DiscoverAndRegister(cmd.Context())
			return runTrailResume(cmd, opts)
		},
	}

	cmd.Flags().StringVar(&opts.Selector, "trail", "", "Trail to resume (number, id, or branch; defaults to the current branch's trail)")
	cmd.Flags().StringVar(&opts.ExpectedRepo, "repo", "", "Expected GitHub repository (owner/name); fails if the current checkout points elsewhere")
	cmd.Flags().StringVar(&opts.ExpectedBranch, "branch", "", "Expected trail branch; fails if the trail is attached to a different branch")
	cmd.Flags().StringVar(&opts.SessionID, "session", "", "Resume a specific known local session on the trail branch")
	cmd.Flags().StringVar(&opts.CheckpointID, "checkpoint", "", "Resume a specific checkpoint on the trail branch")
	cmd.Flags().BoolVarP(&opts.Force, "force", "f", false, "Skip prompts and overwrite existing session logs from checkpoints")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output trail resume context as JSON")
	cmd.Flags().BoolVar(&opts.NoResume, "no-resume", false, "Show trail resume context without restoring or resuming a session")

	return cmd
}

func validateTrailResumeOptions(opts trailResumeOptions) error {
	if strings.TrimSpace(opts.SessionID) != "" && strings.TrimSpace(opts.CheckpointID) != "" {
		return errors.New("cannot combine --session and --checkpoint")
	}
	if opts.JSON && !opts.NoResume {
		return errors.New("--json can only be used with --no-resume")
	}
	if opts.NoResume && (strings.TrimSpace(opts.SessionID) != "" || strings.TrimSpace(opts.CheckpointID) != "") {
		return errors.New("cannot combine --no-resume with --session or --checkpoint")
	}
	if _, err := parseTrailResumeRepoFlag(opts.ExpectedRepo); err != nil {
		return fmt.Errorf("validate --repo: %w", err)
	}
	if checkpointID := strings.TrimSpace(opts.CheckpointID); checkpointID != "" {
		if err := id.Validate(checkpointID); err != nil {
			return fmt.Errorf("validate --checkpoint: %w", err)
		}
	}
	return nil
}

func runTrailResume(cmd *cobra.Command, opts trailResumeOptions) error {
	return runAuthenticatedDataAPI(cmd.Context(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd), func(ctx context.Context, client *api.Client) error {
		forge, owner, repo, err := resolveTrailRemote(ctx)
		if err != nil {
			return err
		}
		targetRepo := trailResumeRepository{Forge: forge, Owner: owner, Repo: repo}
		expectedRepo, err := parseTrailResumeRepoFlag(opts.ExpectedRepo)
		if err != nil {
			return fmt.Errorf("validate --repo: %w", err)
		}
		if err := validateTrailResumeExpectedRepo(targetRepo, expectedRepo); err != nil {
			return err
		}
		if expectedRepo.Repo != "" {
			forge, owner, repo = expectedRepo.Forge, expectedRepo.Owner, expectedRepo.Repo
		}

		found, err := resolveTrailBySelector(ctx, client, forge, owner, repo, opts.Selector, opts.ExpectedBranch)
		if err != nil {
			return err
		}
		branch := strings.TrimSpace(found.Branch)
		if branch == "" {
			return fmt.Errorf("%s has no branch to resume", describeTrailRef(found))
		}
		if err := validateTrailResumeExpectedBranch(found, opts.ExpectedBranch); err != nil {
			return err
		}

		sessions, sessionsSkipped, sessionErr := resolveTrailResumeSessionContexts(ctx, branch)
		sessions, sessionsSkipped, sessionsUnavailable := knownTrailResumeSessionsForContext(sessions, sessionsSkipped, sessionErr)
		if sessionsUnavailable != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not load trail checkpoint sessions: %s\n", sessionsUnavailable)
		}

		findings, findingsErr := loadTrailResumeFindingsContext(ctx, client, found.ID)
		if findingsErr != nil {
			findings.Unavailable = findingsErr.Error()
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not load trail findings: %v\n", findingsErr)
		}

		resumeCtx := buildTrailResumeContextForRepoWithSkipped(*found, sessions, sessionsUnavailable, sessionsSkipped, findings, owner+"/"+repo)
		if opts.JSON {
			return encodeTrailResumeContextJSON(cmd.OutOrStdout(), resumeCtx)
		}

		printTrailResumeContext(cmd.OutOrStdout(), resumeCtx)
		if opts.NoResume {
			return nil
		}

		if opts.CheckpointID != "" {
			return resumeTrailCheckpoint(ctx, cmd, branch, id.CheckpointID(opts.CheckpointID), "", opts.Force)
		}

		if opts.SessionID != "" {
			sessionCtx, ok := findTrailResumeSession(resumeCtx.Sessions, opts.SessionID)
			if ok {
				return resumeTrailCheckpoint(ctx, cmd, branch, id.CheckpointID(sessionCtx.CheckpointID), opts.SessionID, opts.Force)
			}
			return resumeTrailLatest(ctx, cmd, branch, opts.Force, opts.SessionID)
		}

		if interactive.CanPromptInteractively() && len(resumeCtx.Sessions) > 1 {
			return runTrailResumePicker(ctx, cmd, branch, resumeCtx.Sessions, opts.Force)
		}

		return resumeTrailLatest(ctx, cmd, branch, opts.Force, "")
	})
}

func parseTrailResumeRepoFlag(value string) (trailResumeRepository, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return trailResumeRepository{}, nil
	}
	owner, repo, err := parseGitHubURL(value)
	if err != nil {
		return trailResumeRepository{}, err
	}
	return trailResumeRepository{Forge: "gh", Owner: owner, Repo: repo}, nil
}

func validateTrailResumeExpectedRepo(current, expected trailResumeRepository) error {
	if strings.TrimSpace(expected.Repo) == "" {
		return nil
	}
	if current.Forge == expected.Forge &&
		strings.EqualFold(current.Owner, expected.Owner) &&
		strings.EqualFold(current.Repo, expected.Repo) {
		return nil
	}
	return fmt.Errorf("this command targets repository %s/%s, but the current checkout is %s/%s", expected.Owner, expected.Repo, current.Owner, current.Repo)
}

func validateTrailResumeExpectedBranch(found *api.TrailResource, expectedBranch string) error {
	expectedBranch = strings.TrimSpace(expectedBranch)
	if expectedBranch == "" {
		return nil
	}
	actualBranch := strings.TrimSpace(found.Branch)
	if actualBranch == expectedBranch {
		return nil
	}
	return fmt.Errorf("%s is attached to branch %q, not expected branch %q", describeTrailRef(found), actualBranch, expectedBranch)
}

func knownTrailResumeSessionsForContext(sessions []trailResumeSessionContext, skipped int, sessionErr error) ([]trailResumeSessionContext, int, string) {
	if sessionErr != nil {
		return nil, 0, sessionErr.Error()
	}
	return sessions, skipped, ""
}

func resumeTrailLatest(ctx context.Context, cmd *cobra.Command, branch string, force bool, preferredSessionID string) error {
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	if !ensureTrailResumeBranchAvailable(ctx, w, branch) {
		return nil
	}
	proceed, err := switchToBranchForResume(ctx, w, errW, branch, trailResumeSkipBranchPrompts(force))
	if err != nil || !proceed {
		return err
	}
	sessions, err := restoreFromCurrentBranch(ctx, w, errW, branch, force)
	if err != nil {
		return err
	}
	return continueTrailRestoredSessions(ctx, cmd, sessions, preferredSessionID, force)
}

func resumeTrailCheckpoint(ctx context.Context, cmd *cobra.Command, branch string, checkpointID id.CheckpointID, preferredSessionID string, force bool) error {
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	if !ensureTrailResumeBranchAvailable(ctx, w, branch) {
		return nil
	}
	proceed, err := switchToBranchForResume(ctx, w, errW, branch, trailResumeSkipBranchPrompts(force))
	if err != nil || !proceed {
		return err
	}
	sessions, err := restoreByCheckpointID(ctx, w, errW, checkpointID, force)
	if err != nil {
		return err
	}
	return continueTrailRestoredSessions(ctx, cmd, sessions, preferredSessionID, force)
}

func trailResumeSkipBranchPrompts(force bool) bool {
	return force || !interactive.CanPromptInteractively()
}

func trailResumeCanPromptRestoredSessions(force bool) bool {
	return !force && interactive.CanPromptInteractively()
}

func continueTrailRestoredSessions(ctx context.Context, cmd *cobra.Command, sessions []strategy.RestoredSession, preferredSessionID string, force bool) error {
	w := cmd.OutOrStdout()
	return continueRestoredSessions(ctx, w, sessions, restoredSessionContinueOptions{
		CanPrompt:          trailResumeCanPromptRestoredSessions(force),
		PreferredSessionID: preferredSessionID,
		PromptSession:      promptTrailRestoredSession,
		Launch:             launchTrailRestoredSession,
		Display:            displayTrailRestoredSessions,
		PrintSummary:       printTrailRestoredSessionSummary,
	})
}

func printTrailRestoredSessionSummary(w io.Writer, sessions []strategy.RestoredSession) {
	checkpointID := restoredSessionsCheckpointID(sessions)
	switch {
	case checkpointID != "" && len(sessions) > 1:
		fmt.Fprintf(w, "\n✓ Restored checkpoint %s (%d sessions).\n", checkpointID, len(sessions))
	case checkpointID != "" && len(sessions) == 1:
		fmt.Fprintf(w, "✓ Restored checkpoint %s (1 session).\n", checkpointID)
	case len(sessions) > 1:
		fmt.Fprintf(w, "\n✓ Restored %d checkpoint sessions.\n", len(sessions))
	case len(sessions) == 1:
		fmt.Fprintf(w, "✓ Restored checkpoint session %s.\n", sessions[0].SessionID)
	}
	if len(sessions) > 0 && trailRestoredSessionsAreAllReviewOrInvestigation(sessions) {
		fmt.Fprintln(w, "  Only review/investigation checkpoint sessions were found; these are transcript logs and may not appear as trail UI sessions.")
	}
}

func displayTrailRestoredSessions(w io.Writer, sessions []strategy.RestoredSession) error {
	if len(sessions) == 0 {
		return nil
	}
	choices := buildTrailResumeRestoredSessionChoices(sessions)
	printTrailRestoredSessionSummary(w, sessions)
	if len(choices) > 1 {
		fmt.Fprintln(w, "To continue:")
	} else {
		fmt.Fprintln(w, "\nTo continue this checkpoint session:")
	}

	isMulti := len(choices) > 1
	mostRecentSessionID := mostRecentRestoredSessionID(sessions)
	for _, choice := range choices {
		sessionAgent, err := strategy.ResolveAgentForRewind(choice.Session.Agent)
		if err != nil {
			return fmt.Errorf("failed to resolve agent for session %s: %w", choice.SessionID, err)
		}
		printSessionCommand(w, sessionAgent.FormatResumeCommand(choice.SessionID), trailRestoredSessionPrompt(choice.Session), isMulti, choice.SessionID == mostRecentSessionID)
	}
	return nil
}

func mostRecentRestoredSessionID(sessions []strategy.RestoredSession) string {
	var latestID string
	var latestTime time.Time
	for _, session := range sessions {
		if session.SessionID == "" || session.CreatedAt.IsZero() {
			continue
		}
		if latestID == "" || session.CreatedAt.After(latestTime) {
			latestID = session.SessionID
			latestTime = session.CreatedAt
		}
	}
	return latestID
}

func resolveTrailResumeSessionContexts(ctx context.Context, branch string) ([]trailResumeSessionContext, int, error) {
	sessions, skipped, err := resolveTrailCheckpointSessions(ctx, branch)
	if err == nil && len(sessions) > 0 {
		return sessions, skipped, nil
	}

	items, localErr := resolveTrailResumeSessions(ctx, branch)
	if localErr != nil {
		if err != nil {
			return nil, 0, err
		}
		return nil, skipped, localErr
	}
	localSessions := trailResumeSessionContextsFromLocal(branch, items)
	if len(localSessions) > 0 {
		return localSessions, skipped, nil
	}
	return sessions, skipped, err
}

func resolveTrailCheckpointSessions(ctx context.Context, branch string) ([]trailResumeSessionContext, int, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("not a git repository: %w", err)
	}
	defer repo.Close()

	result, err := findBranchCheckpointsForBranchRef(repo, branch)
	if err != nil {
		return nil, 0, err
	}
	if len(result.checkpointIDs) == 0 {
		return nil, 0, nil
	}

	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{BlobFetcher: FetchBlobsByHash})
	if err != nil {
		return nil, 0, fmt.Errorf("open checkpoint store: %w", err)
	}
	store := stores.Persistent
	refs := stores.Refs()
	if refs.ReadBootstrappableFromOrigin() {
		promoteRemoteTrackingPrimary(ctx, repo, refs)
	}

	sessions := make([]trailResumeSessionContext, 0)
	skipped := 0
	for _, checkpointID := range result.checkpointIDs {
		checkpointSessions, checkpointSkipped, readErr := readTrailCheckpointSessionContexts(ctx, store, checkpointID)
		if readErr != nil {
			return nil, 0, readErr
		}
		sessions = append(sessions, checkpointSessions...)
		skipped += checkpointSkipped
	}

	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].LastActive.After(sessions[j].LastActive)
	})
	return sessions, skipped, nil
}

func readTrailCheckpointSessionContexts(ctx context.Context, store checkpointInfoReader, checkpointID id.CheckpointID) ([]trailResumeSessionContext, int, error) {
	summary, err := checkpoint.ReadCheckpoint(ctx, store, checkpointID)
	if err != nil {
		return nil, 0, fmt.Errorf("read checkpoint %s: %w", checkpointID, err)
	}

	sessions := make([]trailResumeSessionContext, 0, len(summary.Sessions))
	skipped := 0
	for i := range summary.Sessions {
		content, contentErr := readTrailCheckpointSessionContent(ctx, store, checkpointID, i)
		if contentErr != nil {
			skipped++
			continue
		}
		metadata := content.Metadata
		if strings.TrimSpace(metadata.SessionID) == "" {
			continue
		}
		prompt := strategy.ExtractFirstPrompt(content.Prompts)
		if prompt == "" {
			prompt = strings.TrimSpace(metadata.ReviewPrompt)
		}
		sessions = append(sessions, trailResumeSessionContext{
			SessionID:    metadata.SessionID,
			Agent:        string(metadata.Agent),
			LastPrompt:   prompt,
			LastActive:   metadata.CreatedAt,
			CheckpointID: checkpointID.String(),
		})
	}

	if len(sessions) == 0 {
		info, infoErr := readCheckpointInfoFromStore(ctx, store, checkpointID)
		if infoErr == nil && strings.TrimSpace(info.SessionID) != "" {
			sessions = append(sessions, trailResumeSessionContext{
				SessionID:    info.SessionID,
				Agent:        string(info.Agent),
				LastActive:   info.CreatedAt,
				CheckpointID: checkpointID.String(),
			})
		}
	}

	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].LastActive.After(sessions[j].LastActive)
	})
	return sessions, skipped, nil
}

func readTrailCheckpointSessionContent(
	ctx context.Context,
	store checkpointInfoReader,
	checkpointID id.CheckpointID,
	sessionIndex int,
) (*checkpoint.SessionContent, error) {
	if reader, ok := store.(interface {
		ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*checkpoint.Metadata, string, error)
	}); ok {
		metadata, prompts, err := reader.ReadSessionMetadataAndPrompts(ctx, checkpointID, sessionIndex)
		if err != nil {
			return nil, err //nolint:wrapcheck // Contextualized by caller.
		}
		if metadata == nil {
			return nil, errors.New("checkpoint session metadata missing")
		}
		return &checkpoint.SessionContent{Metadata: *metadata, Prompts: prompts}, nil
	}
	metadata, err := store.ReadSessionMetadata(ctx, checkpointID, sessionIndex)
	if err != nil {
		return nil, err //nolint:wrapcheck // Contextualized by caller.
	}
	if metadata == nil {
		return nil, errors.New("checkpoint session metadata missing")
	}
	return &checkpoint.SessionContent{Metadata: *metadata}, nil
}

func resolveTrailResumeSessions(ctx context.Context, branch string) ([]resumableSession, error) {
	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}
	states = filterResumableSessions(states)

	repo, err := openRepository(ctx)
	if err != nil {
		items := make([]resumableSession, 0, len(states))
		for _, state := range states {
			if state == nil {
				continue
			}
			items = append(items, resumableSession{
				state:        state,
				branch:       state.Branch,
				checkpointID: state.LastCheckpointID,
			})
		}
		return items, nil
	}
	defer repo.Close()

	items := resolveResumableBranches(repo, states)
	for i := range items {
		if items[i].branch == "" && items[i].state != nil && items[i].state.Branch == branch {
			items[i].branch = branch
		}
	}
	return items, nil
}

func trailResumeSessionContextsFromLocal(branch string, items []resumableSession) []trailResumeSessionContext {
	var sessions []trailResumeSessionContext
	for _, item := range items {
		if item.state == nil || !item.isResumable() || item.branch != branch {
			continue
		}
		sessions = append(sessions, trailResumeSessionContext{
			SessionID:    item.state.SessionID,
			Agent:        string(item.state.AgentType),
			LastPrompt:   strings.TrimSpace(item.state.LastPrompt),
			LastActive:   sessionLastActiveTime(item.state),
			CheckpointID: item.checkpointID.String(),
		})
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].LastActive.After(sessions[j].LastActive)
	})
	return sessions
}

func loadTrailResumeFindingsContext(ctx context.Context, client *api.Client, trailID string) (trailResumeFindingsContext, error) {
	if strings.TrimSpace(trailID) == "" {
		return trailResumeFindingsContext{}, nil
	}

	summaryComments, err := fetchAllTrailReviewComments(ctx, client, trailID, trailReviewSummaryOptions())
	if err != nil {
		return trailResumeFindingsContext{}, err
	}
	top, hasMore, err := fetchTrailReviewComments(ctx, client, trailID, trailResumeTopFindingOptions())
	if err != nil {
		return trailResumeFindingsContext{}, err
	}
	return trailResumeFindingsContext{
		Counts:  countTrailReviewComments(summaryComments),
		Top:     top,
		HasMore: hasMore,
	}, nil
}

func trailResumeTopFindingOptions() trailReviewListOptions {
	return trailReviewListOptions{
		Status:    trailReviewStatusOpen,
		Severity:  strings.Join([]string{trailReviewSeverityHigh, trailReviewSeverityMedium}, ","),
		Freshness: trailReviewFreshnessCurrent,
		Limit:     3,
	}
}

func buildTrailResumeContext(found api.TrailResource, sessions []trailResumeSessionContext, sessionsUnavailable string, findings trailResumeFindingsContext) trailResumeContext {
	return buildTrailResumeContextForRepoWithSkipped(found, sessions, sessionsUnavailable, 0, findings, "")
}

func buildTrailResumeContextForRepo(found api.TrailResource, sessions []trailResumeSessionContext, sessionsUnavailable string, findings trailResumeFindingsContext, repoFullName string) trailResumeContext {
	return buildTrailResumeContextForRepoWithSkipped(found, sessions, sessionsUnavailable, 0, findings, repoFullName)
}

func buildTrailResumeContextForRepoWithSkipped(found api.TrailResource, sessions []trailResumeSessionContext, sessionsUnavailable string, sessionsSkipped int, findings trailResumeFindingsContext, repoFullName string) trailResumeContext {
	trailCtx := trailResumeTrailContext{
		ID:     found.ID,
		Number: found.Number,
		Title:  strings.TrimSpace(found.Title),
		Repo:   strings.TrimSpace(repoFullName),
		Branch: strings.TrimSpace(found.Branch),
		Base:   strings.TrimSpace(found.Base),
		Status: strings.TrimSpace(found.Status),
		Phase:  strings.TrimSpace(found.Phase),
		URL:    strings.TrimSpace(found.URL),
	}

	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].LastActive.After(sessions[j].LastActive)
	})

	var defaultResume *trailResumeDefaultContext
	if len(sessions) > 0 {
		defaultResume = &trailResumeDefaultContext{
			Branch:       trailCtx.Branch,
			SessionID:    sessions[0].SessionID,
			CheckpointID: sessions[0].CheckpointID,
		}
	} else {
		defaultResume = &trailResumeDefaultContext{Branch: trailCtx.Branch}
	}

	ctx := trailResumeContext{
		Trail:               trailCtx,
		Sessions:            sessions,
		SessionsUnavailable: sessionsUnavailable,
		SessionsSkipped:     sessionsSkipped,
		Findings:            findings,
		DefaultResume:       defaultResume,
	}
	ctx.Commands = buildTrailResumeCommands(ctx)
	return ctx
}

func buildTrailResumeCommands(ctx trailResumeContext) []string {
	selector := trailResumeSelectorForCommands(ctx.Trail)
	if selector == "" {
		return nil
	}
	arg := shellArg(selector)
	resumeCommand := "entire trail resume " + arg
	if repo := strings.TrimSpace(ctx.Trail.Repo); repo != "" {
		resumeCommand += " --repo " + shellArg(repo)
	}
	if branch := strings.TrimSpace(ctx.Trail.Branch); branch != "" {
		resumeCommand += " --branch " + shellArg(branch)
	}
	commands := []string{
		"entire trail finding " + arg + " --json",
		resumeCommand,
	}
	if ctx.DefaultResume != nil && ctx.DefaultResume.CheckpointID != "" {
		commands = append(commands, resumeCommand+" --checkpoint "+shellArg(ctx.DefaultResume.CheckpointID))
	}
	for _, session := range ctx.Sessions {
		commands = append(commands, resumeCommand+" --session "+shellArg(session.SessionID))
	}
	return commands
}

func trailResumeSelectorForCommands(trail trailResumeTrailContext) string {
	if trail.Number > 0 {
		return strconv.Itoa(trail.Number)
	}
	if trail.ID != "" {
		return trail.ID
	}
	return trail.Branch
}

func shellArg(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' || r == '/' || r == ':' {
			continue
		}
		return shellQuote(s)
	}
	return s
}

func printTrailResumeContext(w io.Writer, ctx trailResumeContext) {
	printTrailResumeTrail(w, ctx.Trail)
	printTrailResumeSessions(w, ctx.Sessions, ctx.SessionsUnavailable, ctx.SessionsSkipped)
	printTrailResumeFindings(w, ctx.Findings)
	printTrailResumeCommands(w, ctx.Commands)
	fmt.Fprintln(w)
}

func printTrailResumeTrail(w io.Writer, trail trailResumeTrailContext) {
	switch {
	case trail.Number > 0:
		fmt.Fprintf(w, "  Trail #%d  %s\n", trail.Number, trail.Title)
	case trail.ID != "":
		fmt.Fprintf(w, "  Trail %s  %s\n", trail.ID, trail.Title)
	default:
		fmt.Fprintf(w, "  Trail  %s\n", trail.Title)
	}
	parts := []string{}
	if trail.Status != "" {
		parts = append(parts, "Status: "+trail.Status)
	}
	if trail.Phase != "" {
		parts = append(parts, "Phase: "+trail.Phase)
	}
	if trail.Branch != "" {
		parts = append(parts, "Branch: "+trail.Branch)
	}
	if len(parts) > 0 {
		fmt.Fprintf(w, "  %s\n", strings.Join(parts, " · "))
	}
	if trail.Base != "" {
		fmt.Fprintf(w, "  Base: %s\n", trail.Base)
	}
	if trail.URL != "" {
		fmt.Fprintf(w, "  URL: %s\n", trail.URL)
	}
	fmt.Fprintln(w)
}

func printTrailResumeSessions(w io.Writer, sessions []trailResumeSessionContext, sessionsUnavailable string, sessionsSkipped int) {
	fmt.Fprintln(w, "  Checkpoint sessions:")
	if sessionsUnavailable != "" {
		fmt.Fprintf(w, "    unavailable before restore: %s\n", sessionsUnavailable)
		fmt.Fprintln(w)
		return
	}
	if len(sessions) == 0 {
		fmt.Fprintln(w, "    none found before restore")
		printTrailResumeSkippedSessions(w, sessionsSkipped)
		fmt.Fprintln(w)
		return
	}
	var table strings.Builder
	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tAGENT\tCHECKPOINT\tLAST ACTIVE\tPROMPT")
	for _, session := range sessions {
		prompt := strings.TrimSpace(session.LastPrompt)
		if prompt == "" {
			prompt = trailResumeNoPrompt
		} else {
			prompt = stringutil.TruncateRunes(stringutil.CollapseWhitespace(prompt), 72, "...")
		}
		agent := session.Agent
		if agent == "" {
			agent = unknownPlaceholder
		}
		when := "-"
		if !session.LastActive.IsZero() {
			when = timeAgo(session.LastActive)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			abbreviate12(session.SessionID),
			agent,
			session.CheckpointID,
			when,
			prompt,
		)
	}
	_ = tw.Flush()
	printIndentedBlock(w, table.String(), "    ")
	printTrailResumeSkippedSessions(w, sessionsSkipped)
	fmt.Fprintln(w)
}

func printTrailResumeSkippedSessions(w io.Writer, skipped int) {
	if skipped == 0 {
		return
	}
	label := "session"
	if skipped != 1 {
		label = "sessions"
	}
	fmt.Fprintf(w, "    skipped %d checkpoint %s due to read errors\n", skipped, label)
}

func printTrailResumeFindings(w io.Writer, findings trailResumeFindingsContext) {
	if findings.Unavailable != "" {
		fmt.Fprintln(w, "  Findings:")
		fmt.Fprintf(w, "    unavailable: %s\n\n", findings.Unavailable)
		return
	}
	counts := findings.Counts
	fmt.Fprintf(w, "  Findings: open %d  high %d  medium %d  low %d  resolved %d  dismissed %d  stale %d\n",
		counts.Open, counts.OpenHigh, counts.OpenMedium, counts.OpenLow, counts.Resolved, counts.Dismissed, counts.Stale)
	if len(findings.Top) == 0 {
		fmt.Fprintln(w, "    no current high/medium open findings")
		fmt.Fprintln(w)
		return
	}
	var table strings.Builder
	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSEV\tLOCATION\tSUMMARY")
	for _, finding := range findings.Top {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			abbreviate12(finding.ID),
			severityTableDisplay(finding.Severity),
			trailReviewLocationDisplay(finding.Location),
			trailReviewCommentSummary(finding),
		)
	}
	_ = tw.Flush()
	printIndentedBlock(w, table.String(), "    ")
	if findings.HasMore {
		fmt.Fprintln(w, "    more high/medium findings available; run the finding command for the full list")
	}
	fmt.Fprintln(w)
}

func printTrailResumeCommands(w io.Writer, commands []string) {
	if len(commands) == 0 {
		return
	}
	fmt.Fprintln(w, "  Commands:")
	for _, command := range commands {
		fmt.Fprintf(w, "    %s\n", command)
	}
}

func encodeTrailResumeContextJSON(w io.Writer, ctx trailResumeContext) error {
	payload := struct {
		Trail               trailResumeTrailContext     `json:"trail"`
		Sessions            []trailResumeSessionContext `json:"sessions"`
		SessionsUnavailable string                      `json:"sessions_unavailable,omitempty"`
		SessionsSkipped     int                         `json:"sessions_skipped,omitempty"`
		FindingsSummary     *trailResumeFindingCounts   `json:"findings_summary,omitempty"`
		Findings            []api.TrailReviewComment    `json:"findings"`
		FindingsHasMore     bool                        `json:"findings_has_more,omitempty"`
		FindingsUnavailable string                      `json:"findings_unavailable,omitempty"`
		DefaultResume       *trailResumeDefaultContext  `json:"default_resume,omitempty"`
		Commands            []string                    `json:"commands"`
	}{
		Trail:               ctx.Trail,
		Sessions:            ctx.Sessions,
		SessionsUnavailable: ctx.SessionsUnavailable,
		SessionsSkipped:     ctx.SessionsSkipped,
		Findings:            ctx.Findings.Top,
		FindingsHasMore:     ctx.Findings.HasMore,
		FindingsUnavailable: ctx.Findings.Unavailable,
		DefaultResume:       ctx.DefaultResume,
		Commands:            ctx.Commands,
	}
	if ctx.Findings.Unavailable == "" {
		summary := trailResumeFindingCountsFromReviewCounts(ctx.Findings.Counts)
		payload.FindingsSummary = &summary
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		return fmt.Errorf("encode trail resume JSON: %w", err)
	}
	return nil
}

func trailResumeFindingCountsFromReviewCounts(counts trailReviewCommentCounts) trailResumeFindingCounts {
	return trailResumeFindingCounts(counts)
}

func findTrailResumeSession(sessions []trailResumeSessionContext, sessionID string) (trailResumeSessionContext, bool) {
	for _, session := range sessions {
		if session.SessionID == sessionID {
			return session, true
		}
	}
	return trailResumeSessionContext{}, false
}

type trailResumeRestoredSessionChoice struct {
	SessionID string
	Label     string
	Session   strategy.RestoredSession
}

func buildTrailResumeRestoredSessionChoices(sessions []strategy.RestoredSession) []trailResumeRestoredSessionChoice {
	sorted := append([]strategy.RestoredSession(nil), sessions...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if trailRestoredSessionSortRank(sorted[i]) != trailRestoredSessionSortRank(sorted[j]) {
			return trailRestoredSessionSortRank(sorted[i]) < trailRestoredSessionSortRank(sorted[j])
		}
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})

	choices := make([]trailResumeRestoredSessionChoice, 0, len(sorted))
	for i, session := range sorted {
		choices = append(choices, trailResumeRestoredSessionChoice{
			SessionID: session.SessionID,
			Label:     trailRestoredSessionChoiceLabel(session, i == 0 && len(sorted) > 1),
			Session:   session,
		})
	}
	return choices
}

func trailRestoredSessionSortRank(session strategy.RestoredSession) int {
	switch sessionpkg.Kind(session.Kind) {
	case sessionpkg.KindAgentReview, sessionpkg.KindAgentInvestigate:
		return 1
	case sessionpkg.KindImported:
		return 0
	default:
		if trailRestoredSessionLooksReviewLike(session) {
			return 1
		}
		return 0
	}
}

func trailRestoredSessionChoiceLabel(session strategy.RestoredSession, isDefault bool) string {
	prompt := trailRestoredSessionPrompt(session)
	if prompt == "" {
		prompt = trailResumeNoPrompt
	} else {
		prompt = stringutil.TruncateRunes(stringutil.CollapseWhitespace(prompt), 50, "...")
	}
	agentName := strings.TrimSpace(string(session.Agent))
	if agentName == "" {
		agentName = unknownAgentLabel
	}
	when := "-"
	if !session.CreatedAt.IsZero() {
		when = timeAgo(session.CreatedAt)
	}
	parts := []string{session.SessionID, prompt, agentName, "last active " + when}
	if kindLabel := trailRestoredSessionKindLabel(session.Kind); kindLabel != "" {
		parts = append(parts, kindLabel)
	} else if trailRestoredSessionLooksReviewLike(session) {
		parts = append(parts, "review")
	}
	if isDefault {
		parts = append(parts, "default")
	}
	return strings.Join(parts, " · ")
}

func trailRestoredSessionKindLabel(kind string) string {
	switch sessionpkg.Kind(kind) {
	case sessionpkg.KindAgentReview:
		return "review"
	case sessionpkg.KindAgentInvestigate:
		return "investigation"
	case sessionpkg.KindImported:
		return "imported"
	default:
		return ""
	}
}

func trailRestoredSessionPrompt(session strategy.RestoredSession) string {
	if prompt := strings.TrimSpace(session.Prompt); prompt != "" {
		return prompt
	}
	return strings.TrimSpace(session.ReviewPrompt)
}

func trailRestoredSessionLooksReviewLike(session strategy.RestoredSession) bool {
	prompt := strings.ToLower(trailRestoredSessionPrompt(session))
	return strings.HasPrefix(prompt, "review the code changes") ||
		strings.HasPrefix(prompt, "review this branch") ||
		strings.HasPrefix(prompt, "review the branch")
}

func trailRestoredSessionsAreAllReviewOrInvestigation(sessions []strategy.RestoredSession) bool {
	for _, session := range sessions {
		if trailRestoredSessionSortRank(session) == 0 {
			return false
		}
	}
	return len(sessions) > 0
}

func promptTrailRestoredSession(ctx context.Context, w io.Writer, sessions []strategy.RestoredSession) (strategy.RestoredSession, bool, error) {
	choices := buildTrailResumeRestoredSessionChoices(sessions)
	if len(choices) == 0 {
		return strategy.RestoredSession{}, false, nil
	}

	options := make([]huh.Option[string], 0, len(choices)+1)
	for _, choice := range choices {
		options = append(options, huh.NewOption(choice.Label, choice.SessionID))
	}
	options = append(options, huh.NewOption("Cancel", resumePickerCancel))

	selected := choices[0].SessionID
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Choose a checkpoint session to resume").
				Description("These are agent transcript logs restored from the branch checkpoint; they may not appear as trail UI sessions.").
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			return strategy.RestoredSession{}, false, nil
		}
		return strategy.RestoredSession{}, false, fmt.Errorf("selection failed: %w", err)
	}
	if selected == "" || selected == resumePickerCancel {
		fmt.Fprintln(w, "Resume cancelled.")
		return strategy.RestoredSession{}, false, nil
	}
	for _, choice := range choices {
		if choice.SessionID == selected {
			return choice.Session, true, nil
		}
	}
	return strategy.RestoredSession{}, false, fmt.Errorf("invalid selection %q", selected)
}

func findTrailRestoredSession(sessions []strategy.RestoredSession, sessionID string) (strategy.RestoredSession, bool) {
	for _, session := range sessions {
		if session.SessionID == sessionID {
			return session, true
		}
	}
	return strategy.RestoredSession{}, false
}

func launchTrailRestoredSession(ctx context.Context, w io.Writer, session strategy.RestoredSession) error {
	resumeAgent, err := strategy.ResolveAgentForRewind(session.Agent)
	if err != nil {
		return fmt.Errorf("failed to resolve agent for session %s: %w", session.SessionID, err)
	}
	resumeCmd := resumeAgent.FormatResumeCommand(session.SessionID)
	cmd, ok, err := agent.NewResumeForegroundCommand(ctx, resumeAgent.Name(), session.SessionID)
	if !ok {
		fmt.Fprintf(w, "\nTo continue this session:\n")
		printSessionCommand(w, resumeCmd, session.Prompt, false, true)
		return nil
	}
	if err != nil {
		fmt.Fprintf(w, "\nCould not launch %s: %v\n", resumeCmd, err)
		fmt.Fprintf(w, "\nTo continue this session:\n")
		printSessionCommand(w, resumeCmd, session.Prompt, false, true)
		return nil
	}
	fmt.Fprintf(w, "\nLaunching: %s\n", resumeCmd)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil
		}
		return fmt.Errorf("resume command failed: %w", err)
	}
	return nil
}

func runTrailResumePicker(ctx context.Context, cmd *cobra.Command, branch string, sessions []trailResumeSessionContext, force bool) error {
	if !ensureTrailResumeBranchAvailable(ctx, cmd.OutOrStdout(), branch) {
		return nil
	}

	options := make([]huh.Option[string], 0, len(sessions)+1)
	for _, session := range sessions {
		options = append(options, huh.NewOption(trailResumeSessionOptionLabel(session), session.SessionID))
	}
	options = append(options, huh.NewOption("Cancel", resumePickerCancel))

	selected := sessions[0].SessionID
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Choose a checkpoint session to resume").
				Description("These sessions are recorded in the trail branch checkpoint.").
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("selection failed: %w", err)
	}
	if selected == "" || selected == resumePickerCancel {
		fmt.Fprintln(cmd.OutOrStdout(), "Resume cancelled.")
		return nil
	}
	sessionCtx, ok := findTrailResumeSession(sessions, selected)
	if !ok {
		return fmt.Errorf("invalid selection %q", selected)
	}
	return resumeTrailCheckpoint(ctx, cmd, branch, id.CheckpointID(sessionCtx.CheckpointID), sessionCtx.SessionID, force)
}

func ensureTrailResumeBranchAvailable(ctx context.Context, w io.Writer, branch string) bool {
	otherPath, ok := branchCheckedOutElsewhere(ctx, branch)
	if !ok {
		return true
	}
	fmt.Fprint(w, trailResumeWorktreeClashMessage(branch, otherPath))
	return false
}

func trailResumeWorktreeClashMessage(branch, otherPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Branch %q is already checked out in another worktree:\n", branch)
	fmt.Fprintf(&b, "  %s\n\n", otherPath)
	fmt.Fprintf(&b, "Resume from that worktree with:\n")
	fmt.Fprintf(&b, "  cd %s && entire trail resume %s\n", shellQuote(otherPath), shellArg(branch))
	return b.String()
}

func trailResumeSessionOptionLabel(session trailResumeSessionContext) string {
	prompt := strings.TrimSpace(session.LastPrompt)
	if prompt == "" {
		prompt = trailResumeNoPrompt
	} else {
		prompt = stringutil.TruncateRunes(stringutil.CollapseWhitespace(prompt), 50, "...")
	}
	agent := session.Agent
	if agent == "" {
		agent = unknownAgentLabel
	}
	when := "-"
	if !session.LastActive.IsZero() {
		when = timeAgo(session.LastActive)
	}
	return fmt.Sprintf("%s · %s · %s · last active %s", session.SessionID, prompt, agent, when)
}
