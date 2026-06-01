package githelper

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/entireio/cli/internal/remotehelper/debuglog"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
)

// handlePush implements the git-remote-helpers "push" / "push for-push"
// command sequence. Git sends a batch of "push <src>:<dst>" lines
// terminated by a blank line; we shell out to
//
//	git send-pack --stateless-rpc --helper-status --stdin <url>
//
// to do all the heavy lifting (computing reachable objects, building
// the pack, parsing the v0 ref advertisement, framing the
// receive-pack request, parsing report-status). Send-pack's
// stdin/stdout are the "connection" in stateless-rpc mode, so we
// bridge that to a single HTTP POST against /git-receive-pack — this
// preserves our scoped-token auth and replica-failover path.
//
// Protocol flow (mirroring remote-curl.c:push_git + rpc_service):
//  1. Read remaining "push <src>:<dst>" lines from git until blank
//     line.
//  2. Fetch v0 info/refs?service=git-receive-pack (the ref
//     advertisement send-pack would normally read from the network).
//  3. Feed refspecs (as pktlines + flush) + advertisement to
//     send-pack stdin.
//  4. Read send-pack stdout as a receive-pack request body (refs +
//     capabilities + flush, then PACK data for non-delete updates).
//  5. POST it to /git-receive-pack, stream response back into
//     send-pack stdin.
//  6. Send-pack writes a trailing flush + helper-status lines to
//     stdout; we discard the flush and relay helper-status to git,
//     then append the blank line that terminates the status batch.
func handlePush(ctx context.Context, t Transport, firstLine string, opts *Options, stdin *bufio.Reader, stdout io.Writer) error {
	refspecs, err := readPushBatch(firstLine, stdin)
	if err != nil {
		return err
	}

	refsResp, err := t.InfoRefs(ctx, serviceReceivePack)
	if err != nil {
		return fmt.Errorf("fetching receive-pack info/refs: %w", err)
	}
	advertisement, err := gitproto.ReadPostServiceAdvertisement(refsResp)
	_ = refsResp.Close()
	if err != nil {
		return fmt.Errorf("reading receive-pack info/refs: %w", err)
	}

	var preamble bytes.Buffer
	for _, spec := range refspecs {
		fmt.Fprintf(&preamble, "%04x%s\n", len(spec)+5, spec)
	}
	preamble.WriteString("0000")

	args := append([]string{"send-pack", "--stateless-rpc", "--helper-status", "--stdin"},
		opts.SendPackArgs()...)
	args = append(args, t.ErrorBaseURL())
	sp := exec.CommandContext(ctx, "git", args...)
	sp.Stderr = os.Stderr
	spIn, err := sp.StdinPipe()
	if err != nil {
		return fmt.Errorf("send-pack stdin pipe: %w", err)
	}
	spOut, err := sp.StdoutPipe()
	if err != nil {
		return fmt.Errorf("send-pack stdout pipe: %w", err)
	}
	if err := sp.Start(); err != nil {
		return fmt.Errorf("starting send-pack: %w", err)
	}

	respCh := make(chan io.Reader, 1)
	feedErr := make(chan error, 1)
	go func() {
		defer spIn.Close()
		if _, err := spIn.Write(preamble.Bytes()); err != nil {
			feedErr <- fmt.Errorf("writing refspecs to send-pack: %w", err)
			return
		}
		if _, err := spIn.Write(advertisement); err != nil {
			feedErr <- fmt.Errorf("writing advertisement to send-pack: %w", err)
			return
		}
		resp, ok := <-respCh
		if !ok {
			feedErr <- nil
			return
		}
		if _, err := io.Copy(spIn, resp); err != nil {
			feedErr <- fmt.Errorf("piping receive-pack response to send-pack: %w", err)
			return
		}
		feedErr <- nil
	}()

	spOutReader := bufio.NewReader(spOut)
	var requestBody bytes.Buffer
	if err := gitproto.ReadSendPackRequest(spOutReader, &requestBody); err != nil {
		close(respCh)
		killAndWaitSendPack(sp, "after reading send-pack request failed")
		return fmt.Errorf("reading send-pack request: %w", err)
	}
	amendedBody, err := gitproto.AppendAgentToReceivePackRequest(requestBody.Bytes(), Agent)
	if err != nil {
		close(respCh)
		killAndWaitSendPack(sp, "after amending receive-pack agent failed")
		return fmt.Errorf("amending receive-pack agent: %w", err)
	}

	resp, err := t.ServiceRPC(ctx, serviceReceivePack, bytes.NewReader(amendedBody))
	if err != nil {
		close(respCh)
		killAndWaitSendPack(sp, "after posting receive-pack failed")
		return fmt.Errorf("posting receive-pack: %w", err)
	}
	respCh <- resp
	close(respCh)

	// Send-pack's stdout sequence after we write the response:
	//   <0000>                       <- packet_flush(out) at send-pack.c:759
	//   ok refs/heads/...            <- print_helper_status (plain text)
	//   error refs/...
	// Drain the trailing flush so git sees only the newline-delimited
	// status lines that transport-helper.c:push_update_refs_status
	// expects.
	flushBuf := make([]byte, 4)
	if _, err := io.ReadFull(spOutReader, flushBuf); err != nil {
		_ = resp.Close()
		return fmt.Errorf("reading send-pack trailing flush: %w", err)
	}
	if string(flushBuf) != "0000" {
		_ = resp.Close()
		return fmt.Errorf("expected trailing flush from send-pack, got %q", flushBuf)
	}

	helperStatus, err := io.ReadAll(spOutReader)
	_ = resp.Close()
	if err != nil {
		return fmt.Errorf("reading helper-status: %w", err)
	}

	if err := <-feedErr; err != nil {
		if waitErr := sp.Wait(); waitErr != nil {
			return errors.Join(err, fmt.Errorf("send-pack exited after feeder error: %w", waitErr))
		}
		return err
	}
	if err := sp.Wait(); err != nil {
		return fmt.Errorf("send-pack exited with error: %w", err)
	}

	if _, err := stdout.Write(helperStatus); err != nil {
		return fmt.Errorf("writing helper-status: %w", err)
	}
	if _, err := fmt.Fprintln(stdout); err != nil {
		return fmt.Errorf("writing push terminator: %w", err)
	}
	return nil
}

// readPushBatch collects the "push <src>:<dst>" lines that follow the
// initial firstLine (already consumed by the dispatcher), stopping at
// the blank line that terminates the batch.
func readPushBatch(firstLine string, stdin *bufio.Reader) ([]string, error) {
	spec, ok := strings.CutPrefix(firstLine, "push ")
	if !ok {
		return nil, fmt.Errorf("expected 'push <src>:<dst>', got %q", firstLine)
	}
	refspecs := []string{spec}
	for {
		line, err := stdin.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("reading push batch: %w", err)
		}
		line = strings.TrimRight(line, "\n\r")
		if line == "" {
			return refspecs, nil
		}
		spec, ok := strings.CutPrefix(line, "push ")
		if !ok {
			return nil, fmt.Errorf("expected 'push <src>:<dst>', got %q", line)
		}
		refspecs = append(refspecs, spec)
		if err == io.EOF {
			return refspecs, nil
		}
	}
}

func killAndWaitSendPack(sp *exec.Cmd, reason string) {
	if sp == nil || sp.Process == nil {
		return
	}
	if err := sp.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		debuglog.Printf("send-pack cleanup %s: kill: %v", reason, err)
	}
	if err := sp.Wait(); err != nil {
		debuglog.Printf("send-pack cleanup %s: wait: %v", reason, err)
	}
}
