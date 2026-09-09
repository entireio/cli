package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The topology note once told users to "set checkpoint_remote", which names a
// separate {provider, repo} checkpoint repository and silently ignores a remote
// name. These tests pin the corrected advice and the positive line an elected
// Entire remote earns instead of a warning.

func twoPlainRemotes() remoteTopology {
	return remoteTopology{destinations: []remoteDestination{
		{name: "fork", pushURLs: []string{"https://github.com/me/r"}},
		{name: "origin", pushURLs: []string{"https://github.com/o/r"}},
	}}
}

func TestDescribeCheckpointDestination_MultiRemoteNamesPushRemoteSetting(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	twoPlainRemotes().describeCheckpointDestination(&buf, "Header")
	got := buf.String()

	assert.Contains(t, got, "Header\n")
	assert.Contains(t, got, "This repo has 2 remotes (fork, origin).")
	assert.Contains(t, got, "run `entire checkpoint migrate --to <remote>`")
	assert.Contains(t, got, "strategy_options.checkpoint_push_remote in .entire/settings.local.json")
	assert.Contains(t, got, "see\n  checkpoint_remote in the README.")
	assert.NotContains(t, got, "set checkpoint_remote")
	assert.NotContains(t, got, "To pin one repository")
}

func TestDescribeCheckpointDestination_EntireElectedIsNotAmbiguous(t *testing.T) {
	t.Parallel()

	topo := remoteTopology{destinations: []remoteDestination{
		{name: "entire", pushURLs: []string{"entire://cluster.test/gh/o/r"}},
		{name: "origin", pushURLs: []string{"https://github.com/o/r"}},
	}, entireElected: "entire"}
	assert.False(t, topo.ambiguous())

	var buf bytes.Buffer
	topo.describeCheckpointDestination(&buf, "Header")
	got := buf.String()
	assert.Equal(t, "\n✓ Checkpoints sync to entire (your Entire remote).\n", got)
	assert.NotContains(t, got, "Header")
	assert.NotContains(t, got, "This repo has")
}

func TestDescribeCheckpointDestination_SingleRemoteSaysNothing(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	remoteTopology{destinations: []remoteDestination{{name: "origin", pushURLs: []string{"entire://c/gh/o/r"}}}, entireElected: "origin"}.
		describeCheckpointDestination(&buf, "Header")
	assert.Empty(t, buf.String(), "a sole Entire remote is the ordinary repo; enable already says where checkpoints go")

	buf.Reset()
	remoteTopology{destinations: []remoteDestination{{name: "origin", pushURLs: []string{"https://github.com/o/r"}}}}.
		describeCheckpointDestination(&buf, "Header")
	assert.Empty(t, buf.String())
}

func TestDescribeCheckpointDestination_FanOutStillReported(t *testing.T) {
	t.Parallel()

	topo := remoteTopology{destinations: []remoteDestination{
		{name: "entire", pushURLs: []string{"entire://cluster.test/gh/o/r"}},
		{name: "origin", pushURLs: []string{"https://github.com/o/r", "https://user:pw@mirror.example/o/r"}},
	}, entireElected: "entire", primaryIsRefs: true}
	assert.True(t, topo.ambiguous())

	var buf bytes.Buffer
	topo.describeCheckpointDestination(&buf, "Header")
	got := buf.String()
	assert.Contains(t, got, "Header\n")
	assert.Contains(t, got, `Remote "origin" pushes to 2 URLs:`)
	assert.Contains(t, got, "https://github.com/o/r")
	assert.NotContains(t, got, "→ ", "no first-URL marker: checkpoints do not go to any of these URLs")
	assert.Contains(t, got, "Your code goes to every URL; checkpoints do not fan out with it.")
	assert.NotContains(t, got, "Checkpoints go to the first URL only", "that is the non-Entire story")
	assert.Contains(t, got, "https://mirror.example/o/r", "credentials are redacted")
	assert.NotContains(t, got, "user:pw")
	assert.Contains(t, got, "  ✓ Checkpoints sync to entire (your Entire remote).\n")
	assert.NotContains(t, got, "This repo has 2 remotes", "the multi-remote choice is settled by the Entire remote")
	assert.NotContains(t, got, "checkpoint migrate --to")

	// Without the election the fan-out block is followed by the picker advice.
	topo.entireElected = ""
	buf.Reset()
	topo.describeCheckpointDestination(&buf, "Header")
	assert.Contains(t, buf.String(), "This repo has 2 remotes (entire, origin).")
	assert.Contains(t, buf.String(), "entire checkpoint migrate --to <remote>")
	assert.Contains(t, buf.String(), "→ https://github.com/o/r")
	assert.Contains(t, buf.String(), "Checkpoints go to the first URL only")
}

func TestRemoteTopology_PinnedRemoteDoesNotCount(t *testing.T) {
	t.Parallel()

	topo := twoPlainRemotes()
	topo.destinations[0].pinned = true
	assert.False(t, topo.ambiguous(), "a remote pinned to a dedicated checkpoint_remote is not a choice")
	assert.Equal(t, []string{"origin"}, topo.unpinnedNames())
}
