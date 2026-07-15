package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/spf13/cobra"
)

// TestResolveStatusTarget_PrefersActiveContext pins the multi-core fix: status
// targets the active context's CoreURL + its session token, recording a real
// context and reading it back.
func TestResolveStatusTarget_PrefersActiveContext(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	exp := time.Now().Add(time.Hour).Unix()
	if _, err := auth.RecordLoginContext(makeContextJWT(t, fmt.Sprintf(`{"iss":"`+testCoreURL+`","handle":"alice","exp":%d}`, exp)), "", true); err != nil {
		t.Fatalf("record context: %v", err)
	}

	got, err := resolveStatusTarget(t.Context(), auth.Contexts, auth.RefreshedLoginToken)
	if err != nil {
		t.Fatalf("resolveStatusTarget: %v", err)
	}
	if got.coreURL != testCoreURL {
		t.Errorf("coreURL = %q, want the active context's CoreURL", got.coreURL)
	}
	if got.token == "" {
		t.Error("token = empty, want the active context's session token")
	}
	if got.activeContext == "" {
		t.Error("activeContext = empty, want the active context name")
	}
}

// TestResolveStatusTarget_PrefersRefreshedToken pins the fix: status uses the
// refreshed login JWT for the active context, so an expired-but-refreshable
// session reports "logged in" rather than the false "re-login" the raw read
// produced. The resolver returns a token distinct from what's stored; we assert
// status carries the refreshed one.
func TestResolveStatusTarget_PrefersRefreshedToken(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	// Stored token is expired; a raw read would 401 at /me → "re-login".
	expired := time.Now().Add(-time.Hour).Unix()
	if _, err := auth.RecordLoginContext(makeContextJWT(t, fmt.Sprintf(`{"iss":"`+testCoreURL+`","handle":"alice","exp":%d}`, expired)), "entr_refresh", true); err != nil {
		t.Fatalf("record context: %v", err)
	}

	refreshed := func(_ context.Context, _ *contexts.Context) (string, error) { return "refreshed-jwt", nil }
	got, err := resolveStatusTarget(t.Context(), auth.Contexts, refreshed)
	if err != nil {
		t.Fatalf("resolveStatusTarget: %v", err)
	}
	if got.token != "refreshed-jwt" {
		t.Errorf("token = %q, want the refreshed token (not the stale stored one)", got.token)
	}
	if got.coreURL != testCoreURL {
		t.Errorf("coreURL = %q, want the active context's CoreURL", got.coreURL)
	}
}

// TestResolveStatusTarget_FallsBackToStoredWhenRefreshFails pins the safety net:
// when refresh fails (revoked family, network, opaque token) status drops to the
// stored token and lets the /me probe arbitrate — rather than losing the active
// context.
func TestResolveStatusTarget_FallsBackToStoredWhenRefreshFails(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	exp := time.Now().Add(time.Hour).Unix()
	stored := makeContextJWT(t, fmt.Sprintf(`{"iss":"`+testCoreURL+`","handle":"alice","exp":%d}`, exp))
	if _, err := auth.RecordLoginContext(stored, "", true); err != nil {
		t.Fatalf("record context: %v", err)
	}

	failRefresh := func(_ context.Context, _ *contexts.Context) (string, error) {
		return "", auth.ErrNotLoggedIn
	}
	got, err := resolveStatusTarget(t.Context(), auth.Contexts, failRefresh)
	if err != nil {
		t.Fatalf("resolveStatusTarget: %v", err)
	}
	if got.token != stored {
		t.Errorf("token = %q, want the stored token as fallback", got.token)
	}
	if got.coreURL != testCoreURL || got.activeContext == "" {
		t.Errorf("want the active context preserved on fallback, got coreURL=%q activeContext=%q", got.coreURL, got.activeContext)
	}
}

// A genuine contexts.json read/parse error is surfaced by resolveStatusTarget,
// symmetric with the control-plane commands. (A missing file reads as "no
// contexts" and is not an error.)
func TestResolveStatusTarget_CorruptContextsErrors(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	if err := os.WriteFile(filepath.Join(cfgDir, "contexts.json"), []byte("{ not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt contexts.json: %v", err)
	}
	if _, err := resolveStatusTarget(t.Context(), auth.Contexts, auth.RefreshedLoginToken); err == nil {
		t.Fatal("want an error when contexts.json is corrupt, got nil")
	}
}

// With no contexts at all, the target is zero-valued: status renders the
// informational "Not logged in." (exit 0) and logout no-ops — never a probe
// against any default host.
func TestResolveStatusTarget_NoContextsIsZeroTarget(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	got, err := resolveStatusTarget(t.Context(), auth.Contexts, auth.RefreshedLoginToken)
	if err != nil {
		t.Fatalf("resolveStatusTarget: %v", err)
	}
	if got.coreURL != "" || got.token != "" || got.activeContext != "" {
		t.Fatalf("want zero target with no contexts, got %+v", got)
	}
}

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
	for _, hdr := range []string{"CONTEXT", "HANDLE", "LOGIN SERVER"} {
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

func TestCompleteContextNames(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	exp := time.Now().Add(time.Hour).Unix()

	// Two contexts; the second one recorded with activate=true is current.
	if _, err := auth.RecordLoginContext(makeContextJWT(t, fmt.Sprintf(`{"iss":"https://core-a.example.com","handle":"alice","exp":%d}`, exp)), "", false); err != nil {
		t.Fatalf("record core-a: %v", err)
	}
	currentName, err := auth.RecordLoginContext(makeContextJWT(t, fmt.Sprintf(`{"iss":"https://core-b.example.com","handle":"bob","exp":%d}`, exp)), "", true)
	if err != nil {
		t.Fatalf("record core-b: %v", err)
	}

	got, directive := completeContextNames(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("directive = %v, want NoFileComp", directive)
	}
	if len(got) != 2 {
		t.Fatalf("completions = %v, want 2 entries", got)
	}

	// Each entry is "name\tdescription" carrying handle and core URL; the
	// active context is annotated "(active)" and no other entry is.
	var activeCount int
	for _, entry := range got {
		name, desc, found := strings.Cut(entry, "\t")
		if !found {
			t.Fatalf("entry %q missing tab-separated description", entry)
		}
		if name == currentName {
			if !strings.Contains(desc, "(active)") {
				t.Fatalf("active entry %q missing (active) marker", entry)
			}
			if !strings.Contains(desc, "bob") || !strings.Contains(desc, "core-b.example.com") {
				t.Fatalf("active entry %q missing handle/core URL", entry)
			}
			activeCount++
		} else if strings.Contains(desc, "(active)") {
			t.Fatalf("non-active entry %q wrongly marked (active)", entry)
		}
	}
	if activeCount != 1 {
		t.Fatalf("want exactly one (active) entry, got %d", activeCount)
	}

	// Past the single positional: nothing to complete.
	got, directive = completeContextNames(nil, []string{"already"}, "")
	if got != nil || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("with an arg present, want (nil, NoFileComp), got (%v, %v)", got, directive)
	}
}

func TestCompleteContextNames_NoContexts(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	got, directive := completeContextNames(nil, nil, "")
	if len(got) != 0 || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("no contexts: want (empty, NoFileComp), got (%v, %v)", got, directive)
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
