package githelper

// Push-acknowledgement invariants. These tests pin the behaviours a
// git remote helper MUST honour to avoid silent data loss:
//
//   1. Never partially acknowledge a push — every refspec the client
//      provided either reaches the server or surfaces an error to git.
//   2. Never claim success before durable persistence — until the
//      server's report-status arrives in full, we don't write an "ok"
//      line for any ref.
//   3. Never mutate refs unexpectedly — only refspecs the client
//      explicitly pushed appear in the receive-pack request body.
//
// The tests below probe each invariant through handleConnect
// (connect-mode push, which is the default path) using a fake
// Transport that lets us inspect every byte that reached the wire
// and every byte that reached stdout.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestInvariant_NoSpuriousAckOnTransportError: the server's POST
// fails before any response body is written. Helper output MUST
// contain no "ok " lines and the error MUST propagate.
func TestInvariant_NoSpuriousAckOnTransportError(t *testing.T) {
	t.Parallel()
	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	ref := testRefMain

	ft := &fakeTransport{
		infoRefsResp: func() (io.ReadCloser, error) {
			return stringRC(serviceAnnouncement(serviceReceivePack,
				oldSHA+" "+ref+"\x00 report-status\n")), nil
		},
		serviceRPCResp: func(string, []byte) (io.ReadCloser, error) {
			return nil, errors.New("connection reset by peer")
		},
	}

	cmd := oldSHA + " " + newSHA + " " + ref + "\x00 report-status\n"
	var stdin bytes.Buffer
	stdin.WriteString(pktLine(cmd))
	stdin.WriteString("0000")
	stdin.WriteString("PACK fakepackdata")

	var out bytes.Buffer
	err := handleConnect(context.Background(), ft, serviceReceivePack, &stdin, &out)
	if err == nil {
		t.Fatal("expected error on transport failure")
	}
	for _, marker := range []string{"\nok ", "ok " + ref, "unpack ok"} {
		if strings.Contains(out.String(), marker) {
			t.Errorf("spurious success marker %q in output: %s", marker, out.String())
		}
	}
}

// TestInvariant_RequestBodyContainsOnlyClientRefspecs: the bytes
// posted to git-receive-pack MUST contain only the refspecs the
// client supplied — no synthetic creates, deletes, or "for safety"
// updates the helper invents on its own.
func TestInvariant_RequestBodyContainsOnlyClientRefspecs(t *testing.T) {
	t.Parallel()
	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	ref := testRefMain

	ft := &fakeTransport{
		infoRefsResp: func() (io.ReadCloser, error) {
			// Advertise an extra ref the client did NOT include in
			// its push batch. If the helper synthesised a refspec
			// from the advertisement, refs/heads/other would appear
			// in the captured POST body.
			return stringRC(serviceAnnouncement(serviceReceivePack,
				oldSHA+" "+ref+"\x00 report-status\n",
				strings.Repeat("c", 40)+" refs/heads/other\n")), nil
		},
		serviceRPCResp: func(string, []byte) (io.ReadCloser, error) {
			return stringRC(pktLine("unpack ok\n") + pktLine("ok "+ref+"\n") + "0000"), nil
		},
	}

	clientRef := strings.Repeat("d", 40) + " " + newSHA + " " + ref + "\x00 report-status\n"
	var stdin bytes.Buffer
	stdin.WriteString(pktLine(clientRef))
	stdin.WriteString("0000")
	stdin.WriteString("PACK")

	var out bytes.Buffer
	if err := handleConnect(context.Background(), ft, serviceReceivePack, &stdin, &out); err != nil {
		t.Fatalf("handleConnect: %v", err)
	}
	if len(ft.rpcCalls) != 1 {
		t.Fatalf("expected exactly 1 ServiceRPC call, got %d", len(ft.rpcCalls))
	}
	if strings.Contains(string(ft.rpcCalls[0].Body), "refs/heads/other") {
		t.Errorf("POST body mutated refs/heads/other (not in client batch): %q", ft.rpcCalls[0].Body)
	}
	if !strings.Contains(string(ft.rpcCalls[0].Body), ref) {
		t.Errorf("POST body missing client refspec %q: %q", ref, ft.rpcCalls[0].Body)
	}
}

// TestInvariant_DeleteOnlyPushOmitsPackData: a delete-only refspec
// (new OID = zeros) must not be followed by PACK data on the wire.
// receive-pack rejects pack data on delete-only batches with
// "fatal: unable to read header" — and even if the server tolerated
// it, sending pack bytes here would tell the server we pushed
// objects we didn't.
func TestInvariant_DeleteOnlyPushOmitsPackData(t *testing.T) {
	t.Parallel()
	assertDeleteOnlyPushOmitsPackData(t, 40, "")
}

// TestInvariant_DeleteOnlyPushOmitsPackData_SHA256: same as the SHA-1
// case but with 64-hex object IDs — the old fixed-offset detector
// mis-read bytes inside the old OID as the new OID and pulled PACK
// data after a delete-only flush.
func TestInvariant_DeleteOnlyPushOmitsPackData_SHA256(t *testing.T) {
	t.Parallel()
	assertDeleteOnlyPushOmitsPackData(t, 64, " object-format=sha256")
}

func assertDeleteOnlyPushOmitsPackData(t *testing.T, oidLen int, extraCapabilities string) {
	t.Helper()

	oldSHA := strings.Repeat("a", oidLen)
	zeroSHA := strings.Repeat("0", oidLen)
	ref := testRefFeatureBranch

	ft := &fakeTransport{
		infoRefsResp: func() (io.ReadCloser, error) {
			return stringRC(serviceAnnouncement(serviceReceivePack,
				oldSHA+" "+ref+"\x00 report-status delete-refs"+extraCapabilities+"\n")), nil
		},
		serviceRPCResp: func(string, []byte) (io.ReadCloser, error) {
			return stringRC(pktLine("unpack ok\n") + pktLine("ok "+ref+"\n") + "0000"), nil
		},
	}

	cmd := oldSHA + " " + zeroSHA + " " + ref + "\x00 report-status delete-refs\n"
	var stdin bytes.Buffer
	stdin.WriteString(pktLine(cmd))
	stdin.WriteString("0000")
	// No PACK bytes — delete-only push.

	var out bytes.Buffer
	if err := handleConnect(context.Background(), ft, serviceReceivePack, &stdin, &out); err != nil {
		t.Fatalf("handleConnect: %v", err)
	}
	if len(ft.rpcCalls) != 1 {
		t.Fatalf("expected 1 ServiceRPC call, got %d", len(ft.rpcCalls))
	}
	if bytes.Contains(ft.rpcCalls[0].Body, []byte("PACK")) {
		t.Errorf("delete-only push body unexpectedly contains PACK: %q", ft.rpcCalls[0].Body)
	}
}

// TestInvariant_AckOnlyAfterFullResponse: the server's
// report-status arrives in chunks and the helper streams it to
// stdout. The contract is that the bytes appear AFTER the POST has
// returned successfully — i.e. we never write an "ok" line before
// the server has emitted one. The helper's loop is:
//
//  1. read full request from stdin
//  2. POST it, get response
//  3. stream response to stdout
//
// Steps 2 and 3 are sequential: any "ok" the helper writes to
// stdout came from the server. We verify by checking that no "ok"
// appears in stdout when the server returns an empty body.
func TestInvariant_AckOnlyAfterFullResponse(t *testing.T) {
	t.Parallel()
	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	ref := testRefMain

	ft := &fakeTransport{
		infoRefsResp: func() (io.ReadCloser, error) {
			return stringRC(serviceAnnouncement(serviceReceivePack,
				oldSHA+" "+ref+"\x00 report-status\n")), nil
		},
		serviceRPCResp: func(string, []byte) (io.ReadCloser, error) {
			// Empty response body — server processed the push but
			// returned no report-status. Helper must surface this
			// as "no acknowledgement" rather than synthesise one.
			return stringRC(""), nil
		},
	}

	cmd := oldSHA + " " + newSHA + " " + ref + "\x00 report-status\n"
	var stdin bytes.Buffer
	stdin.WriteString(pktLine(cmd))
	stdin.WriteString("0000")
	stdin.WriteString("PACK fakepackdata")

	var out bytes.Buffer
	if err := handleConnect(context.Background(), ft, serviceReceivePack, &stdin, &out); err != nil {
		t.Fatalf("handleConnect: %v", err)
	}
	if strings.Contains(out.String(), "ok ") {
		t.Errorf("stdout contains 'ok' despite empty server response: %q", out.String())
	}
}

// TestInvariant_PartialRefBatchAllOrNothing: the client pushes two
// refs. When the helper reads the receive-pack request body, both
// commands must reach the server in a single POST. Partial batches
// (only one of the two) would let the server commit half the push
// and the client never learn about the other half.
func TestInvariant_PartialRefBatchAllOrNothing(t *testing.T) {
	t.Parallel()
	oldA := strings.Repeat("a", 40)
	newA := strings.Repeat("b", 40)
	oldB := strings.Repeat("c", 40)
	newB := strings.Repeat("d", 40)
	refA := testRefMain
	refB := "refs/heads/feature"

	ft := &fakeTransport{
		infoRefsResp: func() (io.ReadCloser, error) {
			return stringRC(serviceAnnouncement(serviceReceivePack,
				oldA+" "+refA+"\x00 report-status\n",
				oldB+" "+refB+"\n")), nil
		},
		serviceRPCResp: func(string, []byte) (io.ReadCloser, error) {
			return stringRC(pktLine("unpack ok\n") +
				pktLine("ok "+refA+"\n") +
				pktLine("ok "+refB+"\n") + "0000"), nil
		},
	}

	cmdA := oldA + " " + newA + " " + refA + "\x00 report-status\n"
	cmdB := oldB + " " + newB + " " + refB + "\n"
	var stdin bytes.Buffer
	stdin.WriteString(pktLine(cmdA))
	stdin.WriteString(pktLine(cmdB))
	stdin.WriteString("0000")
	stdin.WriteString("PACK fakepackdata")

	var out bytes.Buffer
	if err := handleConnect(context.Background(), ft, serviceReceivePack, &stdin, &out); err != nil {
		t.Fatalf("handleConnect: %v", err)
	}

	if len(ft.rpcCalls) != 1 {
		t.Fatalf("expected exactly 1 POST (atomic batch), got %d", len(ft.rpcCalls))
	}
	body := string(ft.rpcCalls[0].Body)
	if !strings.Contains(body, refA) {
		t.Errorf("POST body missing %q", refA)
	}
	if !strings.Contains(body, refB) {
		t.Errorf("POST body missing %q", refB)
	}
}
