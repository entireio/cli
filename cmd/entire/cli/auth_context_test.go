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
	if !strings.Contains(got, activeContextMarker) {
		t.Fatalf("listing = %q, want the active context flagged %q", got, activeContextMarker)
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

// setupContextsForUse records one login context per host into an isolated config
// dir and returns their names in on-disk order. Only the first stays active, so
// a test can tell the picker's default from whatever was recorded last.
func setupContextsForUse(t *testing.T, hosts ...string) []string {
	t.Helper()
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	exp := time.Now().Add(time.Hour).Unix()
	names := make([]string, 0, len(hosts))
	for i, host := range hosts {
		claims := fmt.Sprintf(`{"iss":"https://%s","handle":"user-%d","exp":%d}`, host, i, exp)
		name, err := auth.RecordLoginContext(makeContextJWT(t, claims), "", i == 0)
		if err != nil {
			t.Fatalf("record %s: %v", host, err)
		}
		names = append(names, name)
	}
	return names
}

// TestSelectContextToUse_NoContexts pins that a bare `entire auth use` with
// nothing saved points at login instead of erroring or opening an empty picker.
func TestSelectContextToUse_NoContexts(t *testing.T) {
	setupContextsForUse(t)

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	got, err := selectContextToUse(cmd)
	if err != nil {
		t.Fatalf("selectContextToUse: %v", err)
	}
	if got != "" {
		t.Fatalf("selected = %q, want no selection", got)
	}
	if !strings.Contains(out.String(), "entire login") {
		t.Fatalf("output = %q, want a hint pointing at 'entire login'", out.String())
	}
}

// TestSelectContextToUse_SingleContextNeedsNoPicker pins that one saved login
// resolves directly — a one-row picker asks a question with one answer, and this
// is the branch that keeps a bare `auth use` working with no terminal.
func TestSelectContextToUse_SingleContextNeedsNoPicker(t *testing.T) {
	names := setupContextsForUse(t, "core-a.example.com")

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	got, err := selectContextToUse(cmd)
	if err != nil {
		t.Fatalf("selectContextToUse: %v", err)
	}
	if got != names[0] {
		t.Fatalf("selected = %q, want the only saved context %q", got, names[0])
	}
}

// TestSelectContextToUse_NoTerminalNamesThePositional pins the agent-safe
// fallback: with several logins and no terminal the picker can't render, so the
// error names every candidate and the non-interactive way to choose one rather
// than hanging or picking for the user.
func TestSelectContextToUse_NoTerminalNamesThePositional(t *testing.T) {
	// interactive.CanPromptInteractively() is false under `go test`, so this
	// exercises the no-terminal branch without faking a TTY.
	names := setupContextsForUse(t, "core-a.example.com", "core-b.example.com")

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	got, err := selectContextToUse(cmd)
	if err == nil {
		t.Fatalf("selected %q, want an error when the picker can't render", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "entire auth use") {
		t.Fatalf("error = %q, want it to name `entire auth use`", msg)
	}
	for _, name := range names {
		if !strings.Contains(msg, name) {
			t.Fatalf("error = %q, want it to list candidate %q", msg, name)
		}
	}
}

// TestContextPickerTable pins the picker rows: one per context, valued by the
// name SetCurrentContext takes, carrying the handle and login server that tell
// similarly-named logins apart, and exactly one marked active.
func TestContextPickerTable(t *testing.T) {
	t.Parallel()

	all := []*contexts.Context{
		{Name: "prod", Handle: "alice", CoreURL: "https://core-a.example.com"},
		{Name: "staging", Handle: "bob", CoreURL: "https://core-b.example.com"},
		{Name: "bare"},
	}

	header, options := contextPickerTable(all, "staging")
	if len(options) != len(all) {
		t.Fatalf("options = %d, want one per context (%d)", len(options), len(all))
	}

	for _, col := range []string{"CONTEXT", "HANDLE", "LOGIN SERVER"} {
		if !strings.Contains(header, col) {
			t.Errorf("header = %q, want column %q (the same ones `auth contexts` prints)", header, col)
		}
	}

	var activeCount int
	for i, opt := range options {
		if opt.Value != all[i].Name {
			t.Errorf("option %d value = %q, want the context name %q", i, opt.Value, all[i].Name)
		}
		if !strings.Contains(opt.Key, all[i].Name) {
			t.Errorf("option %d label = %q, want it to carry the name %q", i, opt.Key, all[i].Name)
		}
		if strings.Contains(opt.Key, "(active)") {
			activeCount++
			if opt.Value != "staging" {
				t.Errorf("option %q is marked active, want only staging", opt.Value)
			}
		}
	}
	if activeCount != 1 {
		t.Fatalf("want exactly one (active) row, got %d", activeCount)
	}

	// Missing handle / login server become the listing's placeholder dash, so
	// the row keeps its columns instead of collapsing to a bare name.
	if got := strings.Fields(options[2].Key); len(got) != 3 || got[0] != "bare" || got[1] != placeholderDash || got[2] != placeholderDash {
		t.Errorf("row for a handle-less, URL-less context = %q, want name plus two %q placeholders", options[2].Key, placeholderDash)
	}
	if !strings.Contains(options[0].Key, "alice") || !strings.Contains(options[0].Key, "core-a.example.com") {
		t.Errorf("row = %q, want the handle and login server", options[0].Key)
	}
}

// TestContextPickerTable_ColumnsAlign pins the alignment that makes this a
// table rather than three fields with spaces between them: every column starts
// at the same offset in the header and in every row, and the header is
// indented past huh's cursor so it sits above the option text.
func TestContextPickerTable_ColumnsAlign(t *testing.T) {
	t.Parallel()

	all := []*contexts.Context{
		{Name: "p", Handle: "a-very-long-handle", CoreURL: "https://short"},
		{Name: "a-very-long-context-name", Handle: "b", CoreURL: "https://a-much-longer-login-server.example.com"},
		{Name: "mid", Handle: "carol", CoreURL: "https://core.example.com"},
	}

	header, options := contextPickerTable(all, "mid")

	if !strings.HasPrefix(header, selectOptionIndent+"CONTEXT") {
		t.Fatalf("header = %q, want it indented by %q so it aligns with the option text", header, selectOptionIndent)
	}

	// Where each column starts in the header, minus the cursor indent (huh
	// indents the rows itself). Anchored on the labels rather than on gaps,
	// because "LOGIN SERVER" contains a space of its own.
	bare := strings.TrimPrefix(header, selectOptionIndent)
	want := make([]int, 0, 3)
	for _, col := range []string{"CONTEXT", "HANDLE", "LOGIN SERVER"} {
		at := strings.Index(bare, col)
		if at < 0 {
			t.Fatalf("header %q is missing column %q", header, col)
		}
		want = append(want, at)
	}

	// Test data has no spaces inside a cell, so a row's columns are its
	// space-separated runs.
	for _, opt := range options {
		got := columnOffsets(opt.Key)
		if len(got) < 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("row %q starts columns at %v, want the header's %v", opt.Key, got, want)
		}
	}
}

// TestContextTableMatchesPicker pins what one builder buys: `entire auth
// contexts` and the `entire auth use` picker print the same table, down to the
// column widths and the trailing "(active)". A column added to one cannot go
// missing from the other, and the marker cannot drift back to two spellings.
// The picker's rows are indented by huh's cursor gutter; nothing else differs.
func TestContextTableMatchesPicker(t *testing.T) {
	t.Parallel()

	all := []*contexts.Context{
		{Name: "prod", Handle: "alice", CoreURL: "https://core-a.example.com"},
		{Name: "staging", Handle: "bob", CoreURL: "https://a-much-longer-login-server.example.com"},
		{Name: "bare"},
	}

	// A bytes.Buffer is not a terminal, so the listing renders unstyled — the
	// only difference the picker's rows would otherwise have.
	var listing bytes.Buffer
	renderContextsTable(&listing, all, "staging")
	want := strings.Split(strings.TrimRight(listing.String(), "\n"), "\n")

	header, options := contextPickerTable(all, "staging")
	got := []string{strings.TrimPrefix(header, selectOptionIndent)}
	for _, opt := range options {
		got = append(got, opt.Key)
	}

	if len(got) != len(want) {
		t.Fatalf("picker rendered %d lines, listing %d:\npicker:  %q\nlisting: %q", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\npicker  = %q\nlisting = %q", i, got[i], want[i])
		}
	}
}

// columnOffsets returns the rune index at which each space-separated run in a
// row begins, which is what has to match the header's column starts.
func columnOffsets(line string) []int {
	var offsets []int
	inGap := true
	for i, r := range []rune(line) {
		switch {
		case r == ' ':
			inGap = true
		case inGap:
			offsets = append(offsets, i)
			inGap = false
		}
	}
	return offsets
}

// TestAuthUseCmd_ArgsAndSwitch pins the command surface: a name still switches
// without asking, and the argument is now optional so a bare `entire auth use`
// reaches the picker instead of failing argument validation.
func TestAuthUseCmd_ArgsAndSwitch(t *testing.T) {
	names := setupContextsForUse(t, "core-a.example.com", "core-b.example.com")

	cmd := newAuthUseCmd()
	if err := cmd.Args(cmd, nil); err != nil {
		t.Fatalf("Args(no args) = %v, want the picker form to be accepted", err)
	}
	if err := cmd.Args(cmd, []string{"a", "b"}); err == nil {
		t.Error("Args(two args) = nil, want at most one context")
	}

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{names[1]})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("auth use %s: %v", names[1], err)
	}
	if !strings.Contains(out.String(), names[1]) {
		t.Fatalf("output = %q, want confirmation naming %q", out.String(), names[1])
	}
	_, current, err := auth.StoredContexts()
	if err != nil {
		t.Fatalf("StoredContexts: %v", err)
	}
	if current != names[1] {
		t.Fatalf("current context = %q, want %q", current, names[1])
	}
}
