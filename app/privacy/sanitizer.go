package privacy

import (
	"regexp"

	"github.com/entireio/cli/app/models"
)

var (
	// Common secret / credential patterns
	tokenRegex  = regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|auth|bearer)\s*[:=]\s*["']?([a-zA-Z0-9_\-\.]{8,})["']?`)
	githubToken = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{36,}`)
)

// PrivacySanitizer redacts PII, secrets, and raw prompt transcripts from domain models before external transmission.
type PrivacySanitizer struct{}

func NewPrivacySanitizer() *PrivacySanitizer {
	return &PrivacySanitizer{}
}

// SanitizeString removes sensitive patterns from raw text.
func (s *PrivacySanitizer) SanitizeString(input string) string {
	if input == "" {
		return input
	}
	res := githubToken.ReplaceAllString(input, "[REDACTED_GITHUB_TOKEN]")
	res = tokenRegex.ReplaceAllString(res, "$1: [REDACTED_SECRET]")
	return res
}

// SanitizeCheckpoint redacts raw prompt text and credentials in a Checkpoint model.
func (s *PrivacySanitizer) SanitizeCheckpoint(cp *models.Checkpoint) *models.Checkpoint {
	if cp == nil {
		return nil
	}
	sanitized := *cp
	sanitized.IntentContext = s.SanitizeString(cp.IntentContext)
	sanitized.VerificationInfo = s.SanitizeString(cp.VerificationInfo)
	return &sanitized
}

// SanitizeCheckpoints redacts a list of Checkpoint models.
func (s *PrivacySanitizer) SanitizeCheckpoints(cps []models.Checkpoint) []models.Checkpoint {
	if cps == nil {
		return nil
	}
	sanitized := make([]models.Checkpoint, len(cps))
	for i, cp := range cps {
		sanitized[i] = *s.SanitizeCheckpoint(&cp)
	}
	return sanitized
}
