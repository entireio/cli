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
	DiscoveredChecks      int
	ExistingV1Checkpoints int
	MissingV2Metadata     int
	MissingRawTranscripts int
	PlannedCheckpoints    int
	PlannedSessions       int
	MigratedCheckpoints   int
	MigratedSessions      int
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
	report := migrationReport{DiscoveredChecks: len(discovered)}
	migrator := checkpointMigrator{
		v1Store:     checkpoint.NewGitStore(repo),
		v2Store:     checkpoint.NewV2GitStore(repo),
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
		if migratedSessions > 0 {
			report.PlannedCheckpoints++
			if opts.apply {
				report.MigratedCheckpoints++
			}
		}
	}
	return report, nil
}

func (m checkpointMigrator) migrateCheckpoint(ctx context.Context, discovered discoveredCheckpoint) (int, error) {
	existing, err := m.v1Store.ReadCommitted(ctx, discovered.ID)
	if err != nil {
		return 0, fmt.Errorf("read v1 checkpoint %s: %w", discovered.ID, err)
	}
	if existing != nil {
		m.report.ExistingV1Checkpoints++
		return 0, nil
	}

	summary, err := m.v2Store.ReadCommitted(ctx, discovered.ID)
	if err != nil {
		return 0, fmt.Errorf("read v2 checkpoint %s: %w", discovered.ID, err)
	}
	if summary == nil || len(summary.Sessions) == 0 {
		m.report.MissingV2Metadata++
		return 0, nil
	}

	migratedSessions := 0
	for sessionIndex := range summary.Sessions {
		content, err := m.v2Store.ReadSessionContent(ctx, discovered.ID, sessionIndex)
		if err != nil {
			if errors.Is(err, checkpoint.ErrNoTranscript) {
				m.report.MissingRawTranscripts++
				continue
			}
			return migratedSessions, fmt.Errorf("read v2 checkpoint %s session %d: %w", discovered.ID, sessionIndex, err)
		}
		if !hasRequiredV2Metadata(content) {
			m.report.MissingV2Metadata++
			continue
		}

		m.report.PlannedSessions++
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
		TranscriptIdentifierAtStart: meta.TranscriptIdentifierAtStart,
		CheckpointTranscriptStart:   0,
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
	fmt.Fprintf(w, "  discovered checkpoint trailers: %d\n", report.DiscoveredChecks)
	fmt.Fprintf(w, "  already present in v1: %d\n", report.ExistingV1Checkpoints)
	fmt.Fprintf(w, "  missing v2 metadata: %d\n", report.MissingV2Metadata)
	fmt.Fprintf(w, "  missing raw transcripts: %d\n", report.MissingRawTranscripts)
	fmt.Fprintf(w, "  checkpoints with raw transcripts: %d\n", report.PlannedCheckpoints)
	fmt.Fprintf(w, "  sessions with raw transcripts: %d\n", report.PlannedSessions)
	if applied {
		fmt.Fprintf(w, "  migrated checkpoints: %d\n", report.MigratedCheckpoints)
		fmt.Fprintf(w, "  migrated sessions: %d\n", report.MigratedSessions)
	}
}
