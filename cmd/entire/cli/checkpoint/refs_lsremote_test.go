package checkpoint

import (
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	lsRemoteTestSHA   = "e9ed0bd3ad3b2071aefab6e6ad20527dc910957b"
	lsRemoteTestSHA2  = "0123456789abcdef0123456789abcdef01234567"
	lsRemoteTestRefZN = "refs/entire/checkpoints/ZN/01KVBJCWYA4YW6J5M9GP655HZN"
	lsRemoteTestRefF6 = "refs/entire/checkpoints/f6/a1b2c3d4e5f6"
)

func lsRemoteTestOutput() []byte {
	return []byte(strings.Join([]string{
		lsRemoteTestSHA + "\tHEAD",
		lsRemoteTestSHA + "\trefs/heads/main",
		lsRemoteTestSHA + "\trefs/heads/entire/checkpoints/v1",
		lsRemoteTestSHA + "\t" + lsRemoteTestRefZN,
		lsRemoteTestSHA2 + "\t" + lsRemoteTestRefF6,
		lsRemoteTestSHA + "\trefs/tags/v1.0.0",
		lsRemoteTestSHA + "\trefs/tags/v1.0.0^{}",
		"not-a-hash\trefs/entire/checkpoints/aa/deadbeefdead",
		"",
	}, "\n"))
}

func TestParseLsRemoteRefs(t *testing.T) {
	t.Parallel()

	refs := ParseLsRemoteRefs(lsRemoteTestOutput())
	require.Len(t, refs, 2, "HEAD, branches, tags and the malformed-hash line drop out")
	assert.Equal(t, plumbing.NewHash(lsRemoteTestSHA), refs[plumbing.ReferenceName(lsRemoteTestRefZN)])
	assert.Equal(t, plumbing.NewHash(lsRemoteTestSHA2), refs[plumbing.ReferenceName(lsRemoteTestRefF6)])

	assert.Empty(t, ParseLsRemoteRefs(nil))
}

func TestParseCheckpointRefNames(t *testing.T) {
	t.Parallel()

	names := ParseCheckpointRefNames(lsRemoteTestOutput())
	assert.Equal(t, []plumbing.ReferenceName{
		plumbing.ReferenceName(lsRemoteTestRefZN),
		plumbing.ReferenceName(lsRemoteTestRefF6),
		plumbing.ReferenceName("refs/entire/checkpoints/aa/deadbeefdead"),
	}, names, "names keep ls-remote order and do not validate the hash column")

	assert.Empty(t, ParseCheckpointRefNames(nil))
}
