package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestConfirmControlPlaneDeletion covers the non-TTY decision paths of the
// destructive-delete gate. The interactive form path needs a real terminal and
// is left to manual/e2e coverage.
func TestConfirmControlPlaneDeletion(t *testing.T) {
	t.Parallel()

	// --force proceeds without prompting (no TTY needed).
	var buf bytes.Buffer
	proceed, err := confirmControlPlaneDeletion(t.Context(), &buf, "org acme (01J)", true, false)
	if err != nil || !proceed {
		t.Fatalf("force: got (proceed=%v, err=%v), want (true, nil)", proceed, err)
	}

	// Non-interactive without --force must refuse, not delete unprompted.
	buf.Reset()
	proceed, err = confirmControlPlaneDeletion(t.Context(), &buf, "org acme (01J)", false, false)
	if err == nil {
		t.Fatalf("non-interactive without --force: expected error, got nil (proceed=%v)", proceed)
	}
	if proceed {
		t.Fatal("non-interactive without --force: must not proceed")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error should mention --force, got: %v", err)
	}
	if !strings.Contains(err.Error(), "org acme") {
		t.Fatalf("error should name the target, got: %v", err)
	}

	// An already-cancelled context is a clean cancel: no prompt, no error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	buf.Reset()
	proceed, err = confirmControlPlaneDeletion(ctx, &buf, "org acme (01J)", false, true)
	if err != nil || proceed {
		t.Fatalf("cancelled ctx: got (proceed=%v, err=%v), want (false, nil)", proceed, err)
	}
}

// TestFetchAllPages walks a multi-page source, stops on the empty cursor,
// and errors rather than looping when the server fails to advance.
func TestFetchAllPages(t *testing.T) {
	t.Parallel()

	t.Run("concatenates pages until empty cursor", func(t *testing.T) {
		t.Parallel()
		// Three pages keyed by the cursor the previous page returned: "" -> a,
		// "c1" -> b, "c2" -> c (last, empty next).
		pages := map[string]struct {
			items []string
			next  string
		}{
			"":   {items: []string{"a", "b"}, next: "c1"},
			"c1": {items: []string{"c", "d"}, next: "c2"},
			"c2": {items: []string{"e"}, next: ""},
		}
		var calls int
		got, err := fetchAllPages(context.Background(), func(_ context.Context, cursor string) ([]string, string, error) {
			calls++
			p := pages[cursor]
			return p.items, p.next, nil
		})
		if err != nil {
			t.Fatalf("fetchAllPages: %v", err)
		}
		if want := []string{"a", "b", "c", "d", "e"}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("items = %v, want %v", got, want)
		}
		if calls != 3 {
			t.Errorf("fetch calls = %d, want 3", calls)
		}
	})

	t.Run("single page", func(t *testing.T) {
		t.Parallel()
		got, err := fetchAllPages(context.Background(), func(_ context.Context, _ string) ([]string, string, error) {
			return []string{"only"}, "", nil
		})
		if err != nil || fmt.Sprint(got) != fmt.Sprint([]string{"only"}) {
			t.Fatalf("got (%v, %v), want ([only], nil)", got, err)
		}
	})

	t.Run("propagates fetch error", func(t *testing.T) {
		t.Parallel()
		sentinel := errors.New("boom")
		if _, err := fetchAllPages(context.Background(), func(_ context.Context, _ string) ([]string, string, error) {
			return nil, "", sentinel
		}); !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want %v", err, sentinel)
		}
	})

	t.Run("errors when cursor does not advance", func(t *testing.T) {
		t.Parallel()
		_, err := fetchAllPages(context.Background(), func(_ context.Context, _ string) ([]string, string, error) {
			return []string{"x"}, "stuck", nil
		})
		if err == nil {
			t.Fatal("expected error on non-advancing cursor, got nil")
		}
	})
}

// printTable/printFields render plain (no color/escape) when the writer
// isn't a TTY — which a bytes.Buffer never is — so these assert the plain
// layout directly.

func TestPrintTable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	items := []string{"alpha", "b"}
	err := printTable(&buf, []string{"NAME", "KIND"}, items, func(s string) []string {
		return []string{s, "repo"}
	})
	if err != nil {
		t.Fatalf("printTable: %v", err)
	}
	want := "NAME   KIND\n" +
		"alpha  repo\n" +
		"b      repo\n"
	if got := buf.String(); got != want {
		t.Errorf("printTable output:\n%q\nwant:\n%q", got, want)
	}
}

func TestPrintFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := printFields(&buf, []string{"ID", "NAME"}, []string{"01J", "widgets"}); err != nil {
		t.Fatalf("printFields: %v", err)
	}
	want := "ID    01J\n" +
		"NAME  widgets\n"
	if got := buf.String(); got != want {
		t.Errorf("printFields output:\n%q\nwant:\n%q", got, want)
	}
}
