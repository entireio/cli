package strategy

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func TestStampedTrailer_SkipsInheritedTrailers(t *testing.T) {
	t.Parallel()
	inherited := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J30")
	fresh := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J31")
	msg := "Squashed\n\nEntire-Checkpoint: " + inherited.String() + "\nEntire-Checkpoint: " + fresh.String() + "\n"

	got, ok := stampedTrailer(msg, []id.CheckpointID{inherited})
	assert.True(t, ok)
	assert.Equal(t, fresh, got)

	_, ok = stampedTrailer("Squashed\n\nEntire-Checkpoint: "+inherited.String()+"\n", []id.CheckpointID{inherited})
	assert.False(t, ok, "an inherited trailer alone is not a stamp")

	got, ok = stampedTrailer(msg, nil)
	assert.True(t, ok)
	assert.Equal(t, inherited, got, "with nothing inherited the first trailer is the stamp, as before")
}

func TestPickCondensationTarget(t *testing.T) {
	t.Parallel()
	a, b, c := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3A"), id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3B"), id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3C")
	existing := map[id.CheckpointID]bool{a: true, b: true}
	exists := func(cpID id.CheckpointID) bool { return existing[cpID] }

	got, ok := pickCondensationTarget([]id.CheckpointID{a}, nil)
	assert.True(t, ok)
	assert.Equal(t, a, got, "a lone trailer is the target without consulting the store")

	got, ok = pickCondensationTarget([]id.CheckpointID{b, a, c}, exists)
	assert.True(t, ok)
	assert.Equal(t, c, got, "the stamped trailer is the one without a checkpoint")

	_, ok = pickCondensationTarget([]id.CheckpointID{b, a}, exists)
	assert.False(t, ok, "only links: nothing to condense into")

	_, ok = pickCondensationTarget(nil, exists)
	assert.False(t, ok)
}
