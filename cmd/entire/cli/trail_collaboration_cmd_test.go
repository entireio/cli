package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestPrintTrailDiscussionDetail_ShowsMessageIDs(t *testing.T) {
	t.Parallel()
	// edit/delete take <discussion-id> <message-id>; the text output must surface
	// the message and reply IDs so they are discoverable without --json.
	out := api.TrailDiscussionDetailResponse{
		Discussion: api.TrailDiscussionSummary{ID: "th1", Title: "Design"},
		Messages: []api.TrailDiscussionMessage{{
			ID:     "msg-abc",
			Author: "alice",
			Body:   "top message",
			Replies: []api.TrailDiscussionReply{{
				ID:     "rep-xyz",
				Author: "bob",
				Body:   "a reply",
			}},
		}},
	}
	var buf bytes.Buffer
	if err := printTrailDiscussionDetail(&buf, out, false); err != nil {
		t.Fatalf("printTrailDiscussionDetail: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "Discussion th1 [unresolved]: Design") {
		t.Errorf("discussion heading missing: %s", got)
	}
	if !strings.Contains(got, "msg-abc") {
		t.Errorf("output missing message ID %q:\n%s", "msg-abc", got)
	}
	if !strings.Contains(got, "rep-xyz") {
		t.Errorf("output missing reply ID %q:\n%s", "rep-xyz", got)
	}
}

func TestTrailDiscussionPathBuilders(t *testing.T) {
	t.Parallel()
	for _, basePath := range []string{"/api/v1/repos/native-repo-id/trails", "/api/v1/trails/gh/acme/widget"} {
		t.Run(basePath, func(t *testing.T) {
			t.Parallel()
			if got := trailDiscussionsPath(basePath, 7); got != basePath+"/7/discussions" {
				t.Errorf("discussions path = %q", got)
			}
			if got := trailDiscussionPath(basePath, 7, "th1"); got != basePath+"/7/discussions/th1" {
				t.Errorf("discussion path = %q", got)
			}
			if got := trailDiscussionMessagesPath(basePath, 7, "th1"); got != basePath+"/7/discussions/th1/messages" {
				t.Errorf("messages path = %q", got)
			}
			if got := trailDiscussionMessagePath(basePath, 7, "th1", "m1"); got != basePath+"/7/discussions/th1/messages/m1" {
				t.Errorf("message path = %q", got)
			}
		})
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

func TestTrailCommentDiscussionHelp(t *testing.T) {
	t.Parallel()
	cmd := newTrailCommentCmd()
	if cmd.Short != "Manage discussions on a trail" {
		t.Errorf("comment summary = %q", cmd.Short)
	}
	for _, tc := range []struct{ name, usage string }{
		{"show", "show <discussion-id>"},
		{"reply", "reply <discussion-id>"},
		{"edit", "edit <discussion-id> <message-id>"},
		{"delete", "delete <discussion-id> <message-id>"},
		{"resolve", "resolve <discussion-id>"},
		{"unresolve", "unresolve <discussion-id>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			child, _, err := newTrailCommentCmd().Find([]string{tc.name})
			if err != nil {
				t.Fatal(err)
			}
			if child.Use != tc.usage {
				t.Errorf("usage = %q, want %q", child.Use, tc.usage)
			}
		})
	}
}
