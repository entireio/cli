package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/trail"

	"github.com/spf13/cobra"
)

// SSE constants for the trail code-review stream. Field names mirror the
// client spec in entirehq/entire.io docs/trail-code-review-stream.md.
const (
	sseEventReady          = "ready"
	sseEventComment        = "comment"
	sseEventCommentDeleted = "comment_deleted"
	sseEventReconnect      = "reconnect"
	sseEventDeleted        = "deleted"
	sseEventError          = "error"
)

// reconnectBackoffInitial / reconnectBackoffCap bound the exponential backoff
// used after network errors. The server's max stream duration (~50s) closes
// cleanly with an `event: reconnect`; in that case we reconnect immediately
// with no backoff.
const (
	reconnectBackoffInitial = 500 * time.Millisecond
	reconnectBackoffCap     = 30 * time.Second
)

func newTrailWatchCmd() *cobra.Command {
	var (
		jsonOutput bool
		showPings  bool
		once       bool
		number     int
	)

	cmd := &cobra.Command{
		Use:   "watch [<number>]",
		Short: "Tail a trail's code review (discussion) live",
		Long: `Subscribe to the SSE stream of a trail's code-review discussion and
print events as they arrive. Reconnects automatically when the server
caps the connection (~50s) and on transient network errors.

If <number> is omitted, the trail for the current branch is used.

Events emitted by the server:
  ready              initial frame, includes existing comment count
  comment            comment added or edited (with full payload)
  comment_deleted    comment removed
  reconnect          server cap reached; re-establishing
  deleted            trail row deleted; stream ends
  error              server-side error; treated as reconnect`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				n, err := strconv.Atoi(args[0])
				if err != nil || n <= 0 {
					return fmt.Errorf("invalid trail number %q", args[0])
				}
				number = n
			}
			return runTrailWatch(cmd, number, jsonOutput, showPings, once)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print each event as a single JSON line")
	cmd.Flags().BoolVar(&showPings, "show-pings", false, "Print SSE keepalive pings (otherwise suppressed)")
	cmd.Flags().BoolVar(&once, "once", false, "Drain the initial replay then exit (no reconnect, no live tail)")

	return cmd
}

func runTrailWatch(cmd *cobra.Command, number int, jsonOutput, showPings, once bool) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	client, err := NewAuthenticatedAPIClient(trailInsecureHTTP(cmd))
	if err != nil {
		return fmt.Errorf("authentication required: %w", err)
	}

	host, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return fmt.Errorf("failed to resolve repository: %w", err)
	}

	// Resolve trail number if not provided: look it up by current branch.
	if number == 0 {
		branch, err := GetCurrentBranch(ctx)
		if err != nil {
			return fmt.Errorf("no trail number given and current branch is unknown: %w", err)
		}
		found, err := findTrailByBranch(ctx, client, host, owner, repo, branch)
		if err != nil {
			return err
		}
		if found == nil {
			return fmt.Errorf("no trail found for branch %q (pass an explicit trail number)", branch)
		}
		if found.Number <= 0 {
			return fmt.Errorf("trail for branch %q has no numeric identifier yet", branch)
		}
		number = found.Number
	}

	streamPath := fmt.Sprintf("%s/%d/code-review/stream", trailsBasePath(host, owner, repo), number)

	fmt.Fprintf(errW, "Watching trail #%d on %s/%s/%s — Ctrl+C to stop\n", number, host, owner, repo)

	backoff := reconnectBackoffInitial
	lastEventID := ""
	resumed := false

	for {
		closeReason, lastSeenID, err := streamOnce(ctx, client, streamPath, lastEventID, resumed, jsonOutput, showPings, once, w, errW)
		if lastSeenID != "" {
			lastEventID = lastSeenID
		}

		// Context cancelled (Ctrl+C) — exit cleanly.
		if ctx.Err() != nil {
			return nil //nolint:nilerr // ctx.Err() is the expected cancellation path; surface as clean exit
		}

		switch closeReason {
		case streamCloseTerminal:
			return err
		case streamCloseDeleted, streamCloseDone:
			return nil
		case streamCloseReconnect:
			// Clean server-initiated reconnect (max_duration). No backoff.
			resumed = true
			backoff = reconnectBackoffInitial
			continue
		case streamCloseError:
			// Server emitted `event: error` — reconnect with backoff.
			fmt.Fprintf(errW, "stream error reported by server, reconnecting in %s\n", backoff)
		case streamCloseTransport:
			// Network/transport error.
			if err != nil {
				fmt.Fprintf(errW, "stream disconnected (%v), reconnecting in %s\n", err, backoff)
			}
		}

		// Sleep with cancellation.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > reconnectBackoffCap {
			backoff = reconnectBackoffCap
		}
		resumed = true
	}
}

type streamCloseReason int

const (
	streamCloseTransport streamCloseReason = iota // network/transport error
	streamCloseReconnect                          // server `event: reconnect`
	streamCloseDeleted                            // server `event: deleted`
	streamCloseError                              // server `event: error`
	streamCloseDone                               // local --once / EOF after replay
	streamCloseTerminal                           // non-recoverable HTTP status (401/403/404/410)
)

// terminalHTTPStatuses are HTTP response codes for which retrying the SSE
// stream cannot succeed: auth failures (401/403) and resource-not-found-style
// errors (404/410). 429 is intentionally *not* terminal — it's a transient
// rate-limit signal that should back off and retry.
var terminalHTTPStatuses = [...]int{
	http.StatusUnauthorized, // 401
	http.StatusForbidden,    // 403
	http.StatusNotFound,     // 404
	http.StatusGone,         // 410
}

func isTerminalHTTPError(err error) bool {
	for _, code := range terminalHTTPStatuses {
		if api.IsHTTPErrorStatus(err, code) {
			return true
		}
	}
	return false
}

// streamOnce opens a single SSE connection, prints events until it closes for
// any reason, and returns the close reason plus the last `id:` observed (so
// the caller can pass it as `Last-Event-ID` on reconnect).
//
//nolint:cyclop,gocognit // SSE framing parser is naturally branchy; splitting hurts readability
func streamOnce(
	ctx context.Context,
	client *api.Client,
	path string,
	lastEventID string,
	resumed bool,
	jsonOutput, showPings, once bool,
	w, errW io.Writer,
) (streamCloseReason, string, error) {
	headers := http.Header{}
	headers.Set("Accept", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	if lastEventID != "" {
		headers.Set("Last-Event-ID", lastEventID)
	} else if resumed {
		// First reconnect after a transport error with no id seen: use the
		// query-param form to suppress replay anyway.
		path += "?replay=false"
	}

	resp, err := client.GetStream(ctx, path, headers)
	if err != nil {
		return streamCloseTransport, "", fmt.Errorf("open SSE stream: %w", err)
	}
	defer resp.Body.Close()

	if err := checkTrailResponse(resp); err != nil {
		// Terminal: surface the error and don't reconnect. 429 deliberately
		// falls through to streamCloseTransport so the caller backs off.
		if isTerminalHTTPError(err) {
			return streamCloseTerminal, "", err
		}
		return streamCloseTransport, "", err
	}

	scanner := bufio.NewScanner(resp.Body)
	// SSE frames can be larger than the default 64KiB scanner buffer when a
	// trail has long comment bodies; bump to 1 MiB to match the API's
	// per-comment limits.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		eventName    string
		dataLines    []string
		eventID      string // id of the in-progress frame (reset on flush)
		lastSeenID   string // most recent SSE id from any frame that includes one
		seenReady    bool
		remainReplay int  // --once: comment events still to drain after ready
		onceExitNext bool // --once: exit after this flush
	)

	flush := func() (streamCloseReason, bool) {
		defer func() {
			eventName = ""
			dataLines = nil
			eventID = ""
		}()
		if eventName == "" && len(dataLines) == 0 {
			return streamCloseTransport, false
		}
		data := strings.Join(dataLines, "\n")
		printSSEEvent(w, errW, eventName, data, jsonOutput)

		if eventID != "" {
			lastSeenID = eventID
		}

		switch eventName {
		case sseEventReady:
			seenReady = true
			if once {
				var p struct {
					CommentCount int  `json:"commentCount"`
					Resumed      bool `json:"resumed"`
				}
				if jerr := json.Unmarshal([]byte(data), &p); jerr != nil {
					fmt.Fprintf(errW, "Warning: malformed ready payload: %v\n", jerr)
				}
				if p.Resumed || p.CommentCount == 0 {
					onceExitNext = true
				} else {
					remainReplay = p.CommentCount
				}
			}
		case sseEventComment:
			if once && seenReady && remainReplay > 0 {
				remainReplay--
				if remainReplay == 0 {
					onceExitNext = true
				}
			}
		case sseEventReconnect:
			return streamCloseReconnect, true
		case sseEventDeleted:
			return streamCloseDeleted, true
		case sseEventError:
			return streamCloseError, true
		}
		if onceExitNext {
			return streamCloseDone, true
		}
		return streamCloseTransport, false
	}

	for scanner.Scan() {
		line := scanner.Text()

		// Blank line dispatches the event.
		if line == "" {
			if reason, done := flush(); done {
				return reason, lastSeenID, nil
			}
			continue
		}

		// Comment / keepalive line.
		if strings.HasPrefix(line, ":") {
			if showPings {
				fmt.Fprintln(errW, "ping:", strings.TrimSpace(strings.TrimPrefix(line, ":")))
			}
			continue
		}

		field, value, ok := strings.Cut(line, ":")
		if !ok {
			// Field-only line (no colon) — per spec the value is empty.
			field = line
			value = ""
		}
		// Per SSE spec: a single leading space after the colon is ignored.
		value = strings.TrimPrefix(value, " ")

		switch field {
		case "event":
			eventName = value
		case "data":
			dataLines = append(dataLines, value)
		case "id":
			eventID = value
		case "retry":
			// Ignored — we manage backoff client-side.
		}
	}

	if err := scanner.Err(); err != nil {
		// Context cancellation surfaces here as a wrapped "context canceled".
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return streamCloseDone, lastSeenID, nil
		}
		return streamCloseTransport, lastSeenID, fmt.Errorf("read SSE stream: %w", err)
	}
	return streamCloseTransport, lastSeenID, io.ErrUnexpectedEOF
}

// printSSEEvent renders a single SSE event in either human-readable or
// json-line form. `data` may be empty (e.g. for the rare frame with only an
// `event:` line) — we still print something so the caller sees it.
func printSSEEvent(w, errW io.Writer, eventName, data string, jsonOutput bool) {
	if jsonOutput {
		// Emit a JSON envelope so consumers can reliably parse with
		// `jq`/scripts. The data field is preserved verbatim if it isn't
		// valid JSON; otherwise it's inlined as a sub-object.
		envelope := map[string]any{"event": eventName}
		if data != "" {
			var sub any
			if err := json.Unmarshal([]byte(data), &sub); err == nil {
				envelope["data"] = sub
			} else {
				envelope["data"] = data
			}
		}
		out, err := json.Marshal(envelope)
		if err != nil {
			fmt.Fprintf(errW, "failed to marshal event: %v\n", err)
			return
		}
		fmt.Fprintln(w, string(out))
		return
	}

	switch eventName {
	case sseEventReady:
		var p struct {
			Repo         string `json:"repo"`
			TrailNumber  int    `json:"trailNumber"`
			CommentCount int    `json:"commentCount"`
			Resumed      bool   `json:"resumed"`
		}
		if err := json.Unmarshal([]byte(data), &p); err == nil {
			if p.Resumed {
				fmt.Fprintf(w, "● connected to %s trail #%d (resumed; %d comment(s))\n",
					p.Repo, p.TrailNumber, p.CommentCount)
			} else {
				fmt.Fprintf(w, "● connected to %s trail #%d (%d comment(s))\n",
					p.Repo, p.TrailNumber, p.CommentCount)
			}
			return
		}
	case sseEventComment:
		var p struct {
			UpdatedAt time.Time     `json:"updatedAt"`
			Comment   trail.Comment `json:"comment"`
		}
		if err := json.Unmarshal([]byte(data), &p); err == nil {
			ts := p.UpdatedAt.Local().Format("15:04:05")
			body := truncateForLog(p.Comment.Body, 200)
			fmt.Fprintf(w, "[%s] %s: %s\n", ts, p.Comment.Author, body)
			for _, r := range p.Comment.Replies {
				fmt.Fprintf(w, "    └─ %s: %s\n", r.Author, truncateForLog(r.Body, 200))
			}
			return
		}
	case sseEventCommentDeleted:
		var p struct {
			UpdatedAt time.Time `json:"updatedAt"`
			CommentID string    `json:"commentId"`
		}
		if err := json.Unmarshal([]byte(data), &p); err == nil {
			ts := p.UpdatedAt.Local().Format("15:04:05")
			fmt.Fprintf(w, "[%s] (deleted comment %s)\n", ts, p.CommentID)
			return
		}
	case sseEventReconnect:
		fmt.Fprintln(errW, "↻ server requested reconnect")
		return
	case sseEventDeleted:
		fmt.Fprintln(errW, "✖ trail was deleted")
		return
	case sseEventError:
		var p struct {
			Message string `json:"message"`
		}
		if jerr := json.Unmarshal([]byte(data), &p); jerr != nil {
			// Best-effort: server payload may be missing or malformed; we still
			// want to surface that an error event arrived.
			fmt.Fprintf(errW, "Warning: malformed error payload: %v\n", jerr)
		}
		if p.Message != "" {
			fmt.Fprintf(errW, "✖ stream error: %s\n", p.Message)
		} else {
			fmt.Fprintln(errW, "✖ stream error")
		}
		return
	}

	// Fallback for unknown events or unparseable payloads: print raw.
	fmt.Fprintf(w, "%s: %s\n", eventName, data)
}

// truncateForLog clips body text on a rune boundary so a single multi-line
// comment doesn't blow up the watch view. Newlines collapse to spaces.
func truncateForLog(s string, maxRunes int) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	return string(rs[:maxRunes]) + "…"
}
