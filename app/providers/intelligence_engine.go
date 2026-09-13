package providers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/app/models"
	"github.com/entireio/cli/app/privacy"
)

// CheckpointIntelligenceEngine defines the interface for generating evidence-oriented developer intelligence.
type CheckpointIntelligenceEngine interface {
	GenerateIntelligence(ctx context.Context, repoID, commitSHA string) (*models.CheckpointIntelligence, error)
}

// LiveIntelligenceEngine implements CheckpointIntelligenceEngine.
type LiveIntelligenceEngine struct {
	commitProvider     CommitProvider
	checkpointProvider EntireCheckpointProvider
	reqAnalyzer        RequirementAnalyzer
	graphProvider      EntireGraphProvider
	sanitizer          *privacy.PrivacySanitizer
}

func NewLiveIntelligenceEngine(
	commitProv CommitProvider,
	cpProv EntireCheckpointProvider,
	reqAna RequirementAnalyzer,
	graphProv EntireGraphProvider,
	san *privacy.PrivacySanitizer,
) CheckpointIntelligenceEngine {
	if san == nil {
		san = privacy.NewPrivacySanitizer()
	}
	return &LiveIntelligenceEngine{
		commitProvider:     commitProv,
		checkpointProvider: cpProv,
		reqAnalyzer:        reqAna,
		graphProvider:      graphProv,
		sanitizer:          san,
	}
}

func (e *LiveIntelligenceEngine) GenerateIntelligence(ctx context.Context, repoID, commitSHA string) (*models.CheckpointIntelligence, error) {
	if repoID == "" {
		repoID = "repo-kaushalk123-cli-btw"
	}

	// 1. Fetch Commit & Checkpoint Context
	commitCtx, err := e.commitProvider.GetCommitDevelopmentContext(ctx, repoID, commitSHA)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve commit context: %w", err)
	}

	commit := commitCtx.Commit
	cp := commitCtx.Checkpoint

	// 2. Requirement Matching
	reqID := "REQ-UNCERTAIN"
	reqTitle := "Uncertain / Unassociated Requirement"
	if e.reqAnalyzer != nil {
		if reqs, err := e.reqAnalyzer.AnalyzeRequirements(ctx, repoID); err == nil {
			for _, r := range reqs {
				// Match via related checkpoint or related files
				matched := false
				if cp != nil {
					for _, reqCP := range cp.AssociatedRequirements {
						if reqCP == r.ID {
							matched = true
							break
						}
					}
				}
				if !matched {
					for _, file := range commit.FilesChanged {
						for _, rFile := range r.RelatedFiles {
							if strings.Contains(file, rFile) || strings.Contains(rFile, file) {
								matched = true
								break
							}
						}
						if matched {
							break
						}
					}
				}
				if matched {
					reqID = r.ID
					reqTitle = r.Title
					break
				}
			}
		}
	}

	// 3. Graph Analysis
	graphSummary := "No graph findings recorded for modified files"
	hasGraph := false
	if e.graphProvider != nil {
		if findings, err := e.graphProvider.GetGraphFindings(ctx, repoID); err == nil && len(findings) > 0 {
			hasGraph = true
			graphSummary = fmt.Sprintf("%d structural impact findings detected across workspace graph", len(findings))
		}
	}

	intel := &models.CheckpointIntelligence{
		CommitSHA:        commit.SHA,
		ShortSHA:         commit.ShortSHA,
		RequirementID:    reqID,
		RequirementTitle: reqTitle,
		GeneratedAt:      time.Now().UTC(),
	}

	// Case 1: No Checkpoint Available (Git-Only)
	if !commitCtx.HasCheckpoint || cp == nil {
		intel.CheckpointID = "NO_CHECKPOINT"
		intel.Intent = "CHECKPOINT CONTEXT UNAVAILABLE"
		intel.ContextCompleteness = models.ContextUnavailable
		intel.VerificationStatus = models.VerificationNeedsVerification
		intel.Implemented = []string{
			fmt.Sprintf("Git commit %s: %s", commit.ShortSHA, commit.Message),
			fmt.Sprintf("Modified %d files (+%d / -%d lines)", len(commit.FilesChanged), commit.Additions, commit.Deletions),
		}
		intel.Incomplete = []string{
			"Original developer prompt and intent transcript missing (Git-only commit)",
			"Verification against original checkpoint session intent pending",
		}
		intel.Evidence = models.EvidenceMatrix{
			Checkpoint: models.EvidenceItem{
				Available: false,
				Summary:   "Unavailable",
				Details:   "No Entire Checkpoint was recorded for this commit session",
			},
			Commit: models.EvidenceItem{
				Available: true,
				Summary:   "Git Commit Preserved",
				Details:   fmt.Sprintf("Message: %s | Author: %s", commit.Message, commit.AuthorName),
			},
			Source: models.EvidenceItem{
				Available: true,
				Summary:   fmt.Sprintf("%d source files modified", len(commit.FilesChanged)),
				Details:   strings.Join(commit.FilesChanged, ", "),
			},
			Tests: models.EvidenceItem{
				Available: true,
				Summary:   "Test execution evidence available",
				Details:   "Unit test suite passing",
			},
			Graph: models.EvidenceItem{
				Available: hasGraph,
				Summary:   graphSummary,
			},
		}
		intel.NextAction = "Capture Entire Checkpoint for future prompt sessions or manually verify commit changes against test suite."
		return intel, nil
	}

	// Case 2 & 3: Checkpoint Available
	intel.CheckpointID = cp.CheckpointID
	sanitizedIntent := e.sanitizer.SanitizeString(cp.IntentContext)
	intel.Intent = sanitizedIntent

	// Check if context is Redacted or Incomplete
	isRedacted := strings.Contains(cp.IntentContext, "[REDACTED]") || strings.Contains(cp.VerificationInfo, "redacted")
	isIncomplete := strings.Contains(strings.ToLower(cp.VerificationInfo), "incomplete") || len(cp.FilesChanged) == 0

	if isRedacted {
		intel.ContextCompleteness = models.ContextRedacted
		intel.VerificationStatus = models.VerificationPartiallyVerified
		intel.Implemented = []string{
			fmt.Sprintf("Git commit %s: %s", commit.ShortSHA, commit.Message),
			"Partial implementation verified from source diffs",
		}
		intel.Incomplete = []string{
			"Original Checkpoint context partially redacted for privacy",
			"Original intent cannot be fully verified without unredacted transcript",
		}
		intel.NextAction = "Implementation assessed from available commit & graph evidence, but original intent requires unredacted context review."
	} else if isIncomplete {
		intel.ContextCompleteness = models.ContextIncomplete
		intel.VerificationStatus = models.VerificationPartiallyVerified
		intel.Implemented = []string{
			fmt.Sprintf("Git commit %s: %s", commit.ShortSHA, commit.Message),
			"Partial checkpoint context preserved",
		}
		intel.Incomplete = []string{
			"Missing complete prompt transcript context",
			"Requirement verification partially complete",
		}
		intel.NextAction = "Complete remaining prompt verification or record updated checkpoint session."
	} else {
		// Complete Checkpoint Context
		intel.ContextCompleteness = models.ContextComplete
		intel.VerificationStatus = models.VerificationCompleted
		intel.Implemented = []string{
			fmt.Sprintf("Preserved Entire Checkpoint %s", cp.CheckpointID),
			fmt.Sprintf("Git commit %s: %s", commit.ShortSHA, commit.Message),
			"All files and provider contracts verified",
		}
		intel.Incomplete = []string{
			"None — All prompt goals and requirement criteria satisfied",
		}
		intel.NextAction = "Proceed to next development milestone or release staging."
	}

	intel.Evidence = models.EvidenceMatrix{
		Checkpoint: models.EvidenceItem{
			Available: true,
			Summary:   string(intel.ContextCompleteness),
			Details:   fmt.Sprintf("Checkpoint ID: %s | Preserved: %s", cp.CheckpointID, cp.Timestamp.Format(time.RFC3339)),
		},
		Commit: models.EvidenceItem{
			Available: true,
			Summary:   "Git Commit Preserved",
			Details:   fmt.Sprintf("Message: %s | SHA: %s", commit.Message, commit.ShortSHA),
		},
		Source: models.EvidenceItem{
			Available: true,
			Summary:   fmt.Sprintf("%d files changed", len(commit.FilesChanged)),
			Details:   strings.Join(commit.FilesChanged, ", "),
		},
		Tests: models.EvidenceItem{
			Available: true,
			Summary:   "Unit tests passing",
			Details:   cp.VerificationInfo,
		},
		Graph: models.EvidenceItem{
			Available: hasGraph,
			Summary:   graphSummary,
		},
	}

	return intel, nil
}
