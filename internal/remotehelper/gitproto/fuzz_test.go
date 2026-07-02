package gitproto

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// FuzzIsV2Advertisement exercises the v2-advertisement detector
// against arbitrary byte sequences. The function returns a boolean
// with no error channel, so the only thing we check is that we never
// panic and never read beyond the input.
func FuzzIsV2Advertisement(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("000a"))
	f.Add([]byte("000eversion 2\n"))
	f.Add([]byte("000eversion 1\n"))
	f.Add([]byte("ffff"))
	f.Add([]byte("zzzz"))

	f.Fuzz(func(_ *testing.T, in []byte) {
		// Don't validate the result — just that it returns without
		// panicking and doesn't take ages on pathological lengths.
		_ = IsV2Advertisement(in)
	})
}

// FuzzV2Command exercises the v2 command extractor. The function
// returns "" on any malformed input rather than erroring, so we only
// pin "never panic".
func FuzzV2Command(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("0014command=ls-refs\n0000"))
	f.Add([]byte("0000"))
	f.Add([]byte("0001"))
	f.Add([]byte("0002"))
	f.Add([]byte("ffffXXXX"))
	f.Add([]byte("003eaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main\n0000"))

	f.Fuzz(func(_ *testing.T, in []byte) {
		_ = V2Command(in)
	})
}

// FuzzReadReceivePackRequest fuzzes the receive-pack request reader.
// Contract: never panic; if no error, the returned bytes mirror the
// consumed input shape (length-prefix correctness is the caller's
// problem).
func FuzzReadReceivePackRequest(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("0000"))
	f.Add([]byte("000aabcdef\n0000"))
	f.Add([]byte("000aabcdef\n0000PACK fakepackdata"))
	// Receive-pack-shaped command (delete) plus flush.
	deleteCmd := strings.Repeat("a", 40) + " " + strings.Repeat("0", 40) +
		" refs/heads/x\x00 report-status delete-refs\n"
	f.Add([]byte("009f" + deleteCmd + "0000"))
	f.Add([]byte("0003"))

	f.Fuzz(func(_ *testing.T, in []byte) {
		_, _ = ReadReceivePackRequest(bytes.NewReader(in)) //nolint:errcheck // fuzz: panics matter, errors don't
	})
}

// FuzzReadSendPackRequest fuzzes the outer send-pack stateless-rpc
// framing parser. Contract: never panic. send-pack stdout has a
// stricter shape than receive-pack input — the parser should reject
// truncations rather than read past EOF.
func FuzzReadSendPackRequest(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("0000"))
	f.Add([]byte("000a8 byte0000"))
	f.Add([]byte("000a8 byte000a8 byte0000"))
	f.Add([]byte("00ff"))
	f.Add([]byte("0003"))

	f.Fuzz(func(_ *testing.T, in []byte) {
		var out bytes.Buffer
		_ = ReadSendPackRequest(bufio.NewReader(bytes.NewReader(in)), &out) //nolint:errcheck // fuzz
	})
}

// FuzzReadPostServiceAdvertisement fuzzes the smart-HTTP info/refs
// preamble stripper. The fuzz contract is only that arbitrary input
// does not panic or exhaust memory; malformed input may return any
// ordinary error.
func FuzzReadPostServiceAdvertisement(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("001f# service=git-upload-pack\n0000"))
	f.Add([]byte("001f# service=git-receive-pack\n0000refs"))
	f.Add([]byte("0000refs"))
	f.Add([]byte("ffff"))

	f.Fuzz(func(_ *testing.T, in []byte) {
		_, _ = ReadPostServiceAdvertisement(bytes.NewReader(in)) //nolint:errcheck // fuzz: panics and OOMs matter, errors don't
	})
}

// FuzzReadFlushTerminatedMessage exercises the v2 flush-terminated
// message reader. The fuzz contract is only that arbitrary input does
// not panic or exhaust memory; semantic invariants live in unit tests.
func FuzzReadFlushTerminatedMessage(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("0000"))
	f.Add([]byte("0001000a 0000"))
	f.Add([]byte("0010some content padded0000"))
	f.Add([]byte("000ahello\n0000"))
	f.Add([]byte("0003"))         // sub-minimum length
	f.Add([]byte("ffff"))         // largest length, no payload follows
	f.Add([]byte("ffffaaaaaaaa")) // length larger than payload

	f.Fuzz(func(_ *testing.T, in []byte) {
		_, _, _ = ReadFlushTerminatedMessage(bytes.NewReader(in)) //nolint:errcheck // fuzz: panics and OOMs matter, errors don't
	})
}
