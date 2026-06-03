package cli

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// makeContextJWT builds a JWT-shaped token (non-"none" alg) carrying the
// given claims, which is all RecordLoginContext needs.
func makeContextJWT(t *testing.T, payloadJSON string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	return header + "." + enc.EncodeToString([]byte(payloadJSON)) + "." + enc.EncodeToString([]byte("sig"))
}

func TestRunAuthContexts(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	var empty bytes.Buffer
	if err := runAuthContexts(&empty); err != nil {
		t.Fatalf("runAuthContexts (empty): %v", err)
	}
	if !strings.Contains(empty.String(), "No login contexts") {
		t.Fatalf("empty listing = %q, want a 'No login contexts' hint", empty.String())
	}

	exp := time.Now().Add(time.Hour).Unix()
	token := makeContextJWT(t, fmt.Sprintf(`{"iss":"https://core.example.com","handle":"alice","exp":%d}`, exp))
	if _, err := auth.RecordLoginContext(token, "", true); err != nil {
		t.Fatalf("RecordLoginContext: %v", err)
	}

	var out bytes.Buffer
	if err := runAuthContexts(&out); err != nil {
		t.Fatalf("runAuthContexts: %v", err)
	}
	got := out.String()
	for _, hdr := range []string{"CONTEXT", "HANDLE", "CORE URL"} {
		if !strings.Contains(got, hdr) {
			t.Fatalf("listing = %q, want column header %q", got, hdr)
		}
	}
	if !strings.Contains(got, "*") {
		t.Fatalf("listing = %q, want an active-context marker", got)
	}
	if !strings.Contains(got, "core.example.com") {
		t.Fatalf("listing = %q, want context core.example.com", got)
	}
	if !strings.Contains(got, "alice") {
		t.Fatalf("listing = %q, want handle alice", got)
	}
}

func TestWarnIfCrossCoreContext(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	t.Setenv("ENTIRE_AUTH_BASE_URL", "https://auth.example.com")
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	exp := time.Now().Add(time.Hour).Unix()

	// Same core as the configured auth host: no warning.
	sameName, err := auth.RecordLoginContext(makeContextJWT(t, fmt.Sprintf(`{"iss":"https://auth.example.com","handle":"alice","exp":%d}`, exp)), "", true)
	if err != nil {
		t.Fatalf("record same-core: %v", err)
	}
	var same bytes.Buffer
	warnIfCrossCoreContext(&same, sameName)
	if same.Len() != 0 {
		t.Fatalf("same-core context should not warn, got: %q", same.String())
	}

	// Different core: warns that the control plane won't follow.
	otherName, err := auth.RecordLoginContext(makeContextJWT(t, fmt.Sprintf(`{"iss":"https://other.example.com","handle":"alice","exp":%d}`, exp)), "", true)
	if err != nil {
		t.Fatalf("record cross-core: %v", err)
	}
	var diff bytes.Buffer
	warnIfCrossCoreContext(&diff, otherName)
	if !strings.Contains(diff.String(), "other.example.com") || !strings.Contains(diff.String(), "control-plane") {
		t.Fatalf("cross-core context should warn, got: %q", diff.String())
	}
}

func TestPromoteNextLogin(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	// No contexts: silent.
	var empty bytes.Buffer
	promoteNextLogin(&empty, &empty)
	if empty.Len() != 0 {
		t.Fatalf("no contexts should be silent, got %q", empty.String())
	}

	exp := time.Now().Add(time.Hour).Unix()
	if _, err := auth.RecordLoginContext(makeContextJWT(t, fmt.Sprintf(`{"iss":"https://a.example.com","handle":"alice","exp":%d}`, exp)), "", true); err != nil {
		t.Fatalf("record a: %v", err)
	}
	if _, err := auth.RecordLoginContext(makeContextJWT(t, fmt.Sprintf(`{"iss":"https://b.example.com","handle":"bob","exp":%d}`, exp)), "", true); err != nil {
		t.Fatalf("record b: %v", err)
	}

	// A current context is set: promotion is a no-op (nothing to promote into).
	var noop bytes.Buffer
	promoteNextLogin(&noop, &noop)
	if noop.Len() != 0 {
		t.Fatalf("with a current context set, promote should be silent, got %q", noop.String())
	}

	// Clear the active context (as logout does): the remaining login is promoted.
	if err := auth.RemoveCurrentContext(); err != nil {
		t.Fatalf("remove current: %v", err)
	}
	var buf bytes.Buffer
	promoteNextLogin(&buf, &buf)
	if !strings.Contains(buf.String(), "Now using") {
		t.Fatalf("expected promotion message, got %q", buf.String())
	}
	if _, current, err := auth.Contexts(); err != nil || current == "" {
		t.Fatalf("expected a context to be promoted to current (current=%q, err=%v)", current, err)
	}
}
