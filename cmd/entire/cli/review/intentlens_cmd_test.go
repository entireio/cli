package review

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/review/intentlens"
)

func TestIntentLensAuditDemoCommand(t *testing.T) {
	t.Parallel()
	cmd := newIntentLensAuditCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--demo"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute audit demo: %v", err)
	}
	for _, want := range []string{intentlens.DemoNotice, "IntentLens Audit", "IMPLEMENTED", "INCOMPLETE", "UNCERTAIN"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("demo output missing %q", want)
		}
	}
}

func TestIntentLensAuditCommandRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	cmd := newIntentLensAuditCommand()
	var output bytes.Buffer
	cmd.SetIn(strings.NewReader("{\"summary\":"))
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--file", "-"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected malformed input error")
	}
	if !strings.Contains(output.String(), "Could not display audit result") {
		t.Fatalf("missing malformed-data state: %s", output.String())
	}
}
