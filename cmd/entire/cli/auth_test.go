package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
)

// --- status -----------------------------------------------------------------

const testCoreURL = "https://eu.auth.entire.io"

// okProfile is a profileFetcher returning a fully-populated profile, for the
// happy-path status tests.
func okProfile(context.Context, string, string) (*authProfile, error) {
	return &authProfile{
		Handle:         "alice",
		DisplayName:    "Alice Smith",
		Email:          "alice@example.com",
		Provider:       "github",
		ProviderUserID: "alice",
	}, nil
}

// unusedProfile is a profileFetcher that fails the test if called — for the
// not-logged-in path, where the empty-token check short-circuits before /me.
func unusedProfile(t *testing.T) profileFetcher {
	return func(context.Context, string, string) (*authProfile, error) {
		t.Helper()
		t.Fatal("/me should not be called when there is no token")
		return nil, errors.New("unreachable")
	}
}

// rejecting returns a profileFetcher that always fails with err.
func rejecting(err error) profileFetcher {
	return func(context.Context, string, string) (*authProfile, error) { return nil, err }
}

// noSessions is a authSessionLister returning an empty list (no table rendered).
func noSessions(context.Context, string, string) ([]api.AuthSession, error) { return nil, nil }

func TestRunAuthStatus_NotLoggedIn(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	target := statusTarget{coreURL: testCoreURL} // empty token
	if err := runAuthStatus(context.Background(), &out, unusedProfile(t), noSessions, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "Not logged in to "+testCoreURL) {
		t.Fatalf("output = %q, want 'Not logged in' message", out.String())
	}
}

func TestRunAuthStatus_LoggedIn(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "eu.auth.entire.io", totalContexts: 1}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, noSessions, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Logged in") {
		t.Fatalf("output = %q, want a logged-in verdict line", got)
	}
	// The handle is provider-qualified so it can be pasted straight into an
	// `entire grant` command.
	if !hasMetadataRow(got, "user", "github:alice") {
		t.Fatalf("output = %q, want a provider-qualified user row", got)
	}
	// Display name and email are deliberately not rendered — the handle is the
	// account's identity everywhere else in the CLI.
	for _, unwanted := range []string{"Alice Smith", "alice@example.com", "github/alice"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("output = %q, must not contain %q", got, unwanted)
		}
	}
	if !hasMetadataRow(got, "context", "eu.auth.entire.io") {
		t.Fatalf("output = %q, want the active-context row", got)
	}
	// noSessions returns an empty list, so no table is rendered.
	if strings.Contains(got, "Active Sessions") {
		t.Fatalf("output = %q, empty session list should render no table", got)
	}
}

// TestWriteProfileLines_Jurisdiction verifies the home jurisdiction slug is
// rendered (so `auth token --jurisdiction` is discoverable) and omitted when the
// server didn't populate it.
func TestWriteProfileLines_Jurisdiction(t *testing.T) {
	t.Parallel()

	sty := newStatusStyles(io.Discard)

	withJ := authProfileRows(sty, &authProfile{Handle: "alice", Provider: "github", Jurisdiction: "us"}, statusTarget{})
	if !hasRow(withJ, "jurisdiction", "us") {
		t.Fatalf("rows = %+v, want a jurisdiction row", withJ)
	}

	withoutJ := authProfileRows(sty, &authProfile{Handle: "alice", Provider: "github"}, statusTarget{})
	if hasLabel(withoutJ, "jurisdiction") {
		t.Fatalf("rows = %+v, want no jurisdiction row when the slug is empty", withoutJ)
	}
}

// decodeAuthStatusJSON runs status in --json mode and unmarshals the envelope,
// so assertions are made against fields rather than against formatted text.
func decodeAuthStatusJSON(t *testing.T, fetch profileFetcher, list authSessionLister, target statusTarget, opts authStatusOptions) authStatusJSON {
	t.Helper()
	opts.JSON = true
	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, fetch, list, target, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got authStatusJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	return got
}

func twoSessions(context.Context, string, string) ([]api.AuthSession, error) {
	lastUsed := "2026-05-01T00:00:00Z"
	return []api.AuthSession{
		{ID: "fam-1", Name: "other login", Scope: "cli", CreatedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2026-12-01T00:00:00Z", LastUsedAt: &lastUsed},
		{ID: "fam-2", Name: "this login", Scope: "cli", CreatedAt: "2026-02-01T00:00:00Z", ExpiresAt: "2026-12-15T00:00:00Z"},
	}, nil
}

// --json collapses the same way the text view does: a count always, the array
// only when --sessions was asked for. Timestamps stay RFC3339 — the relative
// form is a reading aid for humans, not something to parse.
func TestRunAuthStatusJSON_CountsByDefaultAndListsOnRequest(t *testing.T) {
	t.Parallel()

	token := makeTestJWT(t, `{"iss":"https://eu.auth.entire.io","fid":"fam-2"}`)
	target := statusTarget{coreURL: testCoreURL, token: token, activeContext: "eu.auth.entire.io", totalContexts: 1}

	brief := decodeAuthStatusJSON(t, okProfile, twoSessions, target, authStatusOptions{})
	if !brief.LoggedIn {
		t.Error("logged_in = false, want true")
	}
	if brief.User != "github:alice" {
		t.Errorf("user = %q, want %q", brief.User, "github:alice")
	}
	if brief.ActiveSessions == nil || *brief.ActiveSessions != 2 {
		t.Errorf("active_sessions = %v, want 2", brief.ActiveSessions)
	}
	if len(brief.Sessions) != 0 {
		t.Errorf("sessions = %+v, want the array omitted without --sessions", brief.Sessions)
	}
	if brief.CurrentSessionID != "fam-2" {
		t.Errorf("current_session_id = %q, want fam-2", brief.CurrentSessionID)
	}
	if brief.ExpiresAt != "2026-12-15T00:00:00Z" {
		t.Errorf("expires_at = %q, want the caller's own session expiry in RFC3339", brief.ExpiresAt)
	}

	full := decodeAuthStatusJSON(t, okProfile, twoSessions, target, authStatusOptions{Sessions: true})
	if len(full.Sessions) != 2 {
		t.Fatalf("sessions = %+v, want both rows", full.Sessions)
	}
	var current []authSessionJSON
	for _, sess := range full.Sessions {
		if sess.Current {
			current = append(current, sess)
		}
	}
	if len(current) != 1 || current[0].ID != "fam-2" {
		t.Fatalf("current rows = %+v, want exactly fam-2", current)
	}
	if current[0].Scope != "cli" {
		t.Errorf("scope = %q, want it surfaced", current[0].Scope)
	}
}

// A listing failure leaves active_sessions absent rather than reporting zero:
// "we could not ask" and "you have none" are different answers.
func TestRunAuthStatusJSON_ListingFailureIsNotZeroSessions(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "eu.auth.entire.io"}
	failing := func(context.Context, string, string) ([]api.AuthSession, error) {
		return nil, errors.New("sessions endpoint unreachable")
	}

	got := decodeAuthStatusJSON(t, okProfile, failing, target, authStatusOptions{})
	if !got.LoggedIn {
		t.Error("logged_in = false, want true — /me already validated the token")
	}
	if got.ActiveSessions != nil {
		t.Errorf("active_sessions = %v, want absent when the listing failed", *got.ActiveSessions)
	}
	if got.SessionsError == "" {
		t.Error("sessions_error = empty, want the listing failure named")
	}
	if got.ExpiresAt != "" {
		t.Errorf("expires_at = %q, want none without a listing to read it from", got.ExpiresAt)
	}
}

func TestRunAuthStatusJSON_NotLoggedIn(t *testing.T) {
	t.Parallel()

	got := decodeAuthStatusJSON(t, unusedProfile(t), noSessions, statusTarget{coreURL: testCoreURL}, authStatusOptions{})
	if got.LoggedIn {
		t.Error("logged_in = true, want false with no token")
	}
	if got.Server != "eu.auth.entire.io" {
		t.Errorf("server = %q, want the core we are not logged in to", got.Server)
	}
}

// An env bearer has no revocable session family, so the machine-readable answer
// must say so rather than leave a reader to infer it from missing fields.
func TestRunAuthStatusJSON_EnvTokenHasNoSessions(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", envToken: true}
	list := func(context.Context, string, string) ([]api.AuthSession, error) {
		t.Helper()
		t.Fatal("listSessions must not be called in ENTIRE_TOKEN mode")
		return nil, nil
	}

	got := decodeAuthStatusJSON(t, okProfile, list, target, authStatusOptions{})
	if !got.EnvToken {
		t.Error("env_token = false, want true")
	}
	if got.ActiveSessions != nil || got.CurrentSessionID != "" || got.ExpiresAt != "" {
		t.Errorf("got %+v, want no session fields for an env bearer", got)
	}
}

// The context row names the login server only when the context name does not
// already spell it. The name defaults to the host, so repeating it is noise —
// but a context the user named "work" would otherwise leave the server unnamed
// anywhere in the output.
func TestAuthContextRow_NamesTheServerOnlyWhenTheNameDoesNot(t *testing.T) {
	t.Parallel()

	sty := newStatusStyles(io.Discard)
	tests := []struct {
		name      string
		ctxName   string
		coreURL   string
		wantValue string
	}{
		{"name is the host", "eu.auth.entire.io", "https://eu.auth.entire.io", "eu.auth.entire.io"},
		{"name differs", "work", "https://eu.auth.entire.io", "work · eu.auth.entire.io"},
		{"case-insensitive match", "EU.AUTH.ENTIRE.IO", "https://eu.auth.entire.io", "EU.AUTH.ENTIRE.IO"},
		{"unparseable core URL still names something", "work", "::nonsense", "work · ::nonsense"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := authContextRow(sty, tt.ctxName, tt.coreURL)
			if got.Label != "context" {
				t.Errorf("label = %q, want %q", got.Label, "context")
			}
			if got.Value != tt.wantValue {
				t.Errorf("value = %q, want %q", got.Value, tt.wantValue)
			}
		})
	}
}

// hasMetadataRow reports whether out contains a metadataRows line for label
// with exactly value. Assertions match the label/value pair rather than literal
// spacing because metadataRows sizes its label column to the widest label
// present, so the gutter changes with which rows a given case renders.
func hasMetadataRow(out, label, value string) bool {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), label)
		if !ok || !strings.HasPrefix(rest, " ") {
			continue
		}
		if strings.TrimSpace(rest) == value {
			return true
		}
	}
	return false
}

// hasRow reports whether rows contains label with exactly value.
func hasRow(rows []explainRow, label, value string) bool {
	for _, r := range rows {
		if r.Label == label && r.Value == value {
			return true
		}
	}
	return false
}

// hasLabel reports whether rows contains label at all, whatever its value.
func hasLabel(rows []explainRow, label string) bool {
	for _, r := range rows {
		if r.Label == label {
			return true
		}
	}
	return false
}

// defaultFetchProfile must read the account's own home region from
// global.homeJurisdiction, not the top-level jurisdiction field, which is the
// serving node's. A geo-routed device login makes the two differ: an AU-homed
// account approving in EU is answered by EU, whose /me reports jurisdiction
// "eu", global.homeJurisdiction "au", and a regionalUnavailable block.
func TestDefaultFetchProfile_HomeJurisdictionFromGlobal(t *testing.T) {
	t.Parallel()

	// Trimmed from a real EU-served /me for an AU-homed account.
	body := `{
      "global": {
        "accountId":"01KYNMWZ7DDDM2H42SM8TBTQP9","handle":"toothbrush",
        "homeJurisdiction":"au","createdAt":"2026-07-29T00:37:55.565548Z",
        "handles":[{"provider":"github","handle":"toothbrush","providerUserId":"423357"}]
      },
      "auth":   {"provider":"github","providerUserId":"423357"},
      "regionalUnavailable": {
        "error":"foreign_jurisdiction","jurisdiction":"au",
        "homeCoreUrl":"https://au.console.entire.io/#/profile",
        "message":"Your profile is hosted in the au jurisdiction."
      },
      "mode":"regional","jurisdiction":"eu"
    }`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	p, err := defaultFetchProfile(context.Background(), srv.URL, "tok")
	if err != nil {
		t.Fatalf("defaultFetchProfile: %v", err)
	}
	if p.Jurisdiction != "au" {
		t.Errorf("Jurisdiction = %q, want au (global.homeJurisdiction, not the node's %q)", p.Jurisdiction, "eu")
	}
	if !p.ForeignRegion {
		t.Error("ForeignRegion = false, want true when /me returns regionalUnavailable")
	}
}

// The foreign-region note explains why the "Logged in to" host and the
// jurisdiction disagree, and why the display name and email are absent.
func TestRunAuthStatus_ForeignRegionNote(t *testing.T) {
	t.Parallel()

	foreign := func(context.Context, string, string) (*authProfile, error) {
		return &authProfile{Handle: "toothbrush", Provider: "github", Jurisdiction: "au", ForeignRegion: true}, nil
	}

	var out bytes.Buffer
	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "eu.auth.entire.io", totalContexts: 1}
	if err := runAuthStatus(context.Background(), &out, foreign, noSessions, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := out.String()
	if !hasMetadataRow(got, "jurisdiction", "au") {
		t.Fatalf("output = %q, want the account's home region", got)
	}
	if !strings.Contains(got, "outside your home region") {
		t.Fatalf("output = %q, want a note explaining the foreign region", got)
	}
	// The retired console deep link and the message that points at it must not
	// reach the user: a bare "/" redirects by role, never to a profile page.
	if strings.Contains(got, "console.entire.io") || strings.Contains(got, "#/profile") {
		t.Fatalf("output = %q, must not surface the dead console deep link", got)
	}
}

// A same-region login gets no note — the common case must stay quiet.
func TestRunAuthStatus_NoNoteForHomeRegion(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	target := statusTarget{coreURL: testCoreURL, token: "tok", totalContexts: 1}
	if err := runAuthStatus(context.Background(), &out, okProfile, noSessions, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out.String(), "outside your home region") {
		t.Fatalf("output = %q, want no foreign-region note", out.String())
	}
}

// With no home region from /me, the login token's home_jurisdiction claim
// stands in rather than the line disappearing.
func TestRunAuthStatus_JurisdictionFallsBackToTokenClaim(t *testing.T) {
	t.Parallel()

	noJuris := func(context.Context, string, string) (*authProfile, error) {
		return &authProfile{Handle: "alice", Provider: "github"}, nil
	}

	var out bytes.Buffer
	token := makeTestJWT(t, `{"iss":"https://eu.auth.entire.io","home_jurisdiction":"au"}`)
	target := statusTarget{coreURL: testCoreURL, token: token, totalContexts: 1}
	if err := runAuthStatus(context.Background(), &out, noJuris, noSessions, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasMetadataRow(out.String(), "jurisdiction", "au") {
		t.Fatalf("output = %q, want the jurisdiction from the token claim", out.String())
	}
}

// An opaque (non-JWT) token with no /me home region simply omits the line.
func TestRunAuthStatus_NoJurisdictionAnywhere(t *testing.T) {
	t.Parallel()

	noJuris := func(context.Context, string, string) (*authProfile, error) {
		return &authProfile{Handle: "alice", Provider: "github"}, nil
	}

	var out bytes.Buffer
	target := statusTarget{coreURL: testCoreURL, token: "tok", totalContexts: 1}
	if err := runAuthStatus(context.Background(), &out, noJuris, noSessions, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out.String(), "jurisdiction") {
		t.Fatalf("output = %q, want no jurisdiction row when nothing supplies it", out.String())
	}
}

// In ENTIRE_TOKEN mode there is no stored context, keychain slot, or revocable
// session: status names the env-token core and bearer source, and renders none
// of the context/keychain/session lines. listSessions must not be called — you
// can't manage an env-token session.
func TestRunAuthStatus_EnvTokenMode(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", envToken: true}
	listSessions := func(context.Context, string, string) ([]api.AuthSession, error) {
		t.Helper()
		t.Fatal("listSessions must not be called in ENTIRE_TOKEN mode")
		return nil, nil
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, listSessions, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Logged in") {
		t.Fatalf("output = %q, want a logged-in verdict line", got)
	}
	if !hasMetadataRow(got, "user", "github:alice") {
		t.Fatalf("output = %q, want the profile row", got)
	}
	// With no context to name the login server, env-token mode says it outright.
	if !hasMetadataRow(got, "server", "eu.auth.entire.io") {
		t.Fatalf("output = %q, want the login server named", got)
	}
	if !strings.Contains(got, auth.EnvTokenVar+" environment variable") {
		t.Fatalf("output = %q, want the ENTIRE_TOKEN bearer note", got)
	}
	// An env bearer has no session family, so the verdict line carries no expiry.
	if strings.Contains(got, "expires") {
		t.Fatalf("output = %q, must not claim an expiry for an env token", got)
	}
	for _, unwanted := range []string{"context ", "keychain", "Active Sessions", availableContextsRowLabel} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("output = %q, must not contain %q in ENTIRE_TOKEN mode", got, unwanted)
		}
	}
}

// resolveEnvTokenStatusTarget reads the core from the token's aud (the same
// origin coreapi.New dials) and uses the token verbatim as the bearer; a blank
// or aud-less token is a fail-closed error, never a fall-back to a context.
func TestResolveEnvTokenStatusTarget(t *testing.T) {
	t.Parallel()

	t.Run("valid token yields aud core + verbatim bearer", func(t *testing.T) {
		t.Parallel()
		tok := makeJWT(t, `{"alg":"HS256","typ":"JWT"}`, `{"aud":"`+testCoreURL+`"}`)
		got, err := resolveEnvTokenStatusTarget("  " + tok + "  ") // surrounding whitespace trimmed
		if err != nil {
			t.Fatalf("resolveEnvTokenStatusTarget: %v", err)
		}
		if got.coreURL != testCoreURL {
			t.Fatalf("coreURL = %q, want the token's aud %q", got.coreURL, testCoreURL)
		}
		if got.token != tok {
			t.Fatalf("token = %q, want the verbatim env token", got.token)
		}
		if !got.envToken {
			t.Fatal("envToken = false, want true")
		}
	})

	t.Run("blank is fail-closed", func(t *testing.T) {
		t.Parallel()
		if _, err := resolveEnvTokenStatusTarget("   "); err == nil {
			t.Fatal("want an error for a blank ENTIRE_TOKEN, got nil")
		}
	})

	t.Run("token without a URL aud is rejected", func(t *testing.T) {
		t.Parallel()
		tok := makeJWT(t, `{"alg":"HS256","typ":"JWT"}`, `{"sub":"ci-runner"}`)
		if _, err := resolveEnvTokenStatusTarget(tok); err == nil {
			t.Fatal("want an error when the token has no URL-shaped aud, got nil")
		}
	})
}

func TestRunAuthStatus_RendersSessionsTable(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "eu.auth.entire.io", totalContexts: 1}
	lastUsed := "2026-05-01T00:00:00Z"
	listSessions := func(_ context.Context, coreURL, token string) ([]api.AuthSession, error) {
		if coreURL != testCoreURL || token != "tok" {
			t.Errorf("listSessions called with (%q, %q), want the active core+token", coreURL, token)
		}
		return []api.AuthSession{
			{ID: "fam-1", Name: "OIDC login", CreatedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2026-12-01T00:00:00Z", LastUsedAt: &lastUsed},
			{ID: "fam-2", Name: "OIDC login", CreatedAt: "2026-02-01T00:00:00Z", ExpiresAt: "2026-12-15T00:00:00Z"},
		}, nil
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, listSessions, target, authStatusOptions{Sessions: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Active Sessions") {
		t.Fatalf("output = %q, want the active-sessions section", got)
	}
	if !strings.Contains(got, "2 sessions") {
		t.Fatalf("output = %q, want the section footer count", got)
	}
	for _, want := range []string{"NAME", "CREATED", "LAST USED", "EXPIRES", formatAuthTimestamp("2026-01-01T00:00:00Z"), lastUsedNever} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want table to contain %q", got, want)
		}
	}
	if !strings.Contains(got, "entire logout --everywhere") {
		t.Fatalf("output = %q, want logout hint tying the table to logout", got)
	}
	// "tok" is not a JWT, so no session can be identified as the caller's.
	if strings.Contains(got, currentSessionMarker) {
		t.Fatalf("output = %q, must not mark a session the caller was not matched to", got)
	}
}

// The default view answers "am I logged in, as who, for how long" without the
// table — the count row stands in for it. This is the regression the redesign
// exists to prevent: a heavy-login account used to get a screenful of rows.
func TestRunAuthStatus_DefaultViewCountsSessionsInsteadOfListingThem(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "eu.auth.entire.io", totalContexts: 1}
	listSessions := func(context.Context, string, string) ([]api.AuthSession, error) {
		return []api.AuthSession{
			{ID: "fam-1", Name: "OIDC login", CreatedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2026-12-01T00:00:00Z"},
			{ID: "fam-2", Name: "OIDC login", CreatedAt: "2026-02-01T00:00:00Z", ExpiresAt: "2026-12-15T00:00:00Z"},
		}, nil
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, listSessions, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	if !hasMetadataRow(got, "active sessions", "2 · run 'entire auth status --sessions' to list them") {
		t.Fatalf("output = %q, want a sessions count row", got)
	}
	if !strings.Contains(got, "entire auth status --sessions") {
		t.Fatalf("output = %q, want the count row to say how to see the list", got)
	}
	for _, unwanted := range []string{"NAME", "LAST USED", "Active Sessions"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("output = %q, default view must not render the table (%q)", got, unwanted)
		}
	}
}

// The caller's own session is identified by the login token's fid claim — a
// session IS a refresh-token family — and both the verdict line's expiry and
// the table marker hang off that match.
func TestRunAuthStatus_MarksTheCallersOwnSession(t *testing.T) {
	t.Parallel()

	token := makeTestJWT(t, `{"iss":"https://eu.auth.entire.io","fid":"fam-2"}`)
	target := statusTarget{coreURL: testCoreURL, token: token, activeContext: "eu.auth.entire.io", totalContexts: 1}
	listSessions := func(context.Context, string, string) ([]api.AuthSession, error) {
		return []api.AuthSession{
			{ID: "fam-1", Name: "other login", CreatedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2026-12-01T00:00:00Z"},
			{ID: "fam-2", Name: "this login", CreatedAt: "2026-02-01T00:00:00Z", ExpiresAt: "2026-12-15T00:00:00Z"},
		}, nil
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, listSessions, target, authStatusOptions{Sessions: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	if n := strings.Count(got, currentSessionMarker); n != 1 {
		t.Fatalf("output = %q, want exactly one %s marker, got %d", got, currentSessionMarker, n)
	}
	// The marker must sit on fam-2's row, not merely somewhere in the table.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, currentSessionMarker) && !strings.Contains(line, "this login") {
			t.Fatalf("marker landed on the wrong row: %q", line)
		}
	}
	if !strings.Contains(got, "expires "+formatAuthTimestamp("2026-12-15T00:00:00Z")) {
		t.Fatalf("output = %q, want the verdict line to carry the caller's own expiry", got)
	}
}

// A fid naming no listed session degrades to silence rather than guessing: the
// actions reachable from here end a session, and ending someone else's is worse
// than saying nothing.
func TestRunAuthStatus_UnmatchedSessionClaimsNothing(t *testing.T) {
	t.Parallel()

	token := makeTestJWT(t, `{"iss":"https://eu.auth.entire.io","fid":"fam-gone"}`)
	target := statusTarget{coreURL: testCoreURL, token: token, activeContext: "eu.auth.entire.io", totalContexts: 1}
	listSessions := func(context.Context, string, string) ([]api.AuthSession, error) {
		return []api.AuthSession{
			{ID: "fam-1", Name: "other login", CreatedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2026-12-01T00:00:00Z"},
		}, nil
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, listSessions, target, authStatusOptions{Sessions: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	if strings.Contains(got, currentSessionMarker) {
		t.Fatalf("output = %q, must not mark any row when the claim matches none", got)
	}
	if strings.Contains(got, "expires") {
		t.Fatalf("output = %q, must not show an expiry borrowed from another session", got)
	}
}

func TestRunAuthStatus_SessionListFailureIsSoftNote(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "eu.auth.entire.io", totalContexts: 1}
	listSessions := func(context.Context, string, string) ([]api.AuthSession, error) {
		return nil, errors.New("sessions endpoint unreachable")
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, listSessions, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err) // liveness already passed via /me
	}
	got := out.String()
	if !strings.Contains(got, "Logged in") {
		t.Fatalf("output = %q, want still-logged-in", got)
	}
	if !strings.Contains(got, "(unavailable: sessions endpoint unreachable)") {
		t.Fatalf("output = %q, want the listing failure reported in the sessions row", got)
	}
	// Without a listing there is no session to read an expiry from.
	if strings.Contains(got, "expires") {
		t.Fatalf("output = %q, must not claim an expiry it could not read", got)
	}
}

// TestRunAuthStatus_QueriesActiveContextCore pins the multi-core fix: /me is
// called against the active context's core with that context's token, not a
// static AuthBaseURL.
func TestRunAuthStatus_QueriesActiveContextCore(t *testing.T) {
	t.Parallel()

	var gotCoreURL, gotToken string
	fetch := func(_ context.Context, coreURL, token string) (*authProfile, error) {
		gotCoreURL, gotToken = coreURL, token
		return &authProfile{Handle: "alice"}, nil
	}
	target := statusTarget{coreURL: testCoreURL, token: "eu-session-tok", activeContext: "eu.auth.entire.io", totalContexts: 1}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, fetch, noSessions, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotCoreURL != testCoreURL {
		t.Errorf("fetchProfile coreURL = %q, want %q", gotCoreURL, testCoreURL)
	}
	if gotToken != "eu-session-tok" {
		t.Errorf("fetchProfile token = %q, want the active context's token", gotToken)
	}
}

func TestRunAuthStatus_MultipleContextsHint(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "a", totalContexts: 3}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, noSessions, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasMetadataRow(out.String(), availableContextsRowLabel, "3 · run 'entire auth contexts' to list them") {
		t.Fatalf("output = %q, want a saved-contexts row with the listing hint", out.String())
	}
}

// A count of one is dropped on both rows: the sole context and the sole session
// are the ones already described above, so the row costs a line and carries no
// information.
func TestRunAuthStatus_CountRowsAreDroppedAtOne(t *testing.T) {
	t.Parallel()

	oneSession := func(context.Context, string, string) ([]api.AuthSession, error) {
		return []api.AuthSession{{ID: "fam-1", Name: "this login", CreatedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2026-12-01T00:00:00Z"}}, nil
	}
	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "a", totalContexts: 1}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, oneSession, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	for _, unwanted := range []string{availableContextsRowLabel, activeSessionsRowLabel} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("output = %q, want no %q row at a count of one", got, unwanted)
		}
	}
	// A single session makes --everywhere meaningless, and "end all 1" reads badly.
	if strings.Contains(got, "--everywhere") {
		t.Fatalf("output = %q, want no --everywhere hint with a single session", got)
	}
	if !strings.Contains(got, "Run 'entire logout' to end this session.") {
		t.Fatalf("output = %q, want the plain logout hint", got)
	}
}

// Zero sessions still reports: logged in with none is a contradiction worth
// seeing, unlike the redundant count of one.
func TestRunAuthStatus_ZeroSessionsStillReports(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "a", totalContexts: 1}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, noSessions, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasMetadataRow(out.String(), activeSessionsRowLabel, "0") {
		t.Fatalf("output = %q, want a zero-session row", out.String())
	}
}

func TestRunAuthStatus_InvalidTokenShapes(t *testing.T) {
	t.Parallel()

	cases := map[string]error{
		// 401 from GET /me as a typed core error.
		"typed 401": &coreapi.ErrorModelStatusCode{StatusCode: http.StatusUnauthorized},
		// 401 whose body isn't JSON: ogen fails to decode and the status is
		// only in the message string. This is the shape `auth status` hit in
		// the wild against a cross-core mismatch.
		"non-JSON 401": errors.New("decode response: default (code 401): unexpected Content-Type: text/plain"),
		// STS rejection during a split-host exchange (no typed sentinel).
		"sts 4xx": errors.New("token exchange: status 400: invalid_grant: subject_token expired"),
		// Expired core JWT surfaces as a wrapped ErrNotLoggedIn.
		"wrapped not-logged-in": &wrappedTestError{msg: "fetch profile", inner: auth.ErrNotLoggedIn},
	}

	for name, fetchErr := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			target := statusTarget{coreURL: testCoreURL, token: "tok"}
			var out bytes.Buffer
			if err := runAuthStatus(context.Background(), &out, rejecting(fetchErr), noSessions, target, authStatusOptions{}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(out.String(), "no longer valid") {
				t.Fatalf("output = %q, want invalid-token message", out.String())
			}
			if !strings.Contains(out.String(), "entire login") {
				t.Fatalf("output = %q, want re-auth hint", out.String())
			}
		})
	}
}

// wrappedTestError is a tiny stand-in for fmt.Errorf("...: %w", inner).
type wrappedTestError struct {
	msg   string
	inner error
}

func (e *wrappedTestError) Error() string { return e.msg + ": " + e.inner.Error() }
func (e *wrappedTestError) Unwrap() error { return e.inner }

func TestRunAuthStatus_ServerError(t *testing.T) {
	t.Parallel()

	target := statusTarget{coreURL: testCoreURL, token: "tok"}

	var out bytes.Buffer
	err := runAuthStatus(context.Background(), &out, rejecting(errors.New("connection refused")), noSessions, target, authStatusOptions{})
	if err == nil {
		t.Fatal("expected error for non-401 failure")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error = %v, want underlying message", err)
	}
}

// --- registration -----------------------------------------------------------

func TestAuthCmd_RegistersExpectedSubcommands(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var authCmd *struct{}
	for _, c := range root.Commands() {
		if c.Use == "auth" {
			authCmd = &struct{}{}
			subcommands := map[string]bool{}
			for _, sub := range c.Commands() {
				name := strings.Fields(sub.Use)[0]
				subcommands[name] = true
			}
			for _, want := range []string{"login", "logout", "status", "contexts", "switch"} {
				if !subcommands[want] {
					t.Errorf("auth missing subcommand %q (got: %v)", want, subcommands)
				}
			}
		}
	}
	if authCmd == nil {
		t.Fatal("auth command not registered on root")
	}
}

// --- isKeychainTokenRejected -----------------------------------------------

func TestIsKeychainTokenRejected_AllShapes(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		err  error
		want bool
	}{
		"data API 401":           {&api.HTTPError{StatusCode: http.StatusUnauthorized}, true},
		"data API 500":           {&api.HTTPError{StatusCode: http.StatusInternalServerError}, false},
		"ErrNotLoggedIn":         {auth.ErrNotLoggedIn, true},
		"wrapped ErrNotLoggedIn": {errors.New("resolve API token: " + auth.ErrNotLoggedIn.Error()), false /* string-only, no chain — not detected */},
		"sts 401":                {errors.New("token exchange: status 401: invalid_client"), true},
		"sts 400 invalid_grant":  {errors.New("token exchange: status 400: invalid_grant: token expired"), true},
		"sts 500":                {errors.New("token exchange: status 500: server_error"), false},
		"network error":          {errors.New("dial tcp: i/o timeout"), false},
		// ogen decode failure on a non-JSON 401 body (the /me cross-core case).
		"non-JSON 401 decode": {errors.New("decode response: default (code 401): unexpected Content-Type: text/plain"), true},
		"non-JSON 500 decode": {errors.New("decode response: default (code 500): unexpected Content-Type: text/plain"), false},
	}

	// Confirm wrapped chains do propagate (the "wrapped ErrNotLoggedIn"
	// case above uses string substitution which intentionally doesn't
	// preserve the sentinel; this case uses fmt.Errorf %w which does).
	cases["fmt.Errorf %w ErrNotLoggedIn"] = struct {
		err  error
		want bool
	}{errors.Join(errors.New("resolve API token"), auth.ErrNotLoggedIn), true}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := isKeychainTokenRejected(tc.err); got != tc.want {
				t.Errorf("isKeychainTokenRejected(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestAuthCmd_TopLevelLoginAndLogoutStillRegistered(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	want := map[string]bool{"login": false, "logout": false}
	for _, c := range root.Commands() {
		if _, ok := want[c.Use]; ok {
			want[c.Use] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("top-level %q command should remain registered", name)
		}
	}
}

// The Token: provenance line must reflect the configured credential backend:
// with ENTIRE_TOKEN_STORE=file the token lives in a JSON file, not the OS
// keychain, and claiming otherwise misleads exactly the headless users the
// file backend exists for (#1036).
func TestRunAuthStatus_FileTokenStoreProvenance(t *testing.T) {
	// Not parallel: t.Setenv.
	t.Setenv("ENTIRE_TOKEN_STORE", "file")
	t.Setenv("ENTIRE_TOKEN_STORE_PATH", "/ci/secrets/tokens.json")

	target := statusTarget{coreURL: testCoreURL, token: "tok", activeContext: "core"}
	listSessions := func(context.Context, string, string) ([]api.AuthSession, error) { return nil, nil }

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, okProfile, listSessions, target, authStatusOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	if !hasMetadataRow(got, "token", "file /ci/secrets/tokens.json") {
		t.Fatalf("output = %q, want the file-backend provenance line", got)
	}
	if strings.Contains(got, "keychain") {
		t.Fatalf("output = %q, must not claim the OS keychain when the file backend is configured", got)
	}
}
