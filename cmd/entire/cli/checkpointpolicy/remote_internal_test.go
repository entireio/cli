package checkpointpolicy

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseRemotePolicyHash(t *testing.T) {
	t.Parallel()
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)

	got, err := parseRemotePolicyHash(sha1)
	require.NoError(t, err)
	require.Equal(t, sha1, got.String())

	got, err = parseRemotePolicyHash(sha256)
	require.NoError(t, err)
	require.Equal(t, sha256, got.String())

	_, err = parseRemotePolicyHash(strings.Repeat("c", 41))
	require.ErrorContains(t, err, "invalid remote checkpoint policy hash")

	_, err = parseRemotePolicyHash(strings.Repeat("g", 40))
	require.ErrorContains(t, err, "invalid remote checkpoint policy hash")
}
