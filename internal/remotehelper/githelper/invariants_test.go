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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// wrappedSendPackRequest returns the stateless-rpc outer framing
// `git send-pack --stateless-rpc` emits for a minimal one-command
// receive-pack request: one outer packet wrapping the inner pkt-line
// stream, then the outer flush. The command line omits the
// NUL-separated capability list a real send-pack appends so the
// result stays shell-safe for printf stubs; handlePush only relays
// the body, and AppendAgentToReceivePackRequest leaves lines without
// a NUL unchanged.
func wrappedSendPackRequest(oldSHA, newSHA, ref string) string {
	inner := pktLine(oldSHA+" "+newSHA+" "+ref+" report-status\n") + "0000"
	return fmt.Sprintf("%04x%s0000", len(inner)+4, inner)
}

// pushFakeTransport returns a fakeTransport advertising oldSHA at
// testRefMain for receive-pack and answering any service RPC with an
// empty body.
func pushFakeTransport(oldSHA string) *fakeTransport {
	return &fakeTransport{
		infoRefsResp: func() (io.ReadCloser, error) {
			return stringRC(serviceAnnouncement(serviceReceivePack,
				oldSHA+" "+testRefMain+"\x00 report-status\n")), nil
		},
		serviceRPCResp: func(string, []byte) (io.ReadCloser, error) {
			return stringRC(""), nil
		},
	}
}

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

	// The empty ServiceRPC response body means the server processed the
	// push but returned no report-status. Helper must surface this as
	// "no acknowledgement" rather than synthesise one.
	ft := pushFakeTransport(oldSHA)

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

// TestInvariant_HelperStatusSurfacedOnSendPackError: when `git send-pack`
// exits non-zero on a per-ref rejection (e.g. branch protection, D/F
// conflict, server hook decline), its buffered `error <ref> <reason>`
// helper-status lines are what `transport-helper.c` reads to render
// `! [remote rejected] <ref> (<reason>)`. handlePush must therefore
// relay helper-status to stdout BEFORE returning the send-pack exit
// error — relaying only on the success path strips the rejection
// reason and leaves the user with bare "send-pack exited with error".
//
// A shell stub on PATH plays the role of `git send-pack`: it emits the
// wire shape of a server-side rejection (a request WAS sent, so the
// wrapped request and the cmds_sent-guarded trailing flush are both
// present before the helper-status), and exits 1.
func TestInvariant_HelperStatusSurfacedOnSendPackError(t *testing.T) {
	// No t.Parallel(): t.Setenv("PATH", ...) mutates process-global state.
	if runtime.GOOS == goosWindows {
		t.Skip("shell-script PATH stub is POSIX-only")
	}

	ref := testRefMain
	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	helperStatusLine := "error " + ref + " remote rejected: branch protection violation\n"

	// Stub git: emits a wrapped one-command request + outer flush,
	// drains stdin (refspecs + advertisement + receive-pack response),
	// then emits the trailing flush + helper-status line, then exits 1.
	// The exit code mirrors send-pack's behaviour on per-ref rejection.
	stubDir := t.TempDir()
	stubPath := filepath.Join(stubDir, "git")
	stub := "#!/bin/sh\n" +
		"printf '%s' '" + wrappedSendPackRequest(oldSHA, newSHA, ref) + "'\n" +
		"cat > /dev/null\n" +
		"printf '0000" + helperStatusLine + "'\n" +
		"exit 1\n"
	if err := os.WriteFile(stubPath, []byte(stub), 0o755); err != nil {
		t.Fatalf("writing stub git: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ft := pushFakeTransport(oldSHA)

	// handlePush takes the first "push" line plus a bufio.Reader for
	// the rest of the batch; readPushBatch terminates on the blank
	// line.
	firstLine := "push " + oldSHA + ":" + ref
	stdin := bufio.NewReader(strings.NewReader("\n"))

	var stdout bytes.Buffer
	err := handlePush(context.Background(), ft, &refAdvCache{}, firstLine, &Options{}, stdin, &stdout)
	if err == nil {
		t.Fatal("expected error from send-pack exit 1")
	}
	if !strings.Contains(stdout.String(), helperStatusLine) {
		t.Errorf("stdout missing helper-status line; got %q, want substring %q",
			stdout.String(), helperStatusLine)
	}
}

// TestInvariant_PushReusesListForPushAdvertisement pins the fix for ENCLI-267.
// Within one helper session the "push" command MUST reuse the ref
// advertisement fetched during "list for-push" rather than re-fetching
// info/refs. Git snapshots the remote refs from "list for-push" into its
// remote_refs list *before* running the pre-push hook, and the hook pushes
// per-checkpoint refs to the same remote. A fresh info/refs at push time then
// hands send-pack a ref (the freshly-pushed checkpoint) Git never asked to
// push; send-pack emits `error <ref> no match`, and Git — not finding it in
// remote_refs — warns `helper reported unexpected status of <ref>`. Reusing
// the list-for-push snapshot mirrors remote-curl.c's discovery cache and keeps
// that phantom ref out of send-pack's view.
func TestInvariant_PushReusesListForPushAdvertisement(t *testing.T) {
	// No t.Parallel(): t.Setenv("PATH", ...) mutates process-global state.
	if runtime.GOOS == goosWindows {
		t.Skip("shell-script PATH stub is POSIX-only")
	}

	ref := testRefMain
	oldSHA := strings.Repeat("a", 40)

	// Stub git send-pack: emit a wrapped one-command request + outer
	// flush, drain stdin, then the trailing flush + a plain "ok"
	// helper-status, exit 0.
	stubDir := t.TempDir()
	stub := "#!/bin/sh\n" +
		"printf '%s' '" + wrappedSendPackRequest(oldSHA, strings.Repeat("b", 40), ref) + "'\n" +
		"cat > /dev/null\nprintf '0000ok " + ref + "\\n'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "git"), []byte(stub), 0o755); err != nil {
		t.Fatalf("writing stub git: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The checkpoint ref the pre-push hook would push between list-for-push
	// and push: absent from the first advertisement, present in the second, so
	// a re-fetch (the bug) would expose it to send-pack.
	checkpointRef := "refs/entire/checkpoints/9H/01KX2ATMJ3FAZZFZ8CP1CA279H"
	receivePackCalls := 0
	ft := &fakeTransport{
		infoRefsResp: func() (io.ReadCloser, error) {
			receivePackCalls++
			refLine := oldSHA + " " + ref + "\x00report-status object-format=sha1\n"
			if receivePackCalls == 1 {
				return stringRC(serviceAnnouncement(serviceReceivePack, refLine)), nil
			}
			return stringRC(serviceAnnouncement(serviceReceivePack, refLine,
				oldSHA+" "+checkpointRef+"\n")), nil
		},
		serviceRPCResp: func(string, []byte) (io.ReadCloser, error) {
			return stringRC(""), nil
		},
	}

	stdin := strings.NewReader("list for-push\npush " + oldSHA + ":" + ref + "\n\n")
	var stdout bytes.Buffer
	if err := Run(context.Background(), ft, 2, stdin, &stdout); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if receivePackCalls != 1 {
		t.Fatalf("receive-pack info/refs fetched %d times; want 1 (push must reuse the list-for-push advertisement)", receivePackCalls)
	}
}

// TestInvariant_DryRunPushSkipsReceivePackPOST pins the fix for ENT-1199.
// On `git push --dry-run`, stateless-rpc send-pack emits no receive-pack
// request at all — the whole request emission is guarded by !args->dry_run,
// so its stdout is just the outer flush followed directly by helper-status
// lines (the trailing flush at send-pack.c:759 is guarded by cmds_sent).
// The helper must mirror remote-curl.c:rpc_service, which breaks without
// calling post_rpc when the first packet is a flush: no POST (the server
// deliberately 400s zero-byte receive-pack bodies), no trailing-flush
// drain (reading 4 more bytes would eat "ok r" from the status line).
func TestInvariant_DryRunPushSkipsReceivePackPOST(t *testing.T) {
	// No t.Parallel(): t.Setenv("PATH", ...) mutates process-global state.
	if runtime.GOOS == goosWindows {
		t.Skip("shell-script PATH stub is POSIX-only")
	}

	ref := testRefMain
	oldSHA := strings.Repeat("a", 40)

	// Stub git send-pack in dry-run shape: outer flush terminating the
	// (empty) request, drain stdin, then helper-status with NO trailing
	// flush, exit 0.
	stubDir := t.TempDir()
	stub := "#!/bin/sh\n" +
		"printf '0000'\n" +
		"cat > /dev/null\n" +
		"printf 'ok " + ref + "\\n'\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "git"), []byte(stub), 0o755); err != nil {
		t.Fatalf("writing stub git: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ft := pushFakeTransport(oldSHA)

	firstLine := "push " + oldSHA + ":" + ref
	stdin := bufio.NewReader(strings.NewReader("\n"))

	var stdout bytes.Buffer
	err := handlePush(context.Background(), ft, &refAdvCache{}, firstLine, &Options{dryRun: true}, stdin, &stdout)
	if err != nil {
		t.Fatalf("handlePush: %v", err)
	}
	if len(ft.rpcCalls) != 0 {
		t.Errorf("expected no receive-pack POST on dry run, got %d", len(ft.rpcCalls))
	}
	if want := "ok " + ref + "\n\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestInvariant_DryRunPushRealSendPack drives handlePush with the REAL
// git binary — no shell stub — so the wire-shape assumption behind the
// dry-run skip (send-pack emits no request and no trailing flush when
// cmds_sent is 0) is pinned against the installed git, not our model
// of it. A fast-forward update refs/heads/<old> → HEAD is pushed with
// --dry-run; the helper must complete without POSTing and relay the
// "ok" helper-status line.
func TestInvariant_DryRunPushRealSendPack(t *testing.T) {
	// No t.Parallel(): t.Chdir mutates process-global state (send-pack
	// resolves the repo from CWD).
	repo := t.TempDir()
	gitRun := func(args ...string) string {
		t.Helper()
		base := []string{"-C", repo,
			"-c", "user.name=test", "-c", "user.email=test@example.com",
			"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}
		out, err := exec.CommandContext(context.Background(), "git", append(base, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitRun("init", "-q")
	gitRun("commit", "--allow-empty", "-m", "one")
	oldSHA := gitRun("rev-parse", "HEAD")
	gitRun("commit", "--allow-empty", "-m", "two")
	t.Chdir(repo)

	ft := pushFakeTransport(oldSHA)

	firstLine := "push " + testRefMain + ":" + testRefMain
	stdin := bufio.NewReader(strings.NewReader("\n"))

	var stdout bytes.Buffer
	err := handlePush(context.Background(), ft, &refAdvCache{}, firstLine, &Options{dryRun: true}, stdin, &stdout)
	if err != nil {
		t.Fatalf("handlePush: %v", err)
	}
	if len(ft.rpcCalls) != 0 {
		t.Errorf("expected no receive-pack POST on dry run, got %d", len(ft.rpcCalls))
	}
	if want := "ok " + testRefMain + "\n\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestInvariant_AllRejectedPushSkipsReceivePackPOST: same empty-request
// wire shape as a dry run, but produced by a batch where send-pack
// rejected every update client-side (e.g. non-fast-forward without
// force) — cmds_sent stays 0, nothing is emitted, and the helper-status
// `error` lines follow the outer flush directly. The helper must skip
// the POST, relay the error lines, and surface send-pack's exit error.
func TestInvariant_AllRejectedPushSkipsReceivePackPOST(t *testing.T) {
	// No t.Parallel(): t.Setenv("PATH", ...) mutates process-global state.
	if runtime.GOOS == goosWindows {
		t.Skip("shell-script PATH stub is POSIX-only")
	}

	ref := testRefMain
	oldSHA := strings.Repeat("a", 40)
	helperStatusLine := "error " + ref + " non-fast-forward\n"

	stubDir := t.TempDir()
	stub := "#!/bin/sh\n" +
		"printf '0000'\n" +
		"cat > /dev/null\n" +
		"printf '" + helperStatusLine + "'\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(stubDir, "git"), []byte(stub), 0o755); err != nil {
		t.Fatalf("writing stub git: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ft := pushFakeTransport(oldSHA)

	firstLine := "push " + oldSHA + ":" + ref
	stdin := bufio.NewReader(strings.NewReader("\n"))

	var stdout bytes.Buffer
	err := handlePush(context.Background(), ft, &refAdvCache{}, firstLine, &Options{}, stdin, &stdout)
	if err == nil {
		t.Fatal("expected error from send-pack exit 1")
	}
	if len(ft.rpcCalls) != 0 {
		t.Errorf("expected no receive-pack POST for all-rejected batch, got %d", len(ft.rpcCalls))
	}
	if !strings.Contains(stdout.String(), helperStatusLine) {
		t.Errorf("stdout missing helper-status line; got %q, want substring %q",
			stdout.String(), helperStatusLine)
	}
}
