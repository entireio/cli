package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
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

// TestFetchPagesBounded covers the budget branch fetchAllPages delegates to:
// the walk stops once the budget is reached (never splitting a page, so it can
// overshoot) and reports that entries remain; budget<=0 walks to the end.
func TestFetchPagesBounded(t *testing.T) {
	t.Parallel()

	// Three pages keyed by the cursor the previous page returned.
	pages := map[string][]string{"": {"a", "b"}, "c1": {"c", "d"}, "c2": {"e"}}
	nexts := map[string]string{"": "c1", "c1": "c2", "c2": ""}

	t.Run("stops at the budget and reports the partial walk", func(t *testing.T) {
		t.Parallel()
		got, partial, err := fetchPagesBounded(context.Background(), 3, func(_ context.Context, cursor string) ([]string, string, error) {
			return pages[cursor], nexts[cursor], nil
		})
		require.NoError(t, err)
		// Budget 3 is reached after the second page (4 items), which is not
		// split — so the result overshoots to 4 and the walk stops there.
		require.Equal(t, []string{"a", "b", "c", "d"}, got)
		require.True(t, partial, "a cursor still remained, so entries are unseen")
	})

	t.Run("a zero budget walks to the empty cursor", func(t *testing.T) {
		t.Parallel()
		got, partial, err := fetchPagesBounded(context.Background(), 0, func(_ context.Context, cursor string) ([]string, string, error) {
			return pages[cursor], nexts[cursor], nil
		})
		require.NoError(t, err)
		require.Equal(t, []string{"a", "b", "c", "d", "e"}, got)
		require.False(t, partial, "the chain ended, nothing left unseen")
	})
}

// newPageModeTestCmd wires the walk (--all/--limit) and single-page
// (--page-size/--page-token) flags the way the real list commands do, so the
// shared flag helpers can be unit-tested without a command surface.
func newPageModeTestCmd() *cobra.Command {
	var pageSize, limit int
	var pageToken string
	var all bool
	cmd := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
	cmd.Flags().IntVar(&limit, "limit", 0, "")
	cmd.Flags().BoolVar(&all, "all", false, "")
	pageModeFlags(cmd, &pageSize, &pageToken)
	return cmd
}

// TestValidatePageSize covers the local bound the list commands enforce in
// PreRunE: an unset flag passes, and an explicit value outside 1..max fails
// naming the flag (and the max), turning a would-be server 4xx into a
// flag-named error.
func TestValidatePageSize(t *testing.T) {
	t.Parallel()
	check := func(args ...string) error {
		cmd := newPageModeTestCmd()
		require.NoError(t, cmd.Flags().Parse(args))
		ps, err := cmd.Flags().GetInt("page-size")
		require.NoError(t, err)
		return validatePageSize(cmd, ps)
	}
	require.NoError(t, check(), "unset --page-size passes")
	require.NoError(t, check("--page-size", "1"))
	require.NoError(t, check("--page-size", "500"))

	err := check("--page-size", "0")
	require.Error(t, err)
	require.Contains(t, err.Error(), "--page-size")

	err = check("--page-size", "501")
	require.Error(t, err)
	require.Contains(t, err.Error(), "500")
}

// TestPageModeRequested pins that page mode is opted into by SETTING either
// page flag, not by its value: an explicitly empty --page-token (a resume
// loop's natural first call) still selects page mode, so the output shape does
// not flip to the walk's bare array on an empty cursor.
func TestPageModeRequested(t *testing.T) {
	t.Parallel()
	mode := func(args ...string) bool {
		cmd := newPageModeTestCmd()
		require.NoError(t, cmd.Flags().Parse(args))
		return pageModeRequested(cmd)
	}
	require.False(t, mode(), "no page flag → walk mode")
	require.True(t, mode("--page-size", "5"))
	require.True(t, mode("--page-token", "p2"))
	require.True(t, mode("--page-token", ""), "an explicit empty cursor still selects page mode")
}

// TestStyleTableWith covers the pre-styling that keeps a paged list command's
// table colored: the render inside flushThroughPager targets a buffer that
// never looks like a TTY, so color is decided against the real writer up front
// and applied here. The enabled path must color the header row and route each
// data cell through its column style (first column primary, rest secondary);
// the disabled path must be an exact identity so pipes, tests, and NO_COLOR
// see bare text byte for byte.
func TestStyleTableWith(t *testing.T) {
	t.Parallel()

	headers := []string{"ID", "NAME"}
	row := func(r []string) []string { return r }
	item := []string{"a", "b"}

	t.Run("enabled path colors headers and routes cells by column", func(t *testing.T) {
		t.Parallel()
		st := tableStyles{
			enabled: true,
			header:  lipgloss.NewStyle().Bold(true),
			primary: lipgloss.NewStyle().Underline(true),
			cell:    lipgloss.NewStyle().Faint(true),
		}
		gotHeaders, gotRow := styleTableWith(st, headers, row)
		require.Equal(t, []string{st.header.Render("ID"), st.header.Render("NAME")}, gotHeaders)
		// Column 0 is the primary identifier, the rest secondary — the same
		// split printTable applies when it colors a direct render.
		require.Equal(t, []string{st.primary.Render("a"), st.cell.Render("b")}, gotRow(item))
	})

	t.Run("disabled path is an exact identity", func(t *testing.T) {
		t.Parallel()
		gotHeaders, gotRow := styleTableWith(tableStyles{}, headers, row)
		require.Equal(t, headers, gotHeaders)
		require.Equal(t, item, gotRow(item))
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
