package githelper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
)

// handleList implements the git-remote-helpers "list" / "list for-push"
// command. It fetches the v0 info/refs advertisement, extracts each
// ref and HEAD's symref target from the embedded capabilities, and
// writes one "<value> <name>" line per ref followed by a blank-line
// terminator. HEAD is emitted as "@<target> HEAD" when the symref
// capability resolves; detached HEAD falls back to "<sha> HEAD".
func handleList(ctx context.Context, t Transport, forPush bool, stdout io.Writer) error {
	service := serviceUploadPack
	if forPush {
		service = serviceReceivePack
	}
	refs, err := t.InfoRefs(ctx, service)
	if err != nil {
		return fmt.Errorf("list %s info/refs: %w", service, err)
	}
	defer refs.Close()

	r := bufio.NewReader(refs)
	var reply packp.SmartReply
	if err := reply.Decode(r); err != nil {
		return fmt.Errorf("parsing info/refs: %w", err)
	}

	type entry struct{ value, name string }
	var (
		lines        []entry
		headValue    string
		objectFormat string
		symrefs      = map[string]string{}
		first        = true
	)
	for {
		_, content, err := pktline.ReadLine(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("reading ref pkt-line: %w", err)
		}
		if len(content) == 0 {
			// Flush, delim, or response-end. For the v0 ref
			// advertisement only flush is meaningful — it ends
			// the list.
			break
		}
		line := strings.TrimRight(string(content), "\n")

		// The first advertised ref carries the capability list after a
		// NUL separator: "<sha> <ref>\0<cap1> <cap2> ...". Subsequent
		// refs are bare. We pluck every symref=src:dst pair and
		// object-format=<hash> out of those caps; the rest is for the
		// smart-transport client only.
		if first {
			first = false
			var caps string
			if i := strings.IndexByte(line, 0); i >= 0 {
				caps = line[i+1:]
				line = line[:i]
			}
			for c := range strings.FieldsSeq(caps) {
				switch {
				case strings.HasPrefix(c, "object-format="):
					objectFormat = strings.TrimPrefix(c, "object-format=")
				case strings.HasPrefix(c, "symref="):
					if src, dst, ok := strings.Cut(strings.TrimPrefix(c, "symref="), ":"); ok {
						symrefs[src] = dst
					}
				}
			}
		}

		value, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		// Empty repos are advertised with a single dummy ref
		// "<null-oid> capabilities^{}" so the server still gets a
		// chance to send its capability list. Skip it — it's not a
		// real ref and transport-helper would reject the name.
		if name == "capabilities^{}" {
			continue
		}
		if name == "HEAD" {
			headValue = value
			continue
		}
		lines = append(lines, entry{value, name})
	}

	if objectFormat != "" {
		if _, err := fmt.Fprintf(stdout, ":object-format %s\n", objectFormat); err != nil {
			return fmt.Errorf("writing object-format: %w", err)
		}
	}
	for _, l := range lines {
		value := l.value
		if target, ok := symrefs[l.name]; ok {
			value = "@" + target
		}
		if _, err := fmt.Fprintf(stdout, "%s %s\n", value, l.name); err != nil {
			return fmt.Errorf("writing ref: %w", err)
		}
	}
	// Emit HEAD whenever we know something about it: either a SHA
	// (normal or detached HEAD) or a symref target (unborn HEAD on an
	// empty repo where the server advertises
	// symref=HEAD:refs/heads/main but no SHA). transport-helper.c:1276
	// accepts "@<target> HEAD" without a SHA and resolves the OID via
	// resolve_remote_symref.
	if target, ok := symrefs["HEAD"]; ok {
		if _, err := fmt.Fprintf(stdout, "@%s HEAD\n", target); err != nil {
			return fmt.Errorf("writing HEAD symref: %w", err)
		}
	} else if headValue != "" {
		if _, err := fmt.Fprintf(stdout, "%s HEAD\n", headValue); err != nil {
			return fmt.Errorf("writing HEAD: %w", err)
		}
	}
	if _, err := fmt.Fprintln(stdout); err != nil {
		return fmt.Errorf("writing terminator: %w", err)
	}
	return nil
}
