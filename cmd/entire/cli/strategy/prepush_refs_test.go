package strategy

import (
	"context"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

const (
	testSHA1 = "1111111111111111111111111111111111111111"
	testSHA2 = "2222222222222222222222222222222222222222"
	zeroSHA  = "0000000000000000000000000000000000000000"
)

func TestParsePrePushRefs(t *testing.T) {
	t.Parallel()
	refs, err := ParsePrePushRefs(strings.NewReader(
		"refs/heads/main " + testSHA1 + " refs/heads/main " + testSHA2 + "\n" +
			"malformed line\n" +
			"\n" +
			"(delete) " + zeroSHA + " refs/heads/old " + testSHA2 + "\n"))
	require.NoError(t, err)
	require.Equal(t, []PrePushRef{
		{LocalRef: "refs/heads/main", LocalSHA: testSHA1, RemoteRef: "refs/heads/main", RemoteSHA: testSHA2},
		{LocalRef: "(delete)", LocalSHA: zeroSHA, RemoteRef: "refs/heads/old", RemoteSHA: testSHA2},
	}, refs)
}

func TestCheckOuterPushV1(t *testing.T) {
	t.Parallel()
	const v1 = "refs/heads/entire/checkpoints/v1"
	verified := plumbing.NewHash(testSHA1)
	for _, tc := range []struct {
		name     string
		refs     []PrePushRef
		verified plumbing.Hash
		wantErr  bool
	}{
		{name: "no ref list (old hook script)", refs: nil, verified: plumbing.ZeroHash},
		{name: "push without v1", refs: []PrePushRef{{"refs/heads/main", testSHA2, "refs/heads/main", zeroSHA}}, verified: plumbing.ZeroHash},
		{name: "v1 at the verified tip", refs: []PrePushRef{{v1, testSHA1, v1, zeroSHA}}, verified: verified},
		{name: "v1 at a pre-rewrite commit", refs: []PrePushRef{{v1, testSHA2, v1, zeroSHA}}, verified: verified, wantErr: true},
		{name: "v1 while held for the scan", refs: []PrePushRef{{v1, testSHA1, v1, zeroSHA}}, verified: plumbing.ZeroHash, wantErr: true},
		{name: "v1 pushed under another name", refs: []PrePushRef{{v1, testSHA2, "refs/heads/backup", zeroSHA}}, verified: verified, wantErr: true},
		{name: "another branch pushed onto v1", refs: []PrePushRef{{"refs/heads/main", testSHA2, v1, zeroSHA}}, verified: verified, wantErr: true},
		{name: "v1 the remote already has", refs: []PrePushRef{{v1, testSHA2, v1, testSHA2}}, verified: verified},
		{name: "deleting remote v1", refs: []PrePushRef{{"(delete)", zeroSHA, v1, zeroSHA}}, verified: plumbing.ZeroHash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tc.refs != nil {
				ctx = WithPrePushRefs(ctx, tc.refs)
			}
			err := checkOuterPushV1(ctx, tc.verified)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrOuterPushCarriesUnverifiedV1)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
