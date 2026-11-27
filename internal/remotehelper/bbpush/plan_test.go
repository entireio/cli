package bbpush

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseRefspec(t *testing.T) {
	t.Parallel()
	r, err := parseRefspec("+refs/heads/a:refs/heads/b")
	require.NoError(t, err)
	require.Equal(t, refspec{force: true, src: "refs/heads/a", dst: "refs/heads/b"}, r)
	r, err = parseRefspec(":refs/heads/b")
	require.NoError(t, err)
	require.Equal(t, refspec{dst: "refs/heads/b"}, r)
	for _, bad := range []string{"refs/heads/a", "a:b", "refs/heads/*:refs/heads/*", "-x:refs/heads/b", "a:refs/heads/b c"} {
		_, err := parseRefspec(bad)
		require.Error(t, err, bad)
	}
}

func TestParseCAS(t *testing.T) {
	t.Parallel()
	leases, err := parseCAS([]string{"refs/heads/main:" + oidA}, 40)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"refs/heads/main": oidA}, leases)
	for _, bad := range []string{"refs/heads/main", "main:" + oidA, "refs/heads/main:" + oidA[:7], "refs/heads/main:" + strings.Repeat("g", 40)} {
		_, err := parseCAS([]string{bad}, 40)
		require.Error(t, err, bad)
	}
}

func TestRequestCaps(t *testing.T) {
	t.Parallel()
	repo := &localRepo{objectFormat: "sha1", oidHexLen: 40, agent: "git/2.50"}
	full := &advertisement{caps: map[string]string{
		"report-status": "", "side-band-64k": "", "quiet": "", "atomic": "", "push-options": "",
		"object-format": "sha1", "agent": "x", "ofs-delta": "",
	}}
	caps, err := requestCaps(full, repo, Options{Atomic: true, PushOptions: []string{"o"}}, true)
	require.NoError(t, err)
	require.Equal(t, " report-status side-band-64k quiet atomic push-options object-format=sha1 agent=git/2.50 entire-bb", caps)

	bare := &advertisement{caps: map[string]string{"report-status": ""}}
	caps, err = requestCaps(bare, repo, Options{}, true)
	require.NoError(t, err)
	require.Equal(t, " report-status entire-bb", caps)

	for name, tc := range map[string]struct {
		adv  *advertisement
		repo *localRepo
		opts Options
	}{
		"no report-status": {&advertisement{caps: map[string]string{}}, repo, Options{}},
		"atomic":           {bare, repo, Options{Atomic: true}},
		"push options":     {bare, repo, Options{PushOptions: []string{"o"}}},
		"object format":    {bare, &localRepo{objectFormat: "sha256", oidHexLen: 64}, Options{}},
	} {
		_, err := requestCaps(tc.adv, tc.repo, tc.opts, false)
		require.Error(t, err, name)
	}
}

// A push signed "if-asked" is signed only when the remote advertises
// push-cert. Only send-pack signs, so that remote takes send-pack.
func TestRequestCapsPushCertIfAsked(t *testing.T) {
	t.Parallel()
	repo := &localRepo{objectFormat: "sha1", oidHexLen: 40, agent: "git/2.50"}
	opts := Options{PushCertIfAsked: true}
	caps, err := requestCaps(&advertisement{caps: map[string]string{"report-status": ""}}, repo, opts, false)
	require.NoError(t, err)
	require.Equal(t, " report-status entire-bb", caps)

	_, err = requestCaps(&advertisement{caps: map[string]string{"report-status": "", "push-cert": "nonce"}}, repo, opts, false)
	require.ErrorIs(t, err, ErrNeedsSendPack)
}
