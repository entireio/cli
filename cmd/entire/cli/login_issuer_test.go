package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// testRegionalIssuer is the region the apex hands a login off to in these
// tests — the issuer that actually mints the token.
const testRegionalIssuer = "https://us.auth.entire.io"

// The production login server (https://auth.entire.io) is a dispatcher: it
// hands the login off to a regional login server, which mints the token with
// iss set to its own host. issMatches therefore has to accept a strict
// subdomain of the dialled host — and nothing that merely looks like one.
func TestIssMatches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		claimed  string
		expected string
		wantOK   bool
	}{
		{"exact", "https://auth.entire.io", "https://auth.entire.io", true},
		{"trailing slash on claim", "https://auth.entire.io/", "https://auth.entire.io", true},
		{"path stripped from claim", "https://auth.entire.io/oauth/token", "https://auth.entire.io", true},
		{"case and default port normalised", "HTTPS://AUTH.ENTIRE.IO:443", "https://auth.entire.io", true},

		{"regional subdomain", "https://us.auth.entire.io", "https://auth.entire.io", true},
		{"other region", "https://eu.auth.entire.io", "https://auth.entire.io", true},
		{"staging apex delegates too", "https://eu.auth.partial.to", "https://auth.partial.to", true},
		{"deeper subdomain", "https://a.b.auth.entire.io", "https://auth.entire.io", true},

		// The whole point of the label-boundary check.
		{"suffix lookalike", "https://auth.entire.io.evil.com", "https://auth.entire.io", false},
		{"prefix lookalike without a label boundary", "https://xauth.entire.io", "https://auth.entire.io", false},
		{"parent is not below the dialled host", "https://entire.io", "https://auth.entire.io", false},
		{"sibling", "https://api.entire.io", "https://auth.entire.io", false},
		{"unrelated host", "https://evil.example", "https://auth.entire.io", false},
		{"host smuggled in the query", "https://evil.example/?h=us.auth.entire.io", "https://auth.entire.io", false},
		{"trailing-dot FQDN", "https://us.auth.entire.io.", "https://auth.entire.io", false},

		// Delegation must never downgrade the transport or cross a port.
		{"http claim under an https apex", "http://us.auth.entire.io", "https://auth.entire.io", false},
		{"https claim under an http apex", "https://us.auth.entire.io", "http://auth.entire.io", false},
		{"port mismatch", "https://us.auth.entire.io:8443", "https://auth.entire.io", false},
		{"explicit matching port delegates", "https://us.auth.entire.io:8443", "https://auth.entire.io:8443", true},

		// A dev/loopback login server issues its own tokens; exact only.
		{"loopback exact", "http://127.0.0.1:8787", "http://127.0.0.1:8787", true},
		{"loopback port mismatch", "http://127.0.0.1:9999", "http://127.0.0.1:8787", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := issMatches(tc.claimed, tc.expected)
			if tc.wantOK && err != nil {
				t.Fatalf("issMatches(%q, %q) = %v, want nil", tc.claimed, tc.expected, err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("issMatches(%q, %q) = nil, want error", tc.claimed, tc.expected)
			}
		})
	}
}

// The mismatch message must not advertise subdomain delegation for an origin
// that can never delegate — a plaintext dev/loopback login server issues its
// own tokens, so pointing at "a subdomain of it" would be a dead end.
func TestIssMatches_ErrorNamesDelegationOnlyWhenPossible(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		claimed       string
		expected      string
		wantSubdomain bool
	}{
		{"https apex can delegate", "https://evil.example", "https://auth.entire.io", true},
		{"http dev server cannot", "http://evil.example", "http://127.0.0.1:8787", false},
		{"https claim under an http dev server", "https://us.auth.entire.io", "http://auth.entire.io", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := issMatches(tc.claimed, tc.expected)
			if err == nil {
				t.Fatalf("issMatches(%q, %q) = nil, want error", tc.claimed, tc.expected)
			}
			got := strings.Contains(err.Error(), "or a subdomain of it")
			if got != tc.wantSubdomain {
				t.Fatalf("issMatches(%q, %q) = %q; mentions subdomain = %v, want %v",
					tc.claimed, tc.expected, err, got, tc.wantSubdomain)
			}
		})
	}
}

func TestAdoptIssuer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		claimed string
		dialled string
		want    string
		wantErr bool
	}{
		{"none claimed", "", "https://auth.entire.io", "", false},
		{"whitespace only", "   ", "https://auth.entire.io", "", false},
		{"same origin needs no retarget", "https://auth.entire.io", "https://auth.entire.io", "", false},
		{"cosmetic difference needs no retarget", "https://AUTH.entire.io:443/", "https://auth.entire.io", "", false},
		{"regional handoff", "https://us.auth.entire.io", "https://auth.entire.io", "https://us.auth.entire.io", false},
		{"normalised handoff", "https://US.AUTH.ENTIRE.IO/", "https://auth.entire.io", "https://us.auth.entire.io", false},
		{"hostile handoff", "https://auth.entire.io.evil.com", "https://auth.entire.io", "", true},
		{"downgrade handoff", "http://us.auth.entire.io", "https://auth.entire.io", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := adoptIssuer(tc.claimed, tc.dialled)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("adoptIssuer(%q, %q) = %q, want error", tc.claimed, tc.dialled, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("adoptIssuer(%q, %q): %v", tc.claimed, tc.dialled, err)
			}
			if got != tc.want {
				t.Errorf("adoptIssuer(%q, %q) = %q, want %q", tc.claimed, tc.dialled, got, tc.want)
			}
		})
	}
}

// The apex serves no token endpoint, so the code exchange must follow the
// RFC 9207 iss the callback named.
func TestRunBrowserLogin_RetargetsExchangeToCallbackIssuer(t *testing.T) {
	t.Parallel()

	flow := &fakeBrowserFlow{
		authURL:  "https://auth.entire.io/authorize?x=1",
		waitCode: "code-1",
		issuer:   testRegionalIssuer,
		// Stop before persistLogin, which would reach the credential store.
		exchErr: errors.New("stop before persist"),
	}

	err := runBrowserLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, flow,
		"https://auth.entire.io", newTestLoginURLInteractor(), browserLoginTimeout)
	if err == nil || !strings.Contains(err.Error(), "stop before persist") {
		t.Fatalf("runBrowserLogin() = %v, want the exchange to have been attempted", err)
	}
	if flow.tokenIssuerCalls != 1 {
		t.Fatalf("UseTokenIssuer calls = %d, want 1", flow.tokenIssuerCalls)
	}
	if flow.gotTokenIssuer != testRegionalIssuer {
		t.Fatalf("UseTokenIssuer(%q), want %s", flow.gotTokenIssuer, testRegionalIssuer)
	}
	if flow.gotExchangeCode != "code-1" {
		t.Fatalf("Exchange code = %q, want code-1", flow.gotExchangeCode)
	}
}

func TestRunBrowserLogin_NoRetargetWhenIssuerMatchesOrIsAbsent(t *testing.T) {
	t.Parallel()

	for name, issuer := range map[string]string{
		"no iss parameter":            "",
		"iss equals the dialled host": "https://auth.entire.io",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			flow := &fakeBrowserFlow{
				authURL:  "https://auth.entire.io/authorize",
				waitCode: "code-1",
				issuer:   issuer,
				exchErr:  errors.New("stop before persist"),
			}
			err := runBrowserLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, flow,
				"https://auth.entire.io", newTestLoginURLInteractor(), browserLoginTimeout)
			if err == nil {
				t.Fatal("runBrowserLogin() = nil, want the stubbed exchange error")
			}
			if flow.tokenIssuerCalls != 0 {
				t.Fatalf("UseTokenIssuer calls = %d, want 0", flow.tokenIssuerCalls)
			}
		})
	}
}

// A callback that names a host outside the dialled server's delegation must
// abort before the authorization code is sent anywhere.
func TestRunBrowserLogin_RejectsHostileCallbackIssuer(t *testing.T) {
	t.Parallel()

	flow := &fakeBrowserFlow{
		authURL:    "https://auth.entire.io/authorize",
		waitCode:   "code-1",
		issuer:     "https://auth.entire.io.evil.com",
		exchAccess: "should-never-be-reached",
	}

	err := runBrowserLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, flow,
		"https://auth.entire.io", newTestLoginURLInteractor(), browserLoginTimeout)
	if err == nil || !strings.Contains(err.Error(), "unacceptable issuer") {
		t.Fatalf("runBrowserLogin() = %v, want an unacceptable-issuer error", err)
	}
	if flow.tokenIssuerCalls != 0 {
		t.Fatalf("UseTokenIssuer calls = %d, want 0", flow.tokenIssuerCalls)
	}
	if flow.gotExchangeCode != "" {
		t.Fatalf("Exchange was called with %q; the code must not leave the machine", flow.gotExchangeCode)
	}
}

// deviceStart builds a device-authorization response as if it had been
// served by responseOrigin.
func deviceStart(responseOrigin string) *auth.DeviceAuthStart {
	return &auth.DeviceAuthStart{
		DeviceCode:      "dev-1",
		UserCode:        "WDJB-MJHT",
		VerificationURI: responseOrigin + "/device",
		ExpiresIn:       600,
		Interval:        1,
		ResponseOrigin:  responseOrigin,
	}
}

func TestRunLogin_RetargetsPollToTheRespondingRegion(t *testing.T) {
	t.Parallel()

	client := &mockClient{
		baseURL: "https://auth.entire.io",
		start:   deviceStart("https://us.auth.entire.io"),
		// Terminate the poll loop before persistLogin.
		responses: []pollResponse{{result: &auth.DeviceAuthPoll{Error: "access_denied"}}},
	}

	err := runLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, client, newTestLoginURLInteractor(), false)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("runLogin() = %v, want the stubbed access_denied", err)
	}
	if client.gotTokenIssuer != "https://us.auth.entire.io" {
		t.Fatalf("UseTokenIssuer(%q), want https://us.auth.entire.io", client.gotTokenIssuer)
	}
	if client.pollDeviceCodeArg != "dev-1" {
		t.Fatalf("polled device code = %q, want dev-1", client.pollDeviceCodeArg)
	}
}

func TestRunLogin_NoRetargetWhenTheApexAnsweredItself(t *testing.T) {
	t.Parallel()

	client := &mockClient{
		baseURL:   "https://auth.entire.io",
		start:     deviceStart("https://auth.entire.io"),
		responses: []pollResponse{{result: &auth.DeviceAuthPoll{Error: "access_denied"}}},
	}

	if err := runLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, client, newTestLoginURLInteractor(), false); err == nil {
		t.Fatal("runLogin() = nil, want the stubbed access_denied")
	}
	if client.tokenIssuerCalls != 0 {
		t.Fatalf("UseTokenIssuer calls = %d, want 0", client.tokenIssuerCalls)
	}
}

func TestRunLogin_RejectsHostileDeviceResponseOrigin(t *testing.T) {
	t.Parallel()

	client := &mockClient{
		baseURL:   "https://auth.entire.io",
		start:     deviceStart("https://auth.entire.io.evil.com"),
		responses: []pollResponse{{result: &auth.DeviceAuthPoll{AccessToken: "should-never-be-reached"}}},
	}

	err := runLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, client, newTestLoginURLInteractor(), false)
	if err == nil || !strings.Contains(err.Error(), "unacceptable issuer") {
		t.Fatalf("runLogin() = %v, want an unacceptable-issuer error", err)
	}
	if client.tokenIssuerCalls != 0 {
		t.Fatalf("UseTokenIssuer calls = %d, want 0", client.tokenIssuerCalls)
	}
	if client.calls != 0 {
		t.Fatalf("poll calls = %d; the device code must not be sent anywhere", client.calls)
	}
}

// validateReceivedToken sits on issMatches, so a token minted by the region
// the apex delegated to is accepted while a lookalike is not.
func TestValidateReceivedToken_AcceptsRegionalIssuerUnderTheApex(t *testing.T) {
	t.Parallel()

	now := time.Now()
	exp := now.Add(time.Hour).Unix()

	ok := makeJWT(t, `{"alg":"RS256"}`, fmt.Sprintf(`{"iss":"https://us.auth.entire.io","handle":"alice","exp":%d}`, exp))
	if err := validateReceivedToken(ok, "https://auth.entire.io", now); err != nil {
		t.Fatalf("validateReceivedToken(regional iss) = %v, want nil", err)
	}

	evil := makeJWT(t, `{"alg":"RS256"}`, fmt.Sprintf(`{"iss":"https://auth.entire.io.evil.com","handle":"alice","exp":%d}`, exp))
	if err := validateReceivedToken(evil, "https://auth.entire.io", now); err == nil {
		t.Fatal("validateReceivedToken(lookalike iss) = nil, want an iss-mismatch error")
	}
}

func TestLoginCompleteLine(t *testing.T) {
	t.Parallel()

	exp := time.Now().Add(time.Hour).Unix()
	regional := makeJWT(t, `{"alg":"RS256"}`, fmt.Sprintf(`{"iss":"https://us.auth.entire.io","handle":"alice","exp":%d}`, exp))
	same := makeJWT(t, `{"alg":"RS256"}`, fmt.Sprintf(`{"iss":"https://auth.entire.io","handle":"alice","exp":%d}`, exp))

	if got := loginCompleteLine(regional, "https://auth.entire.io"); !strings.Contains(got, "https://us.auth.entire.io") {
		t.Errorf("loginCompleteLine(regional) = %q, want it to name the issuer", got)
	}
	if got := loginCompleteLine(same, "https://auth.entire.io"); got != "✓ Login complete." {
		t.Errorf("loginCompleteLine(same issuer) = %q, want the plain message", got)
	}
	if got := loginCompleteLine("not-a-jwt", "https://auth.entire.io"); got != "✓ Login complete." {
		t.Errorf("loginCompleteLine(unparseable) = %q, want the plain message", got)
	}
}

// A completed dispatched login records the region as the context's CoreURL
// and says so on stdout — the dialled apex is not what later refreshes and
// token exchanges target.
func TestPersistLogin_DispatchedLoginNamesTheRegion(t *testing.T) {
	// Not parallel: mutates the process-global tokenstore backend.
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

	var out bytes.Buffer
	token := loginTestJWT(t, testRegionalIssuer)
	if err := persistLogin(&out, "https://auth.entire.io", testRegionalIssuer, token, "refresh-token"); err != nil {
		t.Fatalf("persistLogin() = %v, want nil", err)
	}
	if !strings.Contains(out.String(), testRegionalIssuer) {
		t.Fatalf("stdout = %q, want it to name the issuing region", out.String())
	}
	// The load-bearing half: the persisted context must be keyed on the region,
	// not the dialled apex. That CoreURL is what every later refresh and RFC
	// 8693 exchange targets, and the apex serves no token endpoint — so if this
	// recorded the apex, refresh would 404 and the login would silently be
	// single-use. Asserting only the stdout line above would leave that
	// verified by a cosmetic string.
	ctxs, active, err := auth.Contexts()
	if err != nil {
		t.Fatalf("auth.Contexts() = %v, want nil", err)
	}
	var got string
	for _, c := range ctxs {
		if c.Name == active {
			got = c.CoreURL
			break
		}
	}
	if got != testRegionalIssuer {
		t.Fatalf("active context CoreURL = %q, want the issuing region (contexts=%+v, active=%q)", got, ctxs, active)
	}
}

// iss names the signer, and a verifier resolves JWKS from it — so only the
// region that answered the handoff can legitimately claim it. A token arriving
// from us.auth.entire.io but claiming a sibling region is misconfiguration, and
// validating against the dialled apex alone would wave it through because both
// sit under it. The account's own home region travels in home_jurisdiction, not
// in iss, so a cross-region mint needs no leniency here.
func TestPersistLogin_RejectsATokenFromAnotherRegion(t *testing.T) {
	// Not parallel: mutates the process-global tokenstore backend.
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

	var out bytes.Buffer
	// Handed off to us.auth.entire.io; token claims eu.auth.entire.io.
	token := loginTestJWT(t, "https://eu.auth.entire.io")
	err := persistLogin(&out, "https://auth.entire.io", testRegionalIssuer, token, "refresh-token")
	if err == nil {
		t.Fatal("persistLogin() = nil, want an iss-mismatch rejection")
	}
	if !strings.Contains(err.Error(), "iss mismatch") {
		t.Fatalf("persistLogin() = %v, want an iss-mismatch error", err)
	}
	// Nothing may be persisted from a rejected token.
	ctxs, _, cerr := auth.Contexts()
	if cerr != nil {
		t.Fatalf("auth.Contexts() = %v, want nil", cerr)
	}
	if len(ctxs) != 0 {
		t.Fatalf("contexts = %+v, want none recorded for a rejected token", ctxs)
	}
}
