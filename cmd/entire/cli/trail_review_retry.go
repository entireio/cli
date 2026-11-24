package cli

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// Finding client IDs are unique only within a review. Reuse the review as well
// as the finding on retries, using the server's actor/change-scoped 24-hour
// Idempotency-Key protocol. Hash IDs, not bodies: changed content with a reused
// ID must hit the existing review's conflict check rather than create new work.
func trailReviewStartKey(inputs []api.TrailReviewCommentInput) (string, error) {
	ids := make([]string, len(inputs))
	for i, input := range inputs {
		if input.ClientID == "" {
			return "", errors.New("finding client ID must not be empty")
		}
		ids[i] = input.ClientID
	}
	sort.Strings(ids)
	encoded, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("encode finding retry key: %w", err)
	}
	// Bounded, header-safe, and unambiguous even for IDs containing separators.
	return fmt.Sprintf("entire-cli:findings:%x", sha256.Sum256(encoded)), nil
}
