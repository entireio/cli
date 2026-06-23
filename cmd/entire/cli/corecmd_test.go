package cli

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/entireio/cli/internal/coreapi"
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

func TestRenderCoreError_ScheduledMaintenance(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   error
	}{
		{"bare 503", &coreapi.ErrorModelStatusCode{StatusCode: http.StatusServiceUnavailable}},
		{"503 with problem-detail body", &coreapi.ErrorModelStatusCode{
			StatusCode: http.StatusServiceUnavailable,
			Response:   coreapi.ErrorModel{Detail: coreapi.OptString{Set: true, Value: "db unreachable"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := renderCoreError(tc.in)
			if got == nil || got.Error() != maintenanceMessage {
				t.Errorf("renderCoreError(%s) = %v, want %q", tc.name, got, maintenanceMessage)
			}
		})
	}
}

func TestRenderCoreError_PassThrough(t *testing.T) {
	t.Parallel()
	if got := renderCoreError(nil); got != nil {
		t.Errorf("renderCoreError(nil) = %v, want nil", got)
	}
	transport := errors.New("dial tcp: connection refused")
	if got := renderCoreError(transport); !errors.Is(got, transport) {
		t.Errorf("renderCoreError(transport) = %v, want passthrough", got)
	}
}
