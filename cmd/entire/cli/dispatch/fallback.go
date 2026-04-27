package dispatch

import (
	"strings"
	"time"
)

const (
	bulletSourceLocalSummary  = "local_summary"
	bulletSourceCommitMessage = "commit_message"
)

type candidate struct {
	CheckpointID      string
	RepoFullName      string
	Branch            string
	CreatedAt         time.Time
	CommitSubject     string
	LocalSummaryTitle string
}

type repoBullet struct {
	RepoFullName string
	Bullet       Bullet
}

type fallbackResult struct {
	Used []repoBullet
}

func applyFallbackChain(candidates []candidate) fallbackResult {
	result := fallbackResult{Used: make([]repoBullet, 0, len(candidates))}

	for _, candidate := range candidates {
		if text := strings.TrimSpace(candidate.LocalSummaryTitle); text != "" {
			result.Used = append(result.Used, repoBullet{
				RepoFullName: candidate.RepoFullName,
				Bullet: Bullet{
					CheckpointID: candidate.CheckpointID,
					Text:         text,
					Source:       bulletSourceLocalSummary,
					Branch:       candidate.Branch,
					CreatedAt:    candidate.CreatedAt,
				},
			})
			continue
		}

		if text := strings.TrimSpace(candidate.CommitSubject); text != "" {
			result.Used = append(result.Used, repoBullet{
				RepoFullName: candidate.RepoFullName,
				Bullet: Bullet{
					CheckpointID: candidate.CheckpointID,
					Text:         text,
					Source:       bulletSourceCommitMessage,
					Branch:       candidate.Branch,
					CreatedAt:    candidate.CreatedAt,
				},
			})
			continue
		}
	}

	return result
}
