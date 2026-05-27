package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
)

type migrationOptions struct {
	apply bool
}

type migrationReport struct {
	DiscoveredCheckpoints       int
	ExistingV1Sessions          int
	MissingV2CheckpointMetadata int
	MissingV2SessionMetadata    int
	MissingRawTranscripts       int
	EligibleCheckpoints         int
	EligibleSessions            int
	MigratedCheckpoints         int
	MigratedSessions            int
}

type checkpointMigrator struct {
	v1Store     *checkpoint.GitStore
	v2Store     *checkpoint.V2GitStore
	opts        migrationOptions
	authorName  string
	authorEmail string
	report      *migrationReport
}

func migrateDiscoveredCheckpoints(ctx context.Context, repo *git.Repository, discovered []discoveredCheckpoint, opts migrationOptions) (migrationReport, error) {
	authorName, authorEmail := checkpoint.GetGitAuthorFromRepo(repo)
	v2Store := checkpoint.NewV2GitStore(repo)
	report := migrationReport{DiscoveredCheckpoints: len(discovered)}
	migrator := checkpointMigrator{
		v1Store:     checkpoint.NewGitStore(repo),
		v2Store:     v2Store,
		opts:        opts,
		authorName:  authorName,
		authorEmail: authorEmail,
		report:      &report,
	}

	for _, discoveredCheckpoint := range discovered {
		migratedSessions, err := migrator.migrateCheckpoint(ctx, discoveredCheckpoint)
		if err != nil {
			return report, err
		}
		if migratedSessions == 0 {
			continue
		}
		report.EligibleCheckpoints++
		if opts.apply {
			report.MigratedCheckpoints++
		}
	}
	return report, nil
}

func (m checkpointMigrator) migrateCheckpoint(ctx context.Context, discovered discoveredCheckpoint) (int, error) {
	existing, err := m.v1Store.ReadCommitted(ctx, discovered.ID)
	if err != nil {
		return 0, fmt.Errorf("read v1 checkpoint %s: %w", discovered.ID, err)
	}
	existingSessionIDs, err := m.existingV1SessionIDs(ctx, discovered, existing)
	if err != nil {
		return 0, err
	}

	summary, err := m.v2Store.ReadCommitted(ctx, discovered.ID)
	if err != nil {
		return 0, fmt.Errorf("read v2 checkpoint %s: %w", discovered.ID, err)
	}
	if summary == nil || len(summary.Sessions) == 0 {
		m.report.MissingV2CheckpointMetadata++
		return 0, nil
	}

	migratedSessions := 0
	for sessionIndex := range summary.Sessions {
		metadataContent, err := m.v2Store.ReadSessionMetadataAndPrompts(ctx, discovered.ID, sessionIndex)
		if err != nil {
			if errors.Is(err, checkpoint.ErrCheckpointNotFound) {
				m.report.MissingV2SessionMetadata++
				continue
			}
			return migratedSessions, fmt.Errorf("read v2 checkpoint %s session %d metadata: %w", discovered.ID, sessionIndex, err)
		}
		if !hasRequiredV2Metadata(metadataContent) {
			m.report.MissingV2SessionMetadata++
			continue
		}
		if _, exists := existingSessionIDs[metadataContent.Metadata.SessionID]; exists {
			m.report.ExistingV1Sessions++
			continue
		}

		content, err := m.v2Store.ReadSessionContent(ctx, discovered.ID, sessionIndex)
		if err != nil {
			if errors.Is(err, checkpoint.ErrNoTranscript) {
				m.report.MissingRawTranscripts++
				continue
			}
			return migratedSessions, fmt.Errorf("read v2 checkpoint %s session %d: %w", discovered.ID, sessionIndex, err)
		}

		m.report.EligibleSessions++
		if m.opts.apply {
			writeOpts := writeOptionsFromV2Content(content, summary, m.authorName, m.authorEmail)
			if err := m.v1Store.WriteCommitted(ctx, writeOpts); err != nil {
				return migratedSessions, fmt.Errorf("write v1 checkpoint %s session %d: %w", discovered.ID, sessionIndex, err)
			}
			m.report.MigratedSessions++
		}
		migratedSessions++
	}
	return migratedSessions, nil
}

func (m checkpointMigrator) existingV1SessionIDs(ctx context.Context, discovered discoveredCheckpoint, summary *checkpoint.CheckpointSummary) (map[string]struct{}, error) {
	existing := make(map[string]struct{})
	if summary == nil {
		return existing, nil
	}
	for sessionIndex := range summary.Sessions {
		content, err := m.v1Store.ReadSessionMetadataAndPrompts(ctx, discovered.ID, sessionIndex)
		if err != nil {
			return nil, fmt.Errorf("read v1 checkpoint %s session %d metadata: %w", discovered.ID, sessionIndex, err)
		}
		if content.Metadata.SessionID == "" {
			continue
		}
		existing[content.Metadata.SessionID] = struct{}{}
	}
	return existing, nil
}

func hasRequiredV2Metadata(content *checkpoint.SessionContent) bool {
	return !content.Metadata.CheckpointID.IsEmpty() && content.Metadata.SessionID != ""
}

func writeOptionsFromV2Content(content *checkpoint.SessionContent, summary *checkpoint.CheckpointSummary, authorName, authorEmail string) checkpoint.WriteCommittedOptions {
	meta := content.Metadata
	return checkpoint.WriteCommittedOptions{
		CheckpointID:                meta.CheckpointID,
		SessionID:                   meta.SessionID,
		CreatedAt:                   meta.CreatedAt,
		CommitTime:                  meta.CreatedAt,
		Strategy:                    meta.Strategy,
		Branch:                      meta.Branch,
		Transcript:                  redact.AlreadyRedacted(content.Transcript),
		Prompts:                     checkpoint.SplitPromptContent(content.Prompts),
		FilesTouched:                meta.FilesTouched,
		CheckpointsCount:            meta.CheckpointsCount,
		AuthorName:                  authorName,
		AuthorEmail:                 authorEmail,
		Agent:                       meta.Agent,
		Model:                       meta.Model,
		TurnID:                      meta.TurnID,
		IsTask:                      meta.IsTask,
		ToolUseID:                   meta.ToolUseID,
		TranscriptIdentifierAtStart: meta.TranscriptIdentifierAtStart,
		CheckpointTranscriptStart:   meta.GetTranscriptStart(),
		TokenUsage:                  meta.TokenUsage,
		SessionMetrics:              meta.SessionMetrics,
		InitialAttribution:          meta.InitialAttribution,
		PromptAttributionsJSON:      meta.PromptAttributions,
		CombinedAttribution:         summary.CombinedAttribution,
		Summary:                     meta.Summary,
		Kind:                        meta.Kind,
		ReviewSkills:                meta.ReviewSkills,
		ReviewPrompt:                meta.ReviewPrompt,
		HasReview:                   session.Kind(meta.Kind).IsReview(),
	}
}

func writeMigrationReport(w io.Writer, report migrationReport, applied bool) {
	if applied {
		fmt.Fprintln(w, "Migration result:")
	} else {
		fmt.Fprintln(w, "Migration plan:")
	}
	fmt.Fprintf(w, "  discovered checkpoints: %d\n", report.DiscoveredCheckpoints)
	fmt.Fprintf(w, "  already present v1 sessions: %d\n", report.ExistingV1Sessions)
	fmt.Fprintf(w, "  missing v2 checkpoint metadata: %d\n", report.MissingV2CheckpointMetadata)
	fmt.Fprintf(w, "  missing required v2 session metadata: %d\n", report.MissingV2SessionMetadata)
	fmt.Fprintf(w, "  missing raw transcripts: %d\n", report.MissingRawTranscripts)
	fmt.Fprintf(w, "  checkpoints eligible for migration: %d\n", report.EligibleCheckpoints)
	fmt.Fprintf(w, "  sessions eligible for migration: %d\n", report.EligibleSessions)
	if applied {
		fmt.Fprintf(w, "  migrated checkpoints: %d\n", report.MigratedCheckpoints)
		fmt.Fprintf(w, "  migrated sessions: %d\n", report.MigratedSessions)
	}
}
