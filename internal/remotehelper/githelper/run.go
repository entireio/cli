package githelper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

// Run drives the git-remote-helper protocol loop against the given
// Transport. It reads commands from stdin one line at a time and
// writes responses to stdout until git closes the pipe or sends an
// unsupported command (which terminates the loop with an error).
//
// protocolVersion selects which capability set to advertise. v2 (the
// upstream default since Git 2.26) advertises `stateless-connect` +
// `push` so Git negotiates wire-protocol v2 over framed HTTP RPC.
// v0/v1 advertises `connect`, giving Git a raw pipe to speak the
// legacy smart-HTTP protocol through. Upstream Git's
// transport-helper.c always prefers `connect` over `stateless-connect`
// when both are present, so the advertised set has to match the
// resolved protocol.version rather than offering both.
//
// The Transport interface decouples this loop from any specific HTTP
// implementation — see the entire/transport package for the
// production wiring.
func Run(ctx context.Context, t Transport, protocolVersion int, stdin io.Reader, stdout io.Writer) error {
	commandReader := bufio.NewReader(stdin)
	opts := &Options{}

	for {
		line, err := commandReader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" {
				return nil
			}
			if !errors.Is(err, io.EOF) {
				return fmt.Errorf("reading protocol input: %w", err)
			}
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		debuglog.Printf("command: %q", line)

		switch {
		case line == "capabilities":
			if protocolVersion >= 2 {
				fmt.Fprintln(stdout, "stateless-connect")
				fmt.Fprintln(stdout, "push")
			} else {
				fmt.Fprintln(stdout, "connect")
			}
			fmt.Fprintln(stdout, "option")
			fmt.Fprintln(stdout)

		case line == "list" || line == "list for-push":
			if err := handleList(ctx, t, line == "list for-push", stdout); err != nil {
				return err
			}

		case strings.HasPrefix(line, "option "):
			name, value, _ := strings.Cut(strings.TrimPrefix(line, "option "), " ")
			fmt.Fprintln(stdout, opts.Set(name, value))

		case strings.HasPrefix(line, "stateless-connect "):
			service := strings.TrimPrefix(line, "stateless-connect ")
			if err := handleStatelessConnect(ctx, t, service, commandReader, stdout); err != nil {
				return err
			}
			return nil

		case strings.HasPrefix(line, "connect "):
			service := strings.TrimPrefix(line, "connect ")
			if service != serviceUploadPack && service != serviceReceivePack {
				return fmt.Errorf("unsupported service: %s", service)
			}
			fmt.Fprintln(stdout)
			if err := handleConnect(ctx, t, service, commandReader, stdout); err != nil {
				return err
			}
			return nil

		case strings.HasPrefix(line, "push "):
			if err := handlePush(ctx, t, line, opts, commandReader, stdout); err != nil {
				return err
			}

		case line == "":
			if errors.Is(err, io.EOF) {
				return nil
			}
			continue

		default:
			return fmt.Errorf("unsupported command: %s", line)
		}
	}
}
