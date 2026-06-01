package gitproto

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

// pktLine encodes a string as a git pkt-line.
func pktLine(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

func TestReadFlushTerminatedMessage_HappyPath(t *testing.T) {
	t.Parallel()
	body := "000ahello\n" + "000aworld\n" + "0000"
	got, ok, err := ReadFlushTerminatedMessage(strings.NewReader(body))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Fatal("ok = false")
	}
	if string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestReadFlushTerminatedMessage_EOFBeforeFirstByte(t *testing.T) {
	t.Parallel()
	got, ok, err := ReadFlushTerminatedMessage(strings.NewReader(""))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("ok = true, want false on clean EOF")
	}
	if got != nil {
		t.Errorf("got = %q, want nil", got)
	}
}

func TestReadFlushTerminatedMessage_TruncatedLength(t *testing.T) {
	t.Parallel()
	_, _, err := ReadFlushTerminatedMessage(strings.NewReader("00"))
	if err == nil {
		t.Fatal("expected error on truncated length")
	}
}

func TestReadFlushTerminatedMessage_TolerateSpecialLengths(t *testing.T) {
	t.Parallel()
	// 0001 delim and 0002 response-end carry no payload; pass through.
	body := "000ahello\n0001000b world\n00020000"
	got, ok, err := ReadFlushTerminatedMessage(strings.NewReader(body))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Fatal("ok = false")
	}
	if string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestReadPostServiceAdvertisementStripsPreamble(t *testing.T) {
	t.Parallel()

	// A realistic v0/v1 receive-pack info/refs body: service
	// announcement pktline, flush, then refs+caps + final flush. The
	// advertisement we hand to send-pack must NOT include the
	// "# service=..." line — connect.c:get_remote_heads dies on it
	// with "protocol error: unexpected '# service=git-receive-pack'".
	refsAndCaps := "003eaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main\n0000"
	body := "001f# service=git-receive-pack\n0000" + refsAndCaps

	got, err := ReadPostServiceAdvertisement(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ReadPostServiceAdvertisement: %v", err)
	}
	if string(got) != refsAndCaps {
		t.Errorf("got %q, want %q", got, refsAndCaps)
	}
}

func TestIsV2Advertisement(t *testing.T) {
	t.Parallel()

	t.Run("happy", func(t *testing.T) {
		t.Parallel()
		if !IsV2Advertisement([]byte(pktLine("version 2\n"))) {
			t.Error("expected true for valid v2 advertisement")
		}
	})
	t.Run("v1 looks-like", func(t *testing.T) {
		t.Parallel()
		if IsV2Advertisement([]byte(pktLine("version 1\n"))) {
			t.Error("expected false for v1 advertisement")
		}
	})
	t.Run("too short", func(t *testing.T) {
		t.Parallel()
		if IsV2Advertisement([]byte("00")) {
			t.Error("expected false for buffer shorter than length prefix")
		}
	})
	t.Run("length larger than buffer", func(t *testing.T) {
		t.Parallel()
		if IsV2Advertisement([]byte("ffff")) {
			t.Error("expected false when length exceeds buffer")
		}
	})
	t.Run("non-hex length", func(t *testing.T) {
		t.Parallel()
		if IsV2Advertisement([]byte("zzzz")) {
			t.Error("expected false on non-hex length")
		}
	})
}

func TestV2Command(t *testing.T) {
	t.Parallel()

	t.Run("happy", func(t *testing.T) {
		t.Parallel()
		message := pktLine("agent=test\n") + pktLine("command=bundle-uri\n") + "00010000"
		if got, want := V2Command([]byte(message)), "bundle-uri"; got != want {
			t.Errorf("V2Command = %q, want %q", got, want)
		}
	})
	t.Run("missing command", func(t *testing.T) {
		t.Parallel()
		message := pktLine("agent=test\n") + "0000"
		if got := V2Command([]byte(message)); got != "" {
			t.Errorf("V2Command = %q, want empty", got)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		t.Parallel()
		// length says 100 bytes but only 8 supplied.
		if got := V2Command([]byte("0064command=ls-refs")); got != "" {
			t.Errorf("V2Command on truncated = %q, want empty", got)
		}
	})
	t.Run("garbage length", func(t *testing.T) {
		t.Parallel()
		if got := V2Command([]byte("zzzzcommand=ls-refs\n")); got != "" {
			t.Errorf("V2Command on bad length = %q, want empty", got)
		}
	})
}

func TestAppendAgentToV2Request(t *testing.T) {
	t.Parallel()

	in := pktLine("command=ls-refs\n") + pktLine("agent=git/2.54.0\n") + "0000"
	got, err := AppendAgentToV2Request([]byte(in), "git-remote-entire/dev")
	if err != nil {
		t.Fatalf("AppendAgentToV2Request: %v", err)
	}
	want := pktLine("command=ls-refs\n") +
		pktLine("agent=git/2.54.0 git-remote-entire/dev\n") +
		"0000"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAppendAgentToV2RequestWithoutAgentUnchanged(t *testing.T) {
	t.Parallel()

	in := pktLine("command=ls-refs\n") + "0000"
	got, err := AppendAgentToV2Request([]byte(in), "git-remote-entire/dev")
	if err != nil {
		t.Fatalf("AppendAgentToV2Request: %v", err)
	}
	if string(got) != in {
		t.Errorf("got %q, want unchanged %q", got, in)
	}
}

func TestAppendAgentToUploadPackRequest(t *testing.T) {
	t.Parallel()

	oid := strings.Repeat("a", 40)
	in := pktLine("want "+oid+" multi_ack agent=git/2.54.0 object-format=sha1\n") + "0000"
	got, err := AppendAgentToUploadPackRequest([]byte(in), "git-remote-entire/dev")
	if err != nil {
		t.Fatalf("AppendAgentToUploadPackRequest: %v", err)
	}
	want := pktLine("want "+oid+" multi_ack agent=git/2.54.0+git-remote-entire/dev object-format=sha1\n") + "0000"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAppendAgentToReceivePackRequest(t *testing.T) {
	t.Parallel()

	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	ref := testRefMain
	cmd := oldSHA + " " + newSHA + " " + ref + "\x00 report-status agent=git/2.54.0\n"
	in := pktLine(cmd) + "0000PACK"

	got, err := AppendAgentToReceivePackRequest([]byte(in), "git-remote-entire/dev")
	if err != nil {
		t.Fatalf("AppendAgentToReceivePackRequest: %v", err)
	}
	wantCmd := oldSHA + " " + newSHA + " " + ref +
		"\x00 report-status agent=git/2.54.0+git-remote-entire/dev\n"
	want := pktLine(wantCmd) + "0000PACK"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadReceivePackRequest_DeleteOnly(t *testing.T) {
	t.Parallel()

	oldSHA := testHeadSHA
	zeroSHA := "0000000000000000000000000000000000000000"
	ref := testRefFeatureBranch

	var input bytes.Buffer
	cmd := oldSHA + " " + zeroSHA + " " + ref + "\x00 report-status delete-refs\n"
	input.WriteString(pktLine(cmd))
	input.WriteString("0000")

	result, err := ReadReceivePackRequest(&input)
	if err != nil {
		t.Fatalf("ReadReceivePackRequest failed: %v", err)
	}

	got := string(result)
	if !strings.Contains(got, oldSHA) {
		t.Errorf("result missing old SHA")
	}
	if !strings.Contains(got, ref) {
		t.Errorf("result missing ref name")
	}
	if !strings.HasSuffix(got, "0000") {
		t.Errorf("result should end with flush packet, got suffix %q", got[len(got)-4:])
	}
}

func TestReceivePackCommandNeedsPack(t *testing.T) {
	t.Parallel()

	sha1Old := strings.Repeat("a", 40)
	sha1New := strings.Repeat("b", 40)
	sha1Zero := strings.Repeat("0", 40)
	sha256Old := strings.Repeat("a", 64)
	sha256New := strings.Repeat("b", 64)
	sha256Zero := strings.Repeat("0", 64)
	ref := testRefMain

	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		{"sha1 update", sha1Old + " " + sha1New + " " + ref, true},
		{"sha1 delete", sha1Old + " " + sha1Zero + " " + ref, false},
		{"sha256 update", sha256Old + " " + sha256New + " " + ref, true},
		{"sha256 delete", sha256Old + " " + sha256Zero + " " + ref, false},
		{"sha256 delete with caps", sha256Old + " " + sha256Zero + " " + ref + "\x00 report-status delete-refs", false},
		{"malformed", "not-a-command", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := receivePackCommandNeedsPack([]byte(tc.cmd)); got != tc.want {
				t.Errorf("receivePackCommandNeedsPack(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

func TestReadReceivePackRequest_DeleteOnlySHA256(t *testing.T) {
	t.Parallel()

	oldSHA := strings.Repeat("a", 64)
	zeroSHA := strings.Repeat("0", 64)
	ref := testRefFeatureBranch
	packData := "PACK must not be consumed on delete-only push"

	var input bytes.Buffer
	cmd := oldSHA + " " + zeroSHA + " " + ref + "\x00 report-status delete-refs\n"
	input.WriteString(pktLine(cmd))
	input.WriteString("0000")
	input.WriteString(packData)

	result, err := ReadReceivePackRequest(&input)
	if err != nil {
		t.Fatalf("ReadReceivePackRequest failed: %v", err)
	}
	got := string(result)
	if strings.Contains(got, "PACK") {
		t.Errorf("delete-only SHA-256 push must not read pack data, got %q", got)
	}
	if !strings.HasSuffix(got, "0000") {
		t.Errorf("result should end with flush packet, got suffix %q", got[len(got)-4:])
	}
}

func TestReadReceivePackRequest_WithPackDataSHA256(t *testing.T) {
	t.Parallel()

	oldSHA := strings.Repeat("a", 64)
	newSHA := strings.Repeat("b", 64)
	ref := testRefMain
	packData := testFakePackData

	var input bytes.Buffer
	cmd := oldSHA + " " + newSHA + " " + ref + "\x00 report-status\n"
	input.WriteString(pktLine(cmd))
	input.WriteString("0000")
	input.WriteString(packData)

	result, err := ReadReceivePackRequest(&input)
	if err != nil {
		t.Fatalf("ReadReceivePackRequest failed: %v", err)
	}
	got := string(result)
	if !strings.Contains(got, packData) {
		t.Errorf("result missing pack data, got: %q", got)
	}
}

func TestReadReceivePackRequest_WithPackData(t *testing.T) {
	t.Parallel()

	oldSHA := testHeadSHA
	newSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	ref := testRefMain
	packData := testFakePackData

	var input bytes.Buffer
	cmd := oldSHA + " " + newSHA + " " + ref + "\x00 report-status\n"
	input.WriteString(pktLine(cmd))
	input.WriteString("0000")
	input.WriteString(packData)

	result, err := ReadReceivePackRequest(&input)
	if err != nil {
		t.Fatalf("ReadReceivePackRequest failed: %v", err)
	}

	got := string(result)
	if !strings.Contains(got, newSHA) {
		t.Errorf("result missing new SHA")
	}
	if !strings.Contains(got, packData) {
		t.Errorf("result missing pack data, got: %q", got)
	}
}

func TestReadReceivePackRequest_TruncatedFails(t *testing.T) {
	t.Parallel()

	// Command pkt-line announces an update but is followed by EOF instead
	// of the expected flush packet — must not be treated as a clean end
	// (otherwise a network truncation could look like a successful no-op
	// to the caller).
	oldSHA := testHeadSHA
	newSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cmd := oldSHA + " " + newSHA + " refs/heads/main\x00 report-status\n"
	body := pktLine(cmd) // no trailing flush

	_, err := ReadReceivePackRequest(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error on truncated request without flush")
	}
}

func TestReadSendPackRequest_IncludesPackData(t *testing.T) {
	t.Parallel()

	oldSHA := strings.Repeat("0", 40)
	newSHA := strings.Repeat("b", 40)
	ref := testRefMain
	cmd := oldSHA + " " + newSHA + " " + ref + "\x00 report-status push-options\n"
	commandSection := pktLine(cmd) + "0000"
	pushOptions := pktLine("ci.skip") + "0000"
	packData := testFakePackData
	trailingStatus := "0000ok " + ref + "\n"

	var input bytes.Buffer
	input.WriteString(pktLine(commandSection + pushOptions))
	input.WriteString(pktLine(packData))
	input.WriteString("0000")
	input.WriteString(trailingStatus)

	reader := bufio.NewReader(&input)
	var got bytes.Buffer
	if err := ReadSendPackRequest(reader, &got); err != nil {
		t.Fatalf("ReadSendPackRequest failed: %v", err)
	}

	want := commandSection + pushOptions + packData
	if got.String() != want {
		t.Fatalf("request body = %q, want %q", got.String(), want)
	}
	remaining, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading remaining status: %v", err)
	}
	if string(remaining) != trailingStatus {
		t.Fatalf("remaining status = %q, want %q", remaining, trailingStatus)
	}
}

func TestReadSendPackRequest_DeleteOnly(t *testing.T) {
	t.Parallel()

	oldSHA := strings.Repeat("a", 40)
	zeroSHA := strings.Repeat("0", 40)
	ref := testRefFeatureBranch
	cmd := oldSHA + " " + zeroSHA + " " + ref + "\x00 report-status delete-refs\n"
	commandSection := pktLine(cmd) + "0000"
	trailingStatus := "0000ok " + ref + "\n"

	var input bytes.Buffer
	input.WriteString(pktLine(commandSection))
	input.WriteString("0000")
	input.WriteString(trailingStatus)

	reader := bufio.NewReader(&input)
	var got bytes.Buffer
	if err := ReadSendPackRequest(reader, &got); err != nil {
		t.Fatalf("ReadSendPackRequest failed: %v", err)
	}

	if got.String() != commandSection {
		t.Fatalf("request body = %q, want %q", got.String(), commandSection)
	}
	remaining, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading remaining status: %v", err)
	}
	if string(remaining) != trailingStatus {
		t.Fatalf("remaining status = %q, want %q", remaining, trailingStatus)
	}
}

func TestReadSendPackRequest_TruncatedFails(t *testing.T) {
	t.Parallel()

	t.Run("truncated length prefix", func(t *testing.T) {
		t.Parallel()
		r := bufio.NewReader(strings.NewReader("00"))
		if err := ReadSendPackRequest(r, &bytes.Buffer{}); err == nil {
			t.Fatal("expected error on truncated length")
		}
	})

	t.Run("missing trailing flush", func(t *testing.T) {
		t.Parallel()
		// One inner payload followed by EOF instead of the outer flush.
		body := pktLine("0000ok refs/heads/main\n")
		r := bufio.NewReader(strings.NewReader(body))
		if err := ReadSendPackRequest(r, &bytes.Buffer{}); err == nil {
			t.Fatal("expected error on missing trailing flush")
		}
	})

	t.Run("payload shorter than declared length", func(t *testing.T) {
		t.Parallel()
		// Length claims 100 bytes of payload but only 10 follow.
		r := bufio.NewReader(strings.NewReader("0064short"))
		if err := ReadSendPackRequest(r, &bytes.Buffer{}); err == nil {
			t.Fatal("expected error on short payload")
		}
	})
}
