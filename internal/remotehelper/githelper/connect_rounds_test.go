package githelper

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Set by uploadPackStub; read by TestMain.
const (
	helperUploadPackEnv = "GITHELPER_TEST_UPLOAD_PACK_DIR"
	helperStripNAKEnv   = "GITHELPER_TEST_STRIP_ROUND_NAK"
	helperSingleACKEnv  = "GITHELPER_TEST_SINGLE_ACK"
	helperNoSideBandEnv = "GITHELPER_TEST_NO_SIDE_BAND"
)

// execTransport answers the helper with `git upload-pack --stateless-rpc`
// on a local repository, so a test needs no listener. stripRoundNAK drops
// the NAK that ends a round without done, as a server that does not
// follow Git's rule would. singleACK leaves multi_ack and
// multi_ack_detailed out of the advertisement, so git asks for neither;
// noSideBand leaves side-band and side-band-64k out, so the pack comes raw.
type execTransport struct {
	dir           string
	stripRoundNAK bool
	singleACK     bool
	noSideBand    bool
}

func (e *execTransport) InfoRefs(ctx context.Context, service string) (io.ReadCloser, error) {
	out, err := exec.CommandContext(ctx, "git", strings.TrimPrefix(service, "git-"), "--stateless-rpc", "--advertise-refs", e.dir).Output()
	if err != nil {
		return nil, fmt.Errorf("advertise refs: %w", err)
	}
	if e.singleACK {
		out = withoutMultiACK(out)
	}
	if e.noSideBand {
		out = withoutSideBand(out)
	}
	header := pktLine("# service="+service+"\n") + "0000"
	return io.NopCloser(io.MultiReader(strings.NewReader(header), bytes.NewReader(out))), nil
}

// withoutMultiACK drops the multi_ack capabilities from the first line of
// a ref advertisement.
func withoutMultiACK(adv []byte) []byte {
	return replaceFirstLine(adv, strings.NewReplacer(" multi_ack_detailed", "", " multi_ack", "", "\x00multi_ack_detailed ", "\x00", "\x00multi_ack ", "\x00"))
}

// withoutSideBand drops the side-band capabilities from the first line of a
// ref advertisement.
func withoutSideBand(adv []byte) []byte {
	return replaceFirstLine(adv, strings.NewReplacer(" side-band-64k", "", " side-band", "", "\x00side-band-64k ", "\x00", "\x00side-band ", "\x00"))
}

// replaceFirstLine rewrites the first pkt-line of a ref advertisement.
func replaceFirstLine(adv []byte, r *strings.Replacer) []byte {
	n, err := strconv.ParseUint(string(adv[:4]), 16, 16)
	if err != nil || n < 4 {
		return adv
	}
	return append([]byte(pktLine(r.Replace(string(adv[4:n])))), adv[n:]...)
}

func (e *execTransport) InfoRefsV2(context.Context) (io.ReadCloser, error) {
	return nil, errors.New("execTransport speaks v0 only")
}

func (e *execTransport) ServiceRPC(ctx context.Context, service string, body io.ReadSeeker, _ ...func(*http.Request)) (io.ReadCloser, error) {
	req, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", strings.TrimPrefix(service, "git-"), "--stateless-rpc", e.dir)
	cmd.Stdin = bytes.NewReader(req)
	out, err := cmd.Output()
	// upload-pack exits non-zero after done with nothing common.
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("%s: %w", service, err)
	}
	if e.stripRoundNAK && !bytes.Contains(req, []byte(pktLine("done\n"))) {
		out = bytes.TrimSuffix(out, []byte("0008NAK\n"))
	}
	return io.NopCloser(bytes.NewReader(out)), nil
}

func (e *execTransport) ErrorBaseURL() string { return "file://" + e.dir }

// uploadPackStub installs a git-remote-<scheme> that re-executes this test
// binary as the helper with an execTransport on dir.
func uploadPackStub(t *testing.T, scheme, dir string, stripRoundNAK, singleACK, noSideBand bool) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return ""
	}
	stubDir := t.TempDir()
	script := "#!/bin/sh\n" +
		helperUploadPackEnv + "='" + dir + "' " +
		helperStripNAKEnv + "=" + flag(stripRoundNAK) + " " +
		helperSingleACKEnv + "=" + flag(singleACK) + " " +
		helperNoSideBandEnv + "=" + flag(noSideBand) + " " +
		"exec '" + exe + "'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "git-remote-"+scheme), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stubDir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func TestPostRound_NonFinalRoundEndsWithOneNAK(t *testing.T) {
	t.Parallel()
	const nak = "0008NAK\n"
	ack := pktLine("ACK " + testHeadSHA + " continue\n")
	shallow := pktLine("shallow "+testHeadSHA+"\n") + "0000"
	for _, tc := range []struct {
		name         string
		resp         string
		stripShallow bool
		want         string
	}{
		{name: "empty round from an older server", resp: "", want: nak},
		{name: "ACKs from an older server", resp: ack, want: ack + nak},
		{name: "server NAK", resp: nak, want: nak},
		{name: "ACKs and server NAK", resp: ack + nak, want: ack + nak},
		{name: "deepen round with server NAK", resp: shallow + ack + nak, stripShallow: true, want: ack + nak},
		{name: "deepen round from an older server", resp: shallow + ack, stripShallow: true, want: ack + nak},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := &fakeTransport{serviceRPCResp: func(string, []byte) (io.ReadCloser, error) {
				return stringRC(tc.resp), nil
			}}
			wants := bytes.NewBufferString(pktLine("want "+testHeadSHA+" multi_ack agent=git/2\n") + "0000")
			haves := bytes.NewBufferString(pktLine("have " + testHeadSHA + "\n"))
			var stdout bytes.Buffer
			if err := postRound(t.Context(), tr, &stdout, wants, haves, &ackState{}, false, tc.stripShallow, 1); err != nil {
				t.Fatalf("postRound: %v", err)
			}
			if got := stdout.String(); got != tc.want {
				t.Errorf("stdout = %q, want %q", got, tc.want)
			}
		})
	}
}

// Without multi_ack, git reads one NAK per round until the server's single
// ACK, then nothing until the pack. Each request repeats every have, so
// the server ACKs the common have again in later rounds and before the
// pack. The helper must drop those repeats.
func TestRelaySingleACKRound(t *testing.T) {
	t.Parallel()
	const nak = "0008NAK\n"
	ack := pktLine("ACK " + testHeadSHA + "\n")
	pack := "000a\x01PACK" + "0000"
	bigPack := pktLine("\x01PACK"+strings.Repeat("x", 60000)) + "0000"
	rawPack := "PACK\x00\x00\x00\x02\x00\x00\x00\x00" + strings.Repeat("\xff", 20)
	for _, tc := range []struct {
		name   string
		rounds []string // the last one is the done round
		want   string
	}{
		{
			name:   "server with round NAK",
			rounds: []string{nak, ack, ack, ack + pack},
			want:   nak + ack + pack,
		},
		{
			name:   "server without round NAK",
			rounds: []string{"", ack, ack, ack + pack},
			want:   nak + ack + pack,
		},
		{
			name:   "common have only in the done round",
			rounds: []string{nak, ack + pack},
			want:   nak + ack + pack,
		},
		{
			// side-band-64k packets reach 65520 bytes, far past a
			// default bufio.Reader.
			name:   "large pack packet after the ACK",
			rounds: []string{ack, ack + bigPack},
			want:   ack + bigPack,
		},
		{
			// Without side-band the pack follows the ACK raw: its header
			// is no pkt-line.
			name:   "pack without side-band after the ACK",
			rounds: []string{nak, ack, ack + rawPack},
			want:   nak + ack + rawPack,
		},
		{
			// The first ACK arrives with the pack, in the done round.
			name:   "pack without side-band, first ACK in the done round",
			rounds: []string{nak, ack + rawPack},
			want:   nak + ack + rawPack,
		},
		{
			// One ACK per request, repeated across rounds because every
			// request repeats the haves. The done round still starts
			// with a repeated ACK, then the raw pack.
			name:   "pack without side-band, several ACKs before the pack",
			rounds: []string{nak, ack, ack, ack + rawPack},
			want:   nak + ack + rawPack,
		},
		{
			// Already passed on main: the done round's NAK ends the loop
			// before the pack header is peeked, and the copy takes the rest.
			name:   "pack without side-band, nothing common",
			rounds: []string{nak, nak + rawPack},
			want:   nak + nak + rawPack,
		},
		{
			name:   "nothing common",
			rounds: []string{nak, nak + pack},
			want:   nak + nak + pack,
		},
		{
			// git stops at ERR; the NAK after it is never read.
			name:   "error before the first ACK",
			rounds: []string{pktLine("ERR boom\n")},
			want:   pktLine("ERR boom\n") + nak,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var acks ackState
			var out bytes.Buffer
			for i, r := range tc.rounds {
				final := i == len(tc.rounds)-1 && strings.Contains(r, "PACK")
				if err := relaySingleACKRound(&out, strings.NewReader(r), &acks, final); err != nil {
					t.Fatalf("round %d: %v", i+1, err)
				}
			}
			if got := out.String(); got != tc.want {
				t.Errorf("relayed %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRoundTail_EndsWithNAK(t *testing.T) {
	t.Parallel()
	ack := pktLine("ACK " + testHeadSHA + " continue\n")
	for _, tc := range []struct {
		name   string
		chunks []string
		want   bool
	}{
		{name: "empty", want: false},
		{name: "NAK alone", chunks: []string{"0008NAK\n"}, want: true},
		{name: "ACK then NAK", chunks: []string{ack + "0008NAK\n"}, want: true},
		{name: "NAK split over writes", chunks: []string{ack + "0008N", "A", "K\n"}, want: true},
		{name: "ACK only", chunks: []string{ack}, want: false},
		{name: "NAK then ACK", chunks: []string{"0008NAK\n", ack}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var tail roundTail
			for _, c := range tc.chunks {
				if _, err := tail.Write([]byte(c)); err != nil {
					t.Fatal(err)
				}
			}
			if got := tail.endsWithNAK(); got != tc.want {
				t.Errorf("endsWithNAK = %v, want %v", got, tc.want)
			}
		})
	}
}

// A non-final round streams to git as it arrives. The server controls
// the response size, so the helper must not hold a whole round in memory.
func TestPostRound_NonFinalRoundStreams(t *testing.T) {
	t.Parallel()
	respR, respW := io.Pipe()
	tr := &fakeTransport{serviceRPCResp: func(string, []byte) (io.ReadCloser, error) {
		return respR, nil
	}}
	outR, outW := io.Pipe()
	wants := bytes.NewBufferString(pktLine("want "+testHeadSHA+" multi_ack agent=git/2\n") + "0000")
	done := make(chan error, 1)
	go func() {
		done <- postRound(t.Context(), tr, outW, wants, &bytes.Buffer{}, &ackState{}, false, false, 1)
		_ = outW.Close()
	}()

	ack := pktLine("ACK " + testHeadSHA + " continue\n")
	go func() { _, _ = io.WriteString(respW, ack) }() //nolint:errcheck // fails only after the test closed the pipe
	got := make([]byte, len(ack))
	read := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(outR, got)
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("reading streamed ACK: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = respW.Close()
		t.Fatal("ACK did not reach git before the round's response ended")
	}
	if string(got) != ack {
		t.Fatalf("streamed %q, want %q", got, ack)
	}

	_ = respW.Close()
	rest, err := io.ReadAll(outR)
	if err != nil {
		t.Fatalf("reading rest: %v", err)
	}
	if string(rest) != "0008NAK\n" {
		t.Errorf("rest = %q, want the helper's NAK", rest)
	}
	if err := <-done; err != nil {
		t.Fatalf("postRound: %v", err)
	}
}

func runGitStdin(t *testing.T, gitDir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"--git-dir=" + gitDir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestGitFetchConnectRounds fetches with canonical git in connect mode
// over several have rounds. The client holds 100 commits the server
// lacks, so rounds of unknown haves come before the common history.
// Git reads a round until NAK and stays one round ahead, so one NAK too
// many per round leaves ACK/NAK lines in front of the pack.
func TestGitFetchConnectRounds(t *testing.T) {
	// No t.Parallel(): newRealGitRepo and the commit dates set process env.
	if runtime.GOOS == goosWindows {
		t.Skip("shell-script helper stub is POSIX-only")
	}
	type fetchCase struct {
		name          string
		stripRoundNAK bool
		singleACK     bool
		noSideBand    bool
		args          []string
	}
	var cases []fetchCase
	for _, noSideBand := range []bool{false, true} {
		for _, singleACK := range []bool{false, true} {
			// With multi_ack the round is a plain io.Copy, so a no-side-band
			// rerun never reaches the changed code. Those cases already pass
			// on main and would only add runtime; the subtests cannot run
			// in parallel.
			if noSideBand && !singleACK {
				continue
			}
			for _, strip := range []bool{true, false} {
				for _, deepen := range []bool{false, true} {
					tc := fetchCase{stripRoundNAK: strip, singleACK: singleACK, noSideBand: noSideBand}
					tc.name = "server with round NAK"
					if strip {
						tc.name = "server without round NAK"
					}
					if deepen {
						tc.name += ", deepen"
						tc.args = []string{"--depth=3"}
					}
					if singleACK {
						tc.name += ", single ACK"
					}
					if noSideBand {
						tc.name += ", no side-band"
					}
					cases = append(cases, tc)
				}
			}
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, run := newRealGitRepo(t)
			// Fixed dates fix the have order: local commits first.
			commitAt := func(minute int, msg string) {
				stamp := strconv.Itoa(1_704_067_200+minute*60) + " +0000"
				t.Setenv("GIT_AUTHOR_DATE", stamp)
				t.Setenv("GIT_COMMITTER_DATE", stamp)
				run("commit", "-q", "--allow-empty", "-m", msg)
			}
			for i := range 30 {
				commitAt(i, "shared "+strconv.Itoa(i))
			}
			shared := run("rev-parse", "HEAD")
			server := t.TempDir()
			runGit(t, "init", "-q", "--bare", "-b", "main", server)
			run("push", "-q", server, "HEAD:refs/heads/main")
			// An incompressible blob makes the pack span many side-band
			// packets of up to 65520 bytes.
			blobFile := filepath.Join(t.TempDir(), "blob")
			blob := make([]byte, 200<<10)
			if _, err := rand.Read(blob); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(blobFile, blob, 0o600); err != nil {
				t.Fatal(err)
			}
			blobID := run("--git-dir="+server, "hash-object", "-w", blobFile)
			tree := runGitStdin(t, server, "100644 blob "+blobID+"\tbig\n", "mktree")
			tip := run("--git-dir="+server, "commit-tree", tree, "-p", shared, "-m", "server tip")
			runGit(t, "-C", server, "update-ref", "refs/heads/main", tip)
			run("checkout", "-q", "-b", "local")
			for i := range 100 {
				commitAt(100+i, "local "+strconv.Itoa(i))
			}

			const scheme = "entiretest"
			path := uploadPackStub(t, scheme, server, tc.stripRoundNAK, tc.singleACK, tc.noSideBand)
			run("remote", "add", "origin", scheme+"://host/repo")
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "protocol.version=0", "fetch", "origin"}, tc.args...)...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+path)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git fetch: %v\n%s", err, out)
			}
			if got := run("rev-parse", "refs/remotes/origin/main"); got != tip {
				t.Errorf("origin/main = %s, want %s", got, tip)
			}
		})
	}
}
