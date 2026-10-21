package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestWriteUnattributedAuthorsLine_Prints covers the sentence status prints
// for one or more unattributed authors (COR-1289): singular vs plural
// "commit", and one line per author when there is more than one.
func TestWriteUnattributedAuthorsLine_Prints(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		authors []unattributedAuthor
		want    string
	}{
		"plural": {
			authors: []unattributedAuthor{{Email: "me@h.local", Count: 9}},
			want:    "Entire has 9 unattributed commits in this repo authored by me@h.local. Run `entire doctor` to link that address to your account.\n",
		},
		"singular": {
			authors: []unattributedAuthor{{Email: "me@h.local", Count: 1}},
			want:    "Entire has 1 unattributed commit in this repo authored by me@h.local. Run `entire doctor` to link that address to your account.\n",
		},
		"two authors, one line each": {
			authors: []unattributedAuthor{
				{Email: "me@h.local", Count: 9},
				{Email: "me@fritz.box", Count: 2},
			},
			want: "Entire has 9 unattributed commits in this repo authored by me@h.local. Run `entire doctor` to link that address to your account.\n" +
				"Entire has 2 unattributed commits in this repo authored by me@fritz.box. Run `entire doctor` to link that address to your account.\n",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			sty := newStatusStyles(&buf)
			writeUnattributedAuthorsLine(&buf, sty, detectionOutcome{LoggedIn: true, Authors: tc.authors})

			if got := buf.String(); got != tc.want {
				t.Errorf("writeUnattributedAuthorsLine() output = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWriteUnattributedAuthorsLine_Silent covers every degrade: status points
// at doctor only on a clean, logged-in, non-skipped hit with authors. Any
// other outcome shape prints nothing — doctor is where reasons are shown.
// Both writeUnattributedAuthorsLine (text) and linkableAuthors (the gate
// runStatusJSON also uses) share the same predicate, so this table covers
// both renderers at once.
func TestWriteUnattributedAuthorsLine_Silent(t *testing.T) {
	t.Parallel()

	tests := map[string]detectionOutcome{
		"logged out with candidates": {
			LoggedIn:   false,
			Candidates: []string{"me@h.local"},
		},
		"skipped": {
			LoggedIn: true,
			Skipped:  "timed out reaching Entire",
		},
		"no candidates": {
			LoggedIn: true,
		},
		"logged in with empty authors": {
			LoggedIn:   true,
			Candidates: []string{"me@h.local"},
			Authors:    nil,
		},
	}

	for name, out := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			sty := newStatusStyles(&buf)
			writeUnattributedAuthorsLine(&buf, sty, out)

			if got := buf.String(); got != "" {
				t.Errorf("writeUnattributedAuthorsLine() output = %q, want empty", got)
			}
			if authors := out.linkableAuthors(); len(authors) != 0 {
				t.Errorf("linkableAuthors() = %+v, want empty (this is runStatusJSON's gate too)", authors)
			}
		})
	}
}

// TestStatusJSON_UnattributedAuthorsField pins the wire shape of the new
// unattributed_authors field: an empty (never null) list on the success path,
// and one entry per detected author (COR-1289).
func TestStatusJSON_UnattributedAuthorsField(t *testing.T) {
	t.Parallel()

	empty := statusJSON{UnattributedAuthors: make([]unattributedAuthor, 0)}
	b, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if !strings.Contains(string(b), `"unattributed_authors":[]`) {
		t.Errorf("json.Marshal(empty) = %s, want it to contain %q", b, `"unattributed_authors":[]`)
	}

	withAuthor := statusJSON{UnattributedAuthors: []unattributedAuthor{{Email: "me@h.local", Count: 9}}}
	b, err = json.Marshal(withAuthor)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	want := `"unattributed_authors":[{"email":"me@h.local","count":9}]`
	if !strings.Contains(string(b), want) {
		t.Errorf("json.Marshal(withAuthor) = %s, want it to contain %q", b, want)
	}
}

// TestRunStatus_NoUnattributedAuthorsLineWhenLoggedOut pins that the
// unattributed-authors line sits behind the cheap login short-circuit
// (statusUnattributedOutcome): a logged-out repo prints nothing about it and
// — because the short-circuit runs before `git shortlog` or the cache file —
// returns promptly rather than hanging on any of detection's steps.
func TestRunStatus_NoUnattributedAuthorsLineWhenLoggedOut(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	// Isolate from any config dir another test in this process may have
	// touched, so this is unambiguously the logged-out path.
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

	done := make(chan string, 1)
	go func() {
		var stdout bytes.Buffer
		if err := runStatus(context.Background(), &stdout, false, false); err != nil {
			done <- "error: " + err.Error()
			return
		}
		done <- stdout.String()
	}()

	select {
	case out := <-done:
		if strings.HasPrefix(out, "error: ") {
			t.Fatalf("runStatus() %s", out)
		}
		if strings.Contains(out, "unattributed") {
			t.Errorf("expected no unattributed-authors line when logged out, got: %s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runStatus() did not return within 5s — detection likely ran shortlog/network on the logged-out path")
	}
}
