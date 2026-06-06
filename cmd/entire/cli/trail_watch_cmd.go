package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"

	"github.com/spf13/cobra"
)

// SSE control events for the agent-native code-review stream. Domain events
// (for example "session.started" or "comment.created") are emitted as their
// code_review_events.event_type values.
const (
	sseEventReady     = "ready"
	sseEventReconnect = "reconnect"
	sseEventForbidden = "forbidden"
	sseEventError     = "error"
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
		Short: "Tail a trail's code-review events live",
		Long: `Subscribe to the trail-scoped agent-native code-review SSE stream and
print events as they arrive. Reconnects automatically when the server caps the
connection (~50s) and on transient network errors.

If <number> is omitted, the trail for the current branch is used.

This command resolves the trail's id internally and streams
GET /api/v1/trails/<id>/reviews/events with Accept: text/event-stream.

Events emitted by the server:
  ready              initial frame, includes trail and cursor
  <event_type>       code-review domain event (session.started, comment.created, ...)
  reconnect          server cap reached; re-establishing
  forbidden          access was revoked; stream ends
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
	cmd.Flags().BoolVar(&once, "once", false, "Open one SSE connection then exit instead of reconnecting")

	return cmd
}

func runTrailWatch(cmd *cobra.Command, number int, jsonOutput, showPings, once bool) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	client, err := NewAuthenticatedAPIClient(ctx, trailInsecureHTTP(cmd))
	if err != nil {
		return fmt.Errorf("authentication required: %w", err)
	}

	trailID, description, err := resolveTrailWatchTarget(ctx, client, number)
	if err != nil {
		return err
	}
	streamPath := reviewEventsPath(trailID)

	fmt.Fprintf(errW, "Watching %s — Ctrl+C to stop\n", description)

	backoff := reconnectBackoffInitial
	lastEventID := ""

	for {
		closeReason, lastSeenID, err := streamOnce(ctx, client, streamPath, lastEventID, jsonOutput, showPings, w, errW)
		if lastSeenID != "" {
			lastEventID = lastSeenID
		}

		// Context cancelled (Ctrl+C) — exit cleanly.
		if ctx.Err() != nil {
			return nil //nolint:nilerr // ctx.Err() is the expected cancellation path; surface as clean exit
		}

		if once {
			switch closeReason {
			case streamCloseTerminal:
				return err
			case streamCloseForbidden:
				return NewSilentError(errors.New("stream access revoked"))
			case streamCloseError:
				if err == nil {
					err = errors.New("stream error reported by server")
				}
				return NewSilentError(err)
			case streamCloseTransport:
				return err
			case streamCloseReconnect, streamCloseDone:
				return nil
			}
		}

		switch closeReason {
		case streamCloseTerminal:
			return err
		case streamCloseDone:
			return nil
		case streamCloseForbidden:
			return NewSilentError(errors.New("stream access revoked"))
		case streamCloseReconnect:
			// Clean server-initiated reconnect (max_duration). No backoff.
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
	}
}

func resolveTrailWatchTarget(ctx context.Context, client *api.Client, number int) (trailID, description string, err error) {
	if number > 0 {
		return resolveTrailWatchNumber(ctx, client, number)
	}

	forge, owner, repo, err := resolveTrailRemote(ctx)
	if err != nil {
		return "", "", err
	}
	branch, err := GetCurrentBranch(ctx)
	if err != nil {
		return "", "", fmt.Errorf("no trail number given and current branch is unknown: %w", err)
	}
	found, err := findTrailByBranch(ctx, client, forge, owner, repo, branch)
	if err != nil {
		return "", "", err
	}
	if found == nil {
		return "", "", fmt.Errorf("no trail found for branch %q (pass an explicit trail number)", branch)
	}
	if found.ID == "" {
		return "", "", fmt.Errorf("trail for branch %q has no id yet", branch)
	}
	return found.ID, trailWatchDescription(forge, owner, repo, found.Number, found.ID), nil
}

func resolveTrailWatchNumber(ctx context.Context, client *api.Client, number int) (trailID, description string, err error) {
	forge, owner, repo, err := resolveTrailRemote(ctx)
	if err != nil {
		return "", "", err
	}
	found, err := findTrailByNumber(ctx, client, forge, owner, repo, number)
	if err != nil {
		return "", "", err
	}
	if found == nil {
		return "", "", fmt.Errorf("no trail #%d found in %s/%s/%s", number, forge, owner, repo)
	}
	if found.ID == "" {
		return "", "", fmt.Errorf("trail #%d has no id yet", number)
	}
	return found.ID, trailWatchDescription(forge, owner, repo, found.Number, found.ID), nil
}

func trailWatchDescription(forge, owner, repo string, number int, trailID string) string {
	if number > 0 {
		return fmt.Sprintf("trail #%d (%s/%s/%s, id %s)", number, forge, owner, repo, trailID)
	}
	return fmt.Sprintf("trail %s (%s/%s/%s)", trailID, forge, owner, repo)
}

func reviewEventsPath(trailID string) string {
	return "/api/v1/trails/" + url.PathEscape(trailID) + "/reviews/events"
}

type streamCloseReason int

const (
	streamCloseTransport streamCloseReason = iota // network/transport error
	streamCloseReconnect                          // server `event: reconnect`
	streamCloseForbidden                          // server `event: forbidden`
	streamCloseError                              // server `event: error`
	streamCloseDone                               // context cancellation
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
	jsonOutput, showPings bool,
	w, errW io.Writer,
) (streamCloseReason, string, error) {
	headers := http.Header{}
	headers.Set("Accept", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	if lastEventID != "" {
		headers.Set("Last-Event-ID", lastEventID)
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
	// review event carries long comment bodies; bump to 1 MiB to match the API's
	// per-comment limits.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		eventName  string
		dataLines  []string
		eventID    string // id of the in-progress frame (reset on flush)
		lastSeenID string // most recent SSE id from any frame that includes one
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
		case sseEventReconnect:
			return streamCloseReconnect, true
		case sseEventForbidden:
			return streamCloseForbidden, true
		case sseEventError:
			return streamCloseError, true
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
		printReadyEvent(w, data)
		return
	case sseEventReconnect:
		fmt.Fprintln(errW, "↻ server requested reconnect")
		return
	case sseEventForbidden:
		fmt.Fprintln(errW, "✖ stream access revoked")
		return
	case sseEventError:
		printStreamError(errW, data)
		return
	}

	if ev, ok := parseReviewStreamEvent(eventName, data); ok {
		printReviewStreamEvent(w, ev)
		return
	}

	// Fallback for unknown events or unparseable payloads: print raw.
	fmt.Fprintf(w, "%s: %s\n", eventName, truncateForLog(data, 500))
}

type reviewReadyPayload struct {
	TrailID string `json:"trail_id"`
	Cursor  int    `json:"cursor"`
}

type reviewStreamEvent struct {
	ID              any            `json:"id"`
	TrailID         string         `json:"trail_id"`
	ReviewSessionID *string        `json:"review_session_id"`
	ActorID         string         `json:"actor_id"`
	EventType       string         `json:"event_type"`
	TargetType      string         `json:"target_type"`
	TargetID        string         `json:"target_id"`
	Payload         map[string]any `json:"payload"`
	CreatedAt       time.Time      `json:"created_at"`
}

func printReadyEvent(w io.Writer, data string) {
	var p reviewReadyPayload
	if err := json.Unmarshal([]byte(data), &p); err == nil {
		parts := []string{"● connected"}
		if p.TrailID != "" {
			parts = append(parts, "to trail "+p.TrailID)
		}
		if p.Cursor > 0 {
			parts = append(parts, fmt.Sprintf("after event %d", p.Cursor))
		}
		fmt.Fprintln(w, strings.Join(parts, " "))
		return
	}
	fmt.Fprintf(w, "ready: %s\n", truncateForLog(data, 500))
}

func printStreamError(errW io.Writer, data string) {
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
}

func parseReviewStreamEvent(eventName, data string) (reviewStreamEvent, bool) {
	var ev reviewStreamEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return ev, false
	}
	if ev.EventType == "" {
		ev.EventType = eventName
	}
	if ev.EventType == "" && ev.TargetType == "" && ev.TargetID == "" {
		return ev, false
	}
	return ev, true
}

func printReviewStreamEvent(w io.Writer, ev reviewStreamEvent) {
	prefix := ""
	if !ev.CreatedAt.IsZero() {
		prefix = "[" + ev.CreatedAt.Local().Format("15:04:05") + "] "
	}
	actor := ev.ActorID
	if actor == "" {
		actor = "unknown actor"
	}

	switch ev.EventType {
	case "code_version.created":
		fmt.Fprintf(w, "%scode version %s created (head %s)\n", prefix, ev.TargetID, payloadString(ev.Payload, "head_sha"))
	case "code_version.base_sha_set":
		fmt.Fprintf(w, "%scode version %s base set to %s\n", prefix, ev.TargetID, payloadString(ev.Payload, "base_sha"))
	case "session.started":
		fmt.Fprintf(w, "%ssession started by %s (code version %s)\n", prefix, actor, payloadString(ev.Payload, "code_version_id"))
	case "session.ended":
		reason := payloadString(ev.Payload, "reason")
		if reason != "" {
			fmt.Fprintf(w, "%ssession ended by %s (%s)\n", prefix, actor, reason)
		} else {
			fmt.Fprintf(w, "%ssession ended by %s\n", prefix, actor)
		}
	case "comment.created":
		file := payloadString(ev.Payload, "file_path")
		severity := payloadString(ev.Payload, "severity")
		switch {
		case file != "" && severity != "":
			fmt.Fprintf(w, "%scomment created by %s on %s (%s) — %s\n", prefix, actor, file, severity, ev.TargetID)
		case file != "":
			fmt.Fprintf(w, "%scomment created by %s on %s — %s\n", prefix, actor, file, ev.TargetID)
		default:
			fmt.Fprintf(w, "%scomment created by %s — %s\n", prefix, actor, ev.TargetID)
		}
	case "comment.status_changed":
		fmt.Fprintf(w, "%scomment %s status %s → %s\n", prefix, ev.TargetID, payloadString(ev.Payload, "from"), payloadString(ev.Payload, "to"))
	case "comment.updated":
		fmt.Fprintf(w, "%scomment %s updated by %s\n", prefix, ev.TargetID, actor)
	case "comment.stale_checked":
		fmt.Fprintf(w, "%scomment %s marked %s (%s)\n", prefix, ev.TargetID, payloadString(ev.Payload, "outcome"), payloadString(ev.Payload, "reason"))
	case "suggested_change.created":
		fmt.Fprintf(w, "%ssuggested change %s created for comment %s (%s)\n", prefix, ev.TargetID, payloadString(ev.Payload, "review_comment_id"), payloadString(ev.Payload, "change_type"))
	case "suggested_change.updated":
		fmt.Fprintf(w, "%ssuggested change %s updated by %s\n", prefix, ev.TargetID, actor)
	case "suggested_change.check_result", "suggested_change.apply_result":
		fmt.Fprintf(w, "%s%s for %s: %s\n", prefix, ev.EventType, payloadString(ev.Payload, "suggested_change_id"), payloadString(ev.Payload, "status"))
	case "thread.created":
		fmt.Fprintf(w, "%sthread %s created for comment %s\n", prefix, ev.TargetID, payloadString(ev.Payload, "review_comment_id"))
	case "thread.message_added":
		fmt.Fprintf(w, "%sthread message %s added by %s\n", prefix, ev.TargetID, actor)
	case "thread.message_edited":
		fmt.Fprintf(w, "%sthread message %s edited by %s\n", prefix, ev.TargetID, actor)
	case "comment.linked":
		fmt.Fprintf(w, "%scomment link created: %s → %s\n", prefix, payloadString(ev.Payload, "source_comment_id"), payloadString(ev.Payload, "target_comment_id"))
	case "comment.unlinked":
		fmt.Fprintf(w, "%scomment link removed: %s → %s\n", prefix, payloadString(ev.Payload, "source_comment_id"), payloadString(ev.Payload, "target_comment_id"))
	default:
		fmt.Fprintf(w, "%s%s %s/%s by %s\n", prefix, ev.EventType, ev.TargetType, ev.TargetID, actor)
	}
}

func payloadString(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	v, ok := payload[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprint(t)
	}
}

// truncateForLog clips body text on a rune boundary so a single multi-line
// payload doesn't blow up the watch view. Newlines collapse to spaces.
func truncateForLog(s string, maxRunes int) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	return string(rs[:maxRunes]) + "…"
}
