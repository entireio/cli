package cli

import (
	"bytes"
	"testing"
)

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
