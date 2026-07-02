package githelper

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

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

	refs, err := t.InfoRefsV2(ctx)
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
		command := gitproto.V2Command(reqBody)
		if command != "" {
			debuglog.Printf("v2 command: %s", command)
		}

		resp, err := t.ServiceRPC(ctx, service, bytes.NewReader(reqBody), func(req *http.Request) {
			req.Header.Set("Git-Protocol", "version=2")
		})
		if err != nil {
			return fmt.Errorf("stateless-connect: POST: %w", err)
		}

		if command == "bundle-uri" {
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
		} else {
			if _, err := io.Copy(stdout, resp); err != nil {
				_ = resp.Close()
				return fmt.Errorf("streaming stateless response: %w", err)
			}
			if err := resp.Close(); err != nil {
				return fmt.Errorf("closing stateless response: %w", err)
			}
		}

		if _, err := stdout.Write([]byte("0002")); err != nil {
			return fmt.Errorf("writing response-end packet: %w", err)
		}
	}
}
