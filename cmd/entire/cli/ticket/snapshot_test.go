package ticket

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiffSnapshot(t *testing.T) {
	t.Parallel()

	base := &Task{Title: "Rate limit /export", State: StateTodo, Intent: "body", Acceptance: "429"}
	prior := snapshotOf(base)

	// No prior snapshot → no changes.
	assert.Nil(t, diffSnapshot(nil, base))

	// Identical → no changes.
	assert.Nil(t, diffSnapshot(prior, base))

	// State change is reported specifically.
	ch := diffSnapshot(prior, &Task{Title: "Rate limit /export", State: StateInReview, Intent: "body", Acceptance: "429"})
	require.NotEmpty(t, ch)
	assert.Contains(t, strings.Join(ch, "; "), "state")

	// Title change.
	ch = diffSnapshot(prior, &Task{Title: "New title", State: StateTodo, Intent: "body", Acceptance: "429"})
	assert.Contains(t, strings.Join(ch, "; "), "title")

	// Body-only change falls back to "details updated".
	ch = diffSnapshot(prior, &Task{Title: "Rate limit /export", State: StateTodo, Intent: "different body", Acceptance: "429"})
	assert.Contains(t, strings.Join(ch, "; "), "details")
}
