package gitproto

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHasV2FetchFeature(t *testing.T) {
	t.Parallel()
	adv := pkt("version 2\n") + pkt("ls-refs=unborn\n") + pkt("fetch=shallow entire-bb-fetch filter\n") + "0000"
	require.True(t, HasV2FetchFeature([]byte(adv), "entire-bb-fetch"))
	require.True(t, HasV2FetchFeature([]byte(adv), "filter"))
	require.False(t, HasV2FetchFeature([]byte(adv), "entire-bb"))
	require.False(t, HasV2FetchFeature([]byte(adv), "unborn"), "a feature of another capability")
	require.False(t, HasV2FetchFeature([]byte(pkt("version 2\n")+pkt("fetch\n")+"0000"), "entire-bb-fetch"))
	require.False(t, HasV2FetchFeature([]byte("zzzz"), "entire-bb-fetch"))
}

func TestParseV2Request(t *testing.T) {
	t.Parallel()
	msg := pkt("command=fetch\n") + pkt("object-format=sha256\n") + "0001" +
		pkt("thin-pack\n") + pkt("want 1234\n") + pkt("sideband-all") + "0000"
	req, err := ParseV2Request([]byte(msg))
	require.NoError(t, err)
	require.Equal(t, []string{"command=fetch", "object-format=sha256"}, req.Capabilities)
	require.Equal(t, []string{"thin-pack", "want 1234", "sideband-all"}, req.Arguments)
	format, ok := req.Capability("object-format")
	require.True(t, ok)
	require.Equal(t, "sha256", format)
	_, ok = req.Capability("agent")
	require.False(t, ok)
	require.True(t, req.HasArgument("sideband-all"))
	require.False(t, req.HasArgument("want"))

	for name, bad := range map[string]string{
		"no flush":          pkt("command=fetch\n"),
		"data after flush":  pkt("command=fetch\n") + "0000" + pkt("x"),
		"two delims":        pkt("command=fetch\n") + "0001" + "0001" + "0000",
		"response end":      pkt("command=fetch\n") + "0002" + "0000",
		"truncated packet":  pkt("command=fetch\n")[:6],
		"bad packet length": "zzzz0000",
	} {
		_, err := ParseV2Request([]byte(bad))
		require.Error(t, err, name)
	}
}

func TestAddV2Argument(t *testing.T) {
	t.Parallel()
	withArgs := pkt("command=fetch\n") + pkt("agent=git/2.50\n") + "0001" + pkt("want 1234\n") + pkt("done\n") + "0000"
	got, err := AddV2Argument([]byte(withArgs), "entire-bb-fetch")
	require.NoError(t, err)
	require.Equal(t, withArgs[:len(withArgs)-4]+pkt("entire-bb-fetch\n")+"0000", string(got))

	noArgs := pkt("command=fetch\n") + "0000"
	got, err = AddV2Argument([]byte(noArgs), "entire-bb-fetch")
	require.NoError(t, err)
	require.Equal(t, pkt("command=fetch\n")+"0001"+pkt("entire-bb-fetch\n")+"0000", string(got))
	req, err := ParseV2Request(got)
	require.NoError(t, err)
	require.True(t, req.HasArgument("entire-bb-fetch"))

	_, err = AddV2Argument([]byte(pkt("command=fetch\n")), "entire-bb-fetch")
	require.Error(t, err)
}
