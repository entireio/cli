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

	// Preference order: the local summary title, else the commit subject.
	sources := []struct {
		text   func(candidate) string
		source string
	}{
		{func(c candidate) string { return c.LocalSummaryTitle }, bulletSourceLocalSummary},
		{func(c candidate) string { return c.CommitSubject }, bulletSourceCommitMessage},
	}

	for _, cand := range candidates {
		for _, s := range sources {
			text := strings.TrimSpace(s.text(cand))
			if text == "" {
				continue
			}
			result.Used = append(result.Used, repoBullet{
				RepoFullName: cand.RepoFullName,
				Bullet: Bullet{
					CheckpointID: cand.CheckpointID,
					Text:         text,
					Source:       s.source,
					Branch:       cand.Branch,
					CreatedAt:    cand.CreatedAt,
				},
			})
			break
		}
	}

	return result
}
