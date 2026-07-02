package debuglog

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintfRespectsEnvVar(t *testing.T) {
	var buf bytes.Buffer
	prev := SetOutput(&buf)
	t.Cleanup(func() { SetOutput(prev) })

	t.Setenv(envVar, "")
	Printf("hidden %s", "msg")
	if buf.Len() != 0 {
		t.Fatalf("expected no output when %s unset, got %q", envVar, buf.String())
	}

	t.Setenv(envVar, "1")
	Printf("visible %s", "msg")
	if !strings.Contains(buf.String(), "visible msg") {
		t.Fatalf("expected message in output, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), prefix) {
		t.Fatalf("expected prefix %q in output, got %q", prefix, buf.String())
	}
}

func TestEnabledTracksEnv(t *testing.T) {
	t.Setenv(envVar, "")
	if Enabled() {
		t.Fatalf("Enabled() = true with %s unset", envVar)
	}
	t.Setenv(envVar, "true")
	if !Enabled() {
		t.Fatalf("Enabled() = false with %s set", envVar)
	}
}
