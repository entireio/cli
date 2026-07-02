package ticket

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSlugify(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "add-rate-limiting", slugify("Add Rate Limiting!"))
	assert.Equal(t, "eng-142", slugify("ENG-142"))
	assert.Empty(t, slugify("  --  "))
}

func TestDefaultBranchName_UsesProviderSuggestion(t *testing.T) {
	t.Parallel()

	// When the provider supplies a branch name, it is used verbatim (no git
	// calls needed), so this is deterministic.
	name := defaultBranchName(
		context.Background(),
		&Task{BranchName: "amy/eng-142-rate-limit"},
		Link{ID: "ENG-142"},
	)
	assert.Equal(t, "amy/eng-142-rate-limit", name)
}
