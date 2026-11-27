package bbpush

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/stretchr/testify/require"
)

const (
	oidA = "1111111111111111111111111111111111111111"
	oidB = "2222222222222222222222222222222222222222"
	zero = "0000000000000000000000000000000000000000"
)

func pkts(lines ...string) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		if l == "" {
			_ = pktline.WriteFlush(&b)
			continue
		}
		if _, err := pktline.WriteString(&b, l); err != nil {
			panic(err)
		}
	}
	return b.Bytes()
}

func TestCommandSection(t *testing.T) {
	t.Parallel()
	cmds := []command{{ref: "refs/heads/main", old: oidA, new: oidB}, {ref: "refs/heads/gone", old: oidB, new: zero}}
	got, err := commandSection(cmds, " report-status entire-bb", nil)
	require.NoError(t, err)
	require.Equal(t, pkts(oidA+" "+oidB+" refs/heads/main\x00 report-status entire-bb", oidB+" "+zero+" refs/heads/gone", ""), got)

	got, err = commandSection(cmds[:1], " push-options", []string{"a", "b=c"})
	require.NoError(t, err)
	require.Equal(t, pkts(oidA+" "+oidB+" refs/heads/main\x00 push-options", "", "a", "b=c", ""), got)
}

func TestReadReport(t *testing.T) {
	t.Parallel()
	body := pkts("unpack ok\n", "ok refs/heads/main\n", "ng refs/heads/blocked hook declined\n", "ng refs/heads/bare\n", "")
	rep, err := readReport(iotest.OneByteReader(bytes.NewReader(body)), false, nil)
	require.NoError(t, err)
	require.Equal(t, "ok", rep.unpack)
	require.Equal(t, map[string]refResult{
		"refs/heads/main":    {ok: true},
		"refs/heads/blocked": {msg: "hook declined"},
		"refs/heads/bare":    {msg: "failed"},
	}, rep.refs)
}

func TestReadReportSideband(t *testing.T) {
	t.Parallel()
	report := pkts("unpack push rejected: missing xorb "+strings.Repeat("ab", 32)+"\n", "ng refs/heads/main unpacker error\n", "")
	var stderr bytes.Buffer
	body := pkts("\x02Resolving deltas: 50%\r", "\x01"+string(report[:10]), "\x02done\nnext", "\x01"+string(report[10:]), "")
	rep, err := readReport(bytes.NewReader(body), true, &stderr)
	require.NoError(t, err)
	require.Equal(t, "remote: Resolving deltas: 50%\rremote: done\nremote: next", stderr.String())
	require.True(t, rep.missingXorb())
	require.True(t, rep.failed([]command{{ref: "refs/heads/main"}}))

	_, err = readReport(bytes.NewReader(pkts("\x03fatal: disk full", "")), true, &stderr)
	require.ErrorContains(t, err, "remote error: fatal: disk full")
}

func TestReadReportRefusesBadStatus(t *testing.T) {
	t.Parallel()
	for name, body := range map[string][]byte{
		"empty":          nil,
		"flush first":    pkts(""),
		"no unpack":      pkts("ok refs/heads/main\n", ""),
		"unknown":        pkts("unpack ok\n", "maybe refs/heads/main\n", ""),
		"no flush":       pkts("unpack ok\n", "ok refs/heads/main\n"),
		"error packet":   pkts("ERR access denied"),
		"unknown band":   pkts("\x05x", ""),
		"empty sideband": nil,
	} {
		_, err := readReport(bytes.NewReader(body), name == "unknown band" || name == "empty sideband", &bytes.Buffer{})
		require.Error(t, err, name)
	}
}

func TestReportFailedAndMissingXorb(t *testing.T) {
	t.Parallel()
	cmds := []command{{ref: "refs/heads/main"}, {ref: "refs/heads/b"}}
	ok := &report{unpack: "ok", refs: map[string]refResult{"refs/heads/main": {ok: true}, "refs/heads/b": {ok: true}}}
	require.False(t, ok.failed(cmds))
	require.False(t, ok.missingXorb())
	noReport := &report{unpack: "ok", refs: map[string]refResult{"refs/heads/main": {ok: true}}}
	require.True(t, noReport.failed(cmds))
	ng := &report{unpack: "ok", refs: map[string]refResult{"refs/heads/main": {msg: "missing xorb " + strings.Repeat("0f", 32)}}}
	require.False(t, ng.missingXorb(), "a hook writes ref statuses; only the unpack status counts")
	unpack := &report{unpack: "push rejected: missing xorb " + strings.Repeat("0f", 32)}
	require.True(t, unpack.missingXorb())
	short := &report{unpack: "push rejected: missing xorb abc"}
	require.False(t, short.missingXorb(), "the hash must be 64 hex digits")
}

// writeStatus prints what `git send-pack --helper-status` prints: the
// advertised refs in order, then the new refs, with "no match" for a ref
// that the push does not update.
func TestWriteStatus(t *testing.T) {
	t.Parallel()
	adv := &advertisement{refs: []advertisedRef{{"refs/heads/main", oidA}, {"refs/heads/other", oidA}, {"refs/heads/gone", oidB}}}
	cmds := []command{
		{ref: "refs/heads/new", old: zero, new: oidB},
		{ref: "refs/heads/main", old: oidA, new: oidB},
		{ref: "refs/heads/gone", old: oidB, new: zero},
	}
	rep := &report{unpack: "ok", refs: map[string]refResult{
		"refs/heads/main": {ok: true},
		"refs/heads/gone": {msg: "hook said \"no\"\n"},
	}}
	var b bytes.Buffer
	require.NoError(t, writeStatus(&b, adv, cmds, rep))
	require.Equal(t, "ok refs/heads/main\n"+
		"error refs/heads/other no match\n"+
		"error refs/heads/gone \"hook said \\\"no\\\"\\n\"\n"+
		"error refs/heads/new expecting report\n", b.String())

	b.Reset()
	require.NoError(t, writeStatus(&b, adv, cmds, nil))
	require.Equal(t, "ok refs/heads/main\nerror refs/heads/other no match\nok refs/heads/gone\nok refs/heads/new\n", b.String())
}

func TestQuoteC(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"plain text":   "plain text",
		"tab\there":    `"tab\there"`,
		`back\slash`:   `"back\\slash"`,
		"caf\xc3\xa9":  `"caf\303\251"`,
		"del\x7f":      `"del\177"`,
		"bell\a\x01":   `"bell\a\001"`,
		"cr\r lf\n vt": `"cr\r lf\n vt"`,
	} {
		require.Equal(t, want, quoteC(in), in)
	}
}

// Server text reaches the terminal as git shows it: control bytes become
// ^X, so a server cannot move the cursor or clear the screen.
func TestSanitize(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"plain text\tand tab\n":         "plain text\tand tab\n",
		"bell\a and erase\x1b[2J":       "bell^G and erase^[[2J",
		"colour \x1b[1;31mred\x1b[m ok": "colour \x1b[1;31mred\x1b[m ok",
		"del\x7f nul\x00 cr\r":          "del^? nul^@ cr^M",
		"bare escape\x1b":               "bare escape^[",
		"utf-8 caf\xc3\xa9":             "utf-8 caf\xc3\xa9",
	} {
		require.Equal(t, want, Sanitize(in), "%q", in)
	}
}

func TestReadReportSanitizesServerText(t *testing.T) {
	t.Parallel()
	report := pkts("unpack ok\n", "ok refs/heads/main\n", "")
	var stderr bytes.Buffer
	body := pkts("\x02evil\x1b[2Jtext\r", "\x01"+string(report), "")
	_, err := readReport(bytes.NewReader(body), true, &stderr)
	require.NoError(t, err)
	require.Equal(t, "remote: evil^[[2Jtext\r", stderr.String())

	_, err = readReport(bytes.NewReader(pkts("\x03fatal\x1b]0;title\a", "")), true, &stderr)
	require.ErrorContains(t, err, "remote error: fatal^[]0;title^G")
}

// The band-1 data of a side-band response is a report-status. A server
// cannot make the client buffer more than maxReportSize of it.
func TestReadReportBoundsSidebandData(t *testing.T) {
	t.Parallel()
	chunk := "\x01" + strings.Repeat("x", 65000)
	var b bytes.Buffer
	for b.Len() <= maxReportSize+65520 {
		b.Write(pkts(chunk))
	}
	b.Write(pkts(""))
	_, err := readReport(&b, true, io.Discard)
	require.ErrorContains(t, err, "side-band data is over")
}

// countingReader counts the bytes that readReport takes.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// Without side-band, the ref status lines come straight from the server.
// A server cannot make the client buffer more than maxReportSize of them.
func TestReadReportBoundsStatusWithoutSideband(t *testing.T) {
	t.Parallel()
	line := "ok refs/heads/" + strings.Repeat("x", 65000)
	var b bytes.Buffer
	b.Write(pkts("unpack ok"))
	for b.Len() <= 2*maxReportSize {
		b.Write(pkts(line))
	}
	b.Write(pkts(""))
	r := &countingReader{r: &b}
	_, err := readReport(r, false, io.Discard)
	require.ErrorContains(t, err, "report-status is over")
	require.LessOrEqual(t, r.n, maxReportSize+2*pktline.MaxSize)
}
