package githelper

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/entireio/cli/internal/remotehelper/debuglog"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
)

// handleStatelessConnect proxies Git protocol v2 upload-pack. The
// remote helper contract is the same shape as git-remote-curl: first
// prove the server speaks v2 by fetching info/refs with
// Git-Protocol: version=2, then write the server's capability
// advertisement and relay each flush-terminated request body as an
// independent POST.
func handleStatelessConnect(ctx context.Context, t Transport, service string, stdin io.Reader, stdout io.Writer) error {
	switch service {
	case serviceUploadPack:
	case serviceReceivePack:
		fmt.Fprintln(stdout)
		return handleConnect(ctx, t, service, stdin, stdout)
	default:
		fmt.Fprintln(stdout, "fallback")
		return nil
	}

	infoRefs := t.InfoRefsV2ForFetch
	if helperChunkingOff() {
		infoRefs = t.InfoRefsV2
	}
	refs, err := infoRefs(ctx)
	if err != nil {
		return fmt.Errorf("stateless-connect v2 info/refs: %w", err)
	}
	defer refs.Close()

	advertisement, err := io.ReadAll(refs)
	if err != nil {
		return fmt.Errorf("reading v2 info/refs: %w", err)
	}
	if !gitproto.IsV2Advertisement(advertisement) {
		fmt.Fprintln(stdout, "fallback")
		return nil
	}

	// bundle-uri is left for native git to drive — we don't intercept
	// the catalogue or inject auth here. If the server advertises
	// bundle-uri and the user opted in via transfer.bundleURI=true,
	// they're responsible for configuring credentials for the bundle
	// host (e.g. via credential.helper / netrc /
	// http.<url>.extraHeader). Previously we generated a temp config
	// with a Bearer token in plain text on disk to make this Just
	// Work; the security trade-off wasn't worth the convenience.

	// Each fetch POST is proxied to a replica, so a server-side want-ref
	// lookup could race the ls-refs answer git already relayed. Hide
	// ref-in-want so git sends explicit want <oid> lines. The server
	// hides it too, but only for the default User-Agent.
	advertisement, err = gitproto.RemoveV2FetchFeature(advertisement, "ref-in-want")
	if err != nil {
		return fmt.Errorf("stateless-connect: strip ref-in-want: %w", err)
	}
	chunked := newChunkedFetch(t, advertisement)
	defer chunked.close()
	// The helper speaks entire-bb-fetch for git, which has no use for it.
	advertisement, err = gitproto.RemoveV2FetchFeature(advertisement, featureBBFetch)
	if err != nil {
		return fmt.Errorf("stateless-connect: strip %s: %w", featureBBFetch, err)
	}

	fmt.Fprintln(stdout)
	if _, err := stdout.Write(advertisement); err != nil {
		return fmt.Errorf("streaming v2 capabilities: %w", err)
	}

	for {
		reqBody, ok, err := gitproto.ReadFlushTerminatedMessage(stdin)
		if err != nil {
			return fmt.Errorf("stateless-connect: read request: %w", err)
		}
		if !ok {
			return nil
		}
		reqBody, err = gitproto.AppendAgentToV2Request(reqBody, Agent)
		if err != nil {
			return fmt.Errorf("stateless-connect: amend agent: %w", err)
		}
		if err := relayV2Request(ctx, t, service, chunked, reqBody, stdout); err != nil {
			return err
		}
	}
}

// relayV2Request sends one v2 command request and copies its response to
// stdout, followed by the response-end packet.
func relayV2Request(ctx context.Context, t Transport, service string, chunked *chunkedFetch, reqBody []byte, stdout io.Writer) error {
	command := gitproto.V2Command(reqBody)
	if command != "" {
		debuglog.Printf("v2 command: %s", command)
	}
	var err error
	if chunked.handles(ctx, command) {
		err = chunked.fetch(ctx, reqBody, stdout)
	} else {
		err = relayPlain(ctx, t, service, command, reqBody, stdout)
	}
	if err != nil {
		return err
	}
	if _, err := stdout.Write([]byte("0002")); err != nil {
		return fmt.Errorf("writing response-end packet: %w", err)
	}
	return nil
}

// relayPlain sends a request as it is and copies the response to stdout.
func relayPlain(ctx context.Context, t Transport, service, command string, reqBody []byte, stdout io.Writer) error {
	resp, err := t.ServiceRPC(ctx, service, bytes.NewReader(reqBody), protocolV2Header)
	if err != nil {
		return fmt.Errorf("stateless-connect: POST: %w", err)
	}
	if command != "bundle-uri" {
		return copyResponse(stdout, resp)
	}
	respBody, readErr := io.ReadAll(resp)
	if readErr != nil {
		_ = resp.Close()
		return fmt.Errorf("reading bundle-uri response: %w", readErr)
	}
	if err := resp.Close(); err != nil {
		return fmt.Errorf("closing bundle-uri response: %w", err)
	}
	if len(respBody) == 0 {
		debuglog.Printf("bundle-uri response was empty; synthesizing flush packet")
		respBody = []byte("0000")
	}
	if _, err := stdout.Write(respBody); err != nil {
		return fmt.Errorf("streaming bundle-uri response: %w", err)
	}
	return nil
}
