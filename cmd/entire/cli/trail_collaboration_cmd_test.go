package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestPrintTrailThreadDetail_ShowsMessageIDs(t *testing.T) {
	t.Parallel()
	// edit/delete take <thread-id> <message-id>; the text output must surface
	// the message and reply IDs so they are discoverable without --json.
	out := api.TrailThreadDetailResponse{
		Thread: api.TrailThreadSummary{ID: "th1", Title: "Design"},
		Messages: []api.TrailThreadMessage{{
			ID:     "msg-abc",
			Author: "alice",
			Body:   "top message",
			Replies: []api.TrailThreadReply{{
				ID:     "rep-xyz",
				Author: "bob",
				Body:   "a reply",
			}},
		}},
	}
	var buf bytes.Buffer
	if err := printTrailThreadDetail(&buf, out, false); err != nil {
		t.Fatalf("printTrailThreadDetail: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "msg-abc") {
		t.Errorf("output missing message ID %q:\n%s", "msg-abc", got)
	}
	if !strings.Contains(got, "rep-xyz") {
		t.Errorf("output missing reply ID %q:\n%s", "rep-xyz", got)
	}
}

func TestTrailThreadPathBuilders(t *testing.T) {
	t.Parallel()
	if got := trailThreadsPath("gh", "acme", "widgets", 7); !strings.HasSuffix(got, "/7/threads") {
		t.Errorf("threads path = %q", got)
	}
	if got := trailThreadPath("gh", "acme", "widgets", 7, "th1"); !strings.HasSuffix(got, "/7/threads/th1") {
		t.Errorf("thread path = %q", got)
	}
	if got := trailThreadMessagesPath("gh", "acme", "widgets", 7, "th1"); !strings.HasSuffix(got, "/threads/th1/messages") {
		t.Errorf("messages path = %q", got)
	}
	if got := trailThreadMessagePath("gh", "acme", "widgets", 7, "th1", "m1"); !strings.HasSuffix(got, "/threads/th1/messages/m1") {
		t.Errorf("message path = %q", got)
	}
}

func TestTrailCommentSubtreeWiring(t *testing.T) {
	t.Parallel()
	cmd := newTrailCommentCmd()
	want := map[string]bool{"list": false, "show": false, "add": false, "reply": false, "edit": false, "delete": false, "resolve": false, "unresolve": false}
	for _, c := range cmd.Commands() {
		want[c.Name()] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("trail comment missing subcommand %q", name)
		}
	}
	if cmd.PersistentFlags().Lookup("trail") == nil || cmd.PersistentFlags().Lookup("branch") == nil {
		t.Error("trail comment missing --trail/--branch persistent flags")
	}
}
