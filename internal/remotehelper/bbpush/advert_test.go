package bbpush

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseAdvertisement(t *testing.T) {
	t.Parallel()
	adv, err := parseAdvertisement(pkts(
		oidA+" refs/heads/main\x00report-status delete-refs side-band-64k entire-bb=65536 agent=git/2.50\n",
		oidB+" refs/heads/other\n",
		oidB+" .have\n",
		"",
	))
	require.NoError(t, err)
	require.Equal(t, []advertisedRef{{"refs/heads/main", oidA}, {"refs/heads/other", oidB}}, adv.refs)
	require.Equal(t, []string{oidB}, adv.have)
	require.True(t, adv.supports("delete-refs"))
	require.False(t, adv.supports("atomic"))
	require.Equal(t, oidA, adv.oldOID("refs/heads/main"))
	require.Empty(t, adv.oldOID("refs/heads/new"))
	n, ok := adv.threshold()
	require.True(t, ok)
	require.Equal(t, int64(65536), n)
}

func TestParseAdvertisementEmptyRepo(t *testing.T) {
	t.Parallel()
	adv, err := parseAdvertisement(pkts(zero+" capabilities^{}\x00report-status entire-bb=1024\n", ""))
	require.NoError(t, err)
	require.Empty(t, adv.refs)
	n, ok := Threshold(pkts(zero+" capabilities^{}\x00report-status entire-bb=1024\n", ""))
	require.True(t, ok)
	require.Equal(t, int64(1024), n)
}

func TestThresholdNeedsAPositiveValue(t *testing.T) {
	t.Parallel()
	for _, caps := range []string{"report-status", "entire-bb", "entire-bb=0", "entire-bb=-5", "entire-bb=x"} {
		_, ok := Threshold(pkts(zero+" capabilities^{}\x00"+caps+"\n", ""))
		require.False(t, ok, caps)
	}
	_, ok := Threshold([]byte("not pkt-lines"))
	require.False(t, ok)
}

// A ref name with a space or a control byte would reach the status lines
// that git reads, so the advertisement drops it.
func TestParseAdvertisementSkipsOddRefNames(t *testing.T) {
	t.Parallel()
	adv, err := parseAdvertisement(pkts(
		oidA+" refs/heads/main\x00report-status\n",
		oidB+" refs/heads/a b\n",
		oidB+" refs/heads/esc\x1b[2J\n",
		oidB+" refs/heads/ok\n",
		"",
	))
	require.NoError(t, err)
	require.Equal(t, []advertisedRef{{"refs/heads/main", oidA}, {"refs/heads/ok", oidB}}, adv.refs)
}
