// Package clilogin runs the loopback OAuth code grant that native CLIs use to
// log in to entire-core. The CLI listens on 127.0.0.1:<random-port>, opens
// the browser at /cli/login (the interstitial that forwards to canonical
// /authorize), and waits for the OAuth callback to drop an auth code on the
// loopback URL. The code is then redeemed at /oauth/token for a JWT +
// refresh token.
//
// The CLI side is a PKCE-only public OAuth client; the server-side OAuth
// client_id+secret for GitHub stay on entire-core where they belong.
package clilogin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
)

// expectedAccessTokenAlgs is the algorithm whitelist josejwt.ParseSigned
// needs even for unsafe (no-verify) parsing. entire-core's KeyStore is
// single-alg (EdDSA); listing only that matches what we expect and rejects
// anything else as malformed before claim extraction.
var expectedAccessTokenAlgs = []jose.SignatureAlgorithm{jose.EdDSA}

// DefaultClientID is the public OAuth client ID `entire-core auth login` uses
// against entire-core's loopback PKCE flow. Distinct from "entire-cli" (the
// user-facing entire CLI's device-flow id) — this one identifies the
// operator/admin CLI bundled in this repo. Must be present in
// OAUTH_CLIENTS_JSON as a "public" client with loopback redirect URIs.
const DefaultClientID = "entire-core-cli"

// callbackPath is the loopback redirect path. Kept fixed (rather than random)
// so the OAuth client only needs to register http://127.0.0.1/callback once,
// and the loopback redirect-URI matcher can compare paths exactly.
const callbackPath = "/callback"

// Result is what a successful loopback login returns.
type Result struct {
	Token        string
	RefreshToken string
	AccountID    string
	Handle       string
	// Provider names the upstream IdP that authenticated the user
	// (e.g. "github"). Used by the CLI to build a default context name
	// like "<provider>:<handle>@<host>".
	Provider string
	// ExpiresIn is the access-token lifetime in seconds, as advertised by
	// the IdP's /oauth/token response. Zero when the server didn't
	// include it (older deployments).
	ExpiresIn int64
	// IssuerURL is the entire-core base URL that actually minted the
	// access token. Taken from the RFC 9207 `iss` query parameter on the
	// OAuth callback (which the home region sets to its own external
	// URL), falling back to opts.CoreBaseURL when absent. In a same-
	// region flow this equals opts.CoreBaseURL; in a cross-region flow
	// (CLI pointed at US for an EU-homed user) the foreign-redirect
	// chain ends with the home region issuing the auth code, and the
	// hint points there. Callers should key keychain entries and
	// refresh endpoints on this rather than the URL the user typed.
	//
	// Note: this is distinct from the JWT's own `iss` claim. Both are
	// URLs after COR-135 and may even be equal for same-region flows,
	// but IssuerURL here is the home-region hint from the OAuth callback,
	// used for keychain keying and refresh routing — not for signature
	// dispatch.
	IssuerURL string
}

// Options tunes the flow. Zero values pick reasonable defaults.
type Options struct {
	// CoreBaseURL is the entire-core base URL (e.g. https://eu.auth.partial.to).
	// Required.
	CoreBaseURL string

	// ClientID overrides the OAuth client_id. Defaults to DefaultClientID.
	ClientID string

	// HTTPClient is used for the back-channel /oauth/token POST. Defaults
	// to http.DefaultClient. CLIs that opt into TLS skipping for dev should
	// pass their own here.
	HTTPClient *http.Client

	// Browser opens the given URL in the user's default browser. Defaults to
	// a no-op (the caller is responsible for printing the URL); injectable
	// for tests.
	Browser func(url string) error

	// Stdout receives status messages ("Authenticating against …", "Open this
	// URL …"). Defaults to io.Discard.
	Stdout io.Writer

	// Timeout bounds the whole flow. Defaults to 5 minutes — generous enough
	// for typing a GitHub 2FA code, tight enough that a forgotten browser tab
	// doesn't hold the listener forever.
	Timeout time.Duration
}

// Run performs the loopback OAuth flow and returns the redeemed JWT.
func Run(ctx context.Context, opts Options) (*Result, error) {
	coreBase := strings.TrimRight(opts.CoreBaseURL, "/")
	if coreBase == "" {
		return nil, errors.New("CoreBaseURL is required")
	}
	clientID := opts.ClientID
	if clientID == "" {
		clientID = DefaultClientID
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	verifier, challenge, err := newPKCEPair()
	if err != nil {
		return nil, fmt.Errorf("generate PKCE: %w", err)
	}
	state, err := newRandomB64URL(24)
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("open loopback listener: %w", err)
	}
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return nil, fmt.Errorf("loopback listener returned non-TCP address: %T", listener.Addr())
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", tcpAddr.Port, callbackPath)

	type callbackResult struct {
		code string
		// issuer is the value of the `iss` query parameter (RFC 9207),
		// which the home region adds to the loopback redirect to tell us
		// where to redeem the auth code. Empty when the server didn't
		// emit it (older deployments) — falls back to opts.CoreBaseURL.
		issuer string
		err    error
	}
	resultCh := make(chan callbackResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		got := q.Get("state")
		if got != state {
			// Could be an unrelated process probing 127.0.0.1, or a stray
			// browser tab. Reject this request without deciding the flow —
			// the real OAuth provider, which echoes our high-entropy state,
			// can still arrive on a later request.
			http.Error(w, "state mismatch — refusing to redeem auth code", http.StatusBadRequest)
			return
		}
		if oerr := q.Get("error"); oerr != "" {
			http.Error(w, "authentication failed: "+oerr, http.StatusBadRequest)
			select {
			case resultCh <- callbackResult{err: fmt.Errorf("OAuth error: %s", oerr)}:
			default:
			}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			select {
			case resultCh <- callbackResult{err: errors.New("OAuth callback missing code")}:
			default:
			}
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, loopbackSuccessHTML) //nolint:errcheck // best-effort UI
		select {
		case resultCh <- callbackResult{code: code, issuer: q.Get("iss")}:
		default:
		}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- srv.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx) //nolint:errcheck // best-effort cleanup
	}()

	q := neturl.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	loginURL := coreBase + "/cli/login?" + q.Encode()

	fmt.Fprintf(stdout, "Authenticating against %s\n", coreBase)
	if opts.Browser != nil {
		_ = opts.Browser(loginURL) //nolint:errcheck // best-effort browser open; URL is printed below
	}
	fmt.Fprintf(stdout, "If a browser didn't open automatically, visit:\n  %s\n", loginURL)

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		code      string
		issuerURL string
	)
	select {
	case res := <-resultCh:
		if res.err != nil {
			return nil, res.err
		}
		code = res.code
		issuerURL = strings.TrimRight(res.issuer, "/")
	case err := <-srvErrCh:
		// Listener stopped before we got a callback. Treat ErrServerClosed
		// as "we shut down on the deferred path" only; otherwise it's an
		// abnormal failure.
		if errors.Is(err, http.ErrServerClosed) {
			return nil, errors.New("loopback listener closed before callback arrived")
		}
		return nil, fmt.Errorf("loopback listener: %w", err)
	case <-waitCtx.Done():
		return nil, fmt.Errorf("timed out waiting for browser sign-in: %w", waitCtx.Err())
	}

	// The home region's iss hint identifies which entire-core minted the
	// code. Fall back to coreBase when absent (older servers, same-region
	// flows that haven't yet been updated to emit it).
	tokenEndpointBase := issuerURL
	if tokenEndpointBase == "" {
		tokenEndpointBase = coreBase
	}
	result, err := exchangeCode(ctx, httpClient, tokenEndpointBase, clientID, code, redirectURI, verifier)
	if err != nil {
		return nil, err
	}

	// The RFC 9207 hint is the home region's own external URL, set by
	// the server on the loopback redirect. It's also the URL the auth
	// code is valid at — we just successfully redeemed it above, so
	// trusting it for downstream STS routing is sound. Falls back to
	// the typed URL when older servers don't emit the hint.
	if issuerURL != "" {
		result.IssuerURL = issuerURL
	} else {
		result.IssuerURL = coreBase
	}
	return result, nil
}

func exchangeCode(ctx context.Context, httpClient *http.Client, coreBase, clientID, code, redirectURI, verifier string) (*Result, error) {
	form := neturl.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)
	form.Set("client_id", clientID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, coreBase+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var oerr struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &oerr) //nolint:errcheck // best-effort decode
		if oerr.Error != "" {
			return nil, fmt.Errorf("token exchange failed (HTTP %d, %s): %s", resp.StatusCode, oerr.Error, oerr.ErrorDescription)
		}
		return nil, fmt.Errorf("token exchange failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var ok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &ok); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if ok.AccessToken == "" {
		return nil, errors.New("token exchange response missing access_token (server may not be configured to issue JWTs to this client)")
	}
	// /oauth/token's RFC 6749 response only carries the tokens; account
	// identity rides as claims on the access JWT. We read those without
	// verifying — entire-core just issued the token, the loopback flow's
	// trust boundary is the OAuth state-check + back-channel POST that
	// got us here. An opaque token, a non-EdDSA alg, or missing claims all
	// surface as a clear error here rather than handing the caller a Result
	// with silently empty identity that downstream prompts would mislabel.
	accountID, handle, provider := identityFromAccessToken(ok.AccessToken)
	if accountID == "" || handle == "" {
		return nil, errors.New("access token missing identity claims (sub/handle); is the issuer entire-core?")
	}
	return &Result{
		Token:        ok.AccessToken,
		RefreshToken: ok.RefreshToken,
		AccountID:    accountID,
		Handle:       handle,
		Provider:     provider,
		ExpiresIn:    ok.ExpiresIn,
	}, nil
}

// identityFromAccessToken extracts the entire-core identity claims (sub,
// handle, provider) from an access JWT. Failures fall through to zero
// values: the caller surfaces a friendlier error from the downstream
// "missing handle"/"missing account_id" checks than we could here.
func identityFromAccessToken(accessToken string) (accountID, handle, provider string) {
	parsed, err := josejwt.ParseSigned(accessToken, expectedAccessTokenAlgs)
	if err != nil {
		return "", "", ""
	}
	var claims struct {
		Subject  string `json:"sub"`
		Handle   string `json:"handle"`
		Provider string `json:"provider"`
	}
	if err := parsed.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return "", "", ""
	}
	return claims.Subject, claims.Handle, claims.Provider
}

func newPKCEPair() (verifier, challenge string, err error) {
	v, err := newRandomB64URL(48)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func newRandomB64URL(nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const loopbackSuccessHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Sign-in complete</title>
<style>
  body { font-family: system-ui, -apple-system, sans-serif; display: flex; align-items: center; justify-content: center; height: 100vh; margin: 0; background: #f5f5f5; }
  .card { background: white; padding: 2.5rem; border-radius: 8px; box-shadow: 0 2px 8px rgba(0,0,0,0.1); text-align: center; max-width: 380px; }
  h2 { margin: 0 0 0.5rem; color: #2c974b; }
  p { color: #555; margin: 0.5rem 0; }
</style>
</head>
<body>
<div class="card">
  <h2>You're signed in</h2>
  <p>You can close this tab and return to your terminal.</p>
</div>
</body>
</html>
`
