package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// ServiceAccountSecretPrefix starts a service account secret. An ENTIRE_TOKEN
// with this prefix is never sent as a bearer: it is exchanged at a Core for a
// short-lived service account access token, and that token is the bearer.
const ServiceAccountSecretPrefix = "ent_svc_" //nolint:gosec // G101: a prefix, not a credential

const (
	// secretClientID is the public OAuth client the exchange names.
	secretClientID = "entire-cli"
	// secretSubjectTokenType is the RFC 8693 subject_token_type for a secret.
	secretSubjectTokenType = "urn:ietf:params:oauth:token-type:access_token" //nolint:gosec // G101: a URN, not a credential
	// secretTokenMargin discards a cached token this long before it expires,
	// so a long command never starts with a token about to lapse.
	secretTokenMargin = 2 * time.Minute
	// secretExchangeTimeout bounds the exchange; ParseEnvToken has no caller
	// context.
	secretExchangeTimeout = 30 * time.Second
	// secretCacheMaxAge bounds how long a cached token is reused, whatever
	// its lifetime. A token Core invalidates early stops being reused
	// within this window, even by a command with no 401 hook.
	secretCacheMaxAge = 10 * time.Minute
	// secretCacheDir holds exchanged tokens under the user cache root.
	secretCacheDir  = "sa-tokens"
	maxExchangeBody = 1 << 20
)

// secretExchangeTransport is the test seam for the exchange round trip.
var secretExchangeTransport http.RoundTripper

// UseSecretExchangeTransportForTesting routes service account secret
// exchanges through rt and returns the restore function. Tests outside this
// package, such as git-remote-entire's, use it; it changes process state, so
// such tests are not parallel.
func UseSecretExchangeTransportForTesting(rt http.RoundTripper) func() {
	prev := secretExchangeTransport
	secretExchangeTransport = rt
	return func() { secretExchangeTransport = prev }
}

// secretNow is the test seam for the cache clock.
var secretNow = time.Now

// CredentialSource names which credential a process authenticates with, for
// status output and scripts.
type CredentialSource string

const (
	CredentialFromContext  CredentialSource = "context"
	CredentialFromEnvToken CredentialSource = "env_token"
	// CredentialFromServiceAccountSecret is a secret in ENTIRE_TOKEN, sent
	// only to the exchange; the bearer is the token it was exchanged for.
	CredentialFromServiceAccountSecret CredentialSource = "sa_secret"
)

// EnvTokenCredential names the credential an ENTIRE_TOKEN value supplies.
func EnvTokenCredential(raw string) CredentialSource {
	if IsServiceAccountSecret(strings.TrimSpace(raw)) {
		return CredentialFromServiceAccountSecret
	}
	return CredentialFromEnvToken
}

// IsServiceAccountSecret reports whether token is a service account secret.
func IsServiceAccountSecret(token string) bool {
	return strings.HasPrefix(token, ServiceAccountSecretPrefix)
}

// ResolveEnvToken is ResolveEnvTokenContext for callers with no context, such
// as ParseEnvToken. The exchange still has secretExchangeTimeout.
func ResolveEnvToken(raw string) (string, error) {
	return ResolveEnvTokenContext(context.Background(), raw)
}

// ResolveEnvTokenContext trims an ENTIRE_TOKEN value and fails closed on a
// blank one. A service account secret becomes a service account access
// token, from the per-user cache when one is still fresh; ctx cancels the
// exchange. Any other value is returned as is.
func ResolveEnvTokenContext(ctx context.Context, raw string) (string, error) {
	token := strings.TrimSpace(raw)
	if token == "" {
		return "", fmt.Errorf("%s is set but blank", EnvTokenVar)
	}
	if !IsServiceAccountSecret(token) {
		return token, nil
	}
	ctx, cancel := context.WithTimeout(ctx, secretExchangeTimeout)
	defer cancel()
	return exchangeServiceAccountSecret(ctx, token)
}

// serviceAccountSecretCacheKey is LocalIdentityCacheKey for a secret: the
// exchange Core and the service account, neither of them secret. It needs no
// exchange, so a cache lookup never goes over the network.
func serviceAccountSecretCacheKey(secret string) (string, error) {
	subj, err := secretExchangeSubject(secret)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{"env-sa", subj.core, subj.accountID}, "|"), nil
}

// exchangeSubject is everything one service account token exchange needs. A
// secret is one kind of subject; another credential (such as a key-signed
// assertion) can build its own and reuse the exchange, the token checks and
// the locked cache.
type exchangeSubject struct {
	// token and tokenType are the RFC 8693 subject_token and its type.
	token     string
	tokenType string
	// accountID is the svc_ account the exchanged token must be for.
	accountID string
	// core is the origin that exchanges the subject, and the token's only
	// URL audience.
	core string
	// cacheName names the cache entry and its lock, without holding the
	// subject token.
	cacheName string
}

// secretExchangeSubject parses secret and returns its exchange subject. The
// exchange, the cache keys and ForgetServiceAccountSecretToken all derive the
// Core here, so they agree.
func secretExchangeSubject(secret string) (exchangeSubject, error) {
	parsed, err := parseServiceAccountSecret(secret)
	if err != nil {
		return exchangeSubject{}, err
	}
	core := jurisdictionCoreURL(parsed.jurisdiction)
	return exchangeSubject{
		token:     secret,
		tokenType: secretSubjectTokenType,
		accountID: parsed.accountID,
		core:      core,
		cacheName: secretCacheKey(core, secret),
	}, nil
}

// jurisdictionCoreURL returns https://<jurisdiction>.<default login host>,
// for example https://eu.auth.entire.io. Core guarantees this layout: the v1
// secret contract defines the exchange Core as https://<juri>.auth.<domain>,
// and the SDK derives it the same way. There is no override: the secret is
// sent to this origin and nothing else. A non-production Core exchanges the
// secret by hand and passes the resulting JWT in ENTIRE_TOKEN.
func jurisdictionCoreURL(jurisdiction string) string {
	return httpclient.SchemeHTTPS + "://" + jurisdiction + "." + strings.TrimPrefix(api.DefaultAuthBaseURL, httpclient.SchemeHTTPS+"://")
}

func exchangeServiceAccountSecret(ctx context.Context, secret string) (string, error) {
	subj, err := secretExchangeSubject(secret)
	if err != nil {
		return "", err
	}
	return exchangeSubjectToken(ctx, subj)
}

// exchangeSubjectToken returns a fresh token for subj from the cache, or
// exchanges it. Concurrent processes share one exchange: on a miss the first
// takes a per-subject lock and the rest find its result in the cache, so N
// parallel commands do not each spend Core's per-secret rate limit.
func exchangeSubjectToken(ctx context.Context, subj exchangeSubject) (string, error) {
	if token, ok := readCachedSecretToken(subj); ok {
		return token, nil
	}
	root, ok := secretCacheRoot()
	if !ok {
		// No cache directory: still authenticate, just without sharing.
		token, _, err := postSecretExchange(ctx, subj)
		return token, err
	}
	release, err := lockSecretCache(ctx, root, subj)
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		// The cache is best effort: a lock that cannot be taken (a file
		// system without lock support, say) costs sharing, not auth.
		token, _, err := postSecretExchange(ctx, subj)
		return token, err
	}
	defer release()
	if token, ok := readCachedSecretToken(subj); ok {
		return token, nil
	}
	token, expires, err := postSecretExchange(ctx, subj)
	if err != nil {
		return "", err
	}
	writeCachedSecretToken(root, subj, token, expires)
	return token, nil
}

// postSecretExchange exchanges subj at its Core and returns the checked
// token and when it expires.
func postSecretExchange(ctx context.Context, subj exchangeSubject) (string, time.Time, error) {
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token_type": {subj.tokenType},
		"subject_token":      {subj.token},
		"audience":           {subj.core},
		"resource":           {subj.accountID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, subj.core+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build %s exchange: %w", EnvTokenVar, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(secretClientID, "")
	client := httpclient.New(httpclient.Options{
		Transport: secretExchangeTransport,
		Timeout:   secretExchangeTimeout,
		// The body holds the secret. Go drops Authorization on a cross-host
		// redirect but a 307/308 resends the body, so never follow one: the
		// 3xx is reported as a refusal below.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	})
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("exchange %s service account secret at %s: %w", EnvTokenVar, subj.core, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxExchangeBody))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read %s exchange response: %w", EnvTokenVar, err)
	}
	var out struct {
		AccessToken      string `json:"access_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &out) //nolint:errcheck // a non-JSON body is reported by status below
	if resp.StatusCode != http.StatusOK {
		// The request holds the secret, so the error names only the reply.
		if out.Error != "" {
			return "", time.Time{}, fmt.Errorf("%s service account secret refused by %s: %s: %s", EnvTokenVar, subj.core, out.Error, out.ErrorDescription)
		}
		return "", time.Time{}, fmt.Errorf("%s service account secret refused by %s: HTTP %d", EnvTokenVar, subj.core, resp.StatusCode)
	}
	if out.AccessToken == "" {
		return "", time.Time{}, errors.New("service account token exchange returned no access token")
	}
	now := secretNow()
	expires, err := checkExchangedToken(subj, out.AccessToken, now)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%s returned an unusable service account token: %w", subj.core, err)
	}
	if byExpiresIn := now.Add(time.Duration(out.ExpiresIn) * time.Second); byExpiresIn.Before(expires) {
		expires = byExpiresIn
	}
	return out.AccessToken, expires, nil
}

// checkExchangedToken checks an exchanged token's unverified claims before it
// is cached or sent anywhere, and returns its exp. It must be for subj's
// account, name subj's Core as its only URL audience, and not have expired.
// Core verifies the signature; this only stops a confused or malicious reply
// from being cached as this account or carried to another origin, since
// routing (CoreURLFromEnvToken) follows the token's first URL audience.
func checkExchangedToken(subj exchangeSubject, token string, now time.Time) (time.Time, error) {
	claims, err := tokens.ParseClaims(token)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse claims: %w", err)
	}
	if claims.Subject != subj.accountID {
		return time.Time{}, fmt.Errorf("sub is %q, want %s", claims.Subject, subj.accountID)
	}
	var urlAuds []string
	for _, aud := range claims.Audience {
		if u, err := url.Parse(aud); err == nil && u.Scheme != "" {
			urlAuds = append(urlAuds, aud)
		}
	}
	if len(urlAuds) != 1 || strings.TrimRight(urlAuds[0], "/") != subj.core {
		return time.Time{}, fmt.Errorf("aud %v must name exactly one origin, %s", claims.Audience, subj.core)
	}
	if claims.ExpiresAt.IsZero() || !claims.ExpiresAt.After(now) {
		return time.Time{}, errors.New("token has no exp or has expired")
	}
	return claims.ExpiresAt, nil
}

// secretCacheKey names a cache file without storing the secret.
func secretCacheKey(core, secret string) string {
	sum := sha256.Sum256([]byte(core + "\x00" + secret))
	return hex.EncodeToString(sum[:16])
}

func secretCacheFile(subj exchangeSubject) string {
	return secretCacheDir + "/" + subj.cacheName + ".json"
}

// secretCacheRoot returns the user cache root with secretCacheDir in it.
func secretCacheRoot() (*os.Root, bool) {
	root, err := userdirs.CacheRoot()
	if err != nil {
		return nil, false
	}
	if osroot.MkdirAllNoSymlink(root, secretCacheDir, 0o700) != nil {
		return nil, false
	}
	return root, true
}

// lockSecretCache takes the per-subject lock that serialises the exchange,
// the cache write and ForgetServiceAccountSecretToken across processes.
func lockSecretCache(ctx context.Context, root *os.Root, subj exchangeSubject) (func(), error) {
	release, err := flock.AcquireContextIn(ctx, root, secretCacheFile(subj)+".lock")
	if err != nil {
		return nil, fmt.Errorf("lock service account token cache: %w", err)
	}
	return release, nil
}

type cachedSecretToken struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// readCachedSecretToken returns a cached token that is still fresh and still
// passes the checks a fresh one does. Every failure is a miss: the caller
// exchanges again.
func readCachedSecretToken(subj exchangeSubject) (string, bool) {
	root, err := userdirs.CacheRoot()
	if err != nil {
		return "", false
	}
	data, err := osroot.ReadFileNoFollow(root, secretCacheFile(subj))
	if err != nil {
		return "", false
	}
	var c cachedSecretToken
	if json.Unmarshal(data, &c) != nil || c.Token == "" {
		return "", false
	}
	now := secretNow()
	exp, err := checkExchangedToken(subj, c.Token, now)
	if err != nil {
		return "", false
	}
	if exp.Before(c.Expires) {
		c.Expires = exp
	}
	if !now.Add(secretTokenMargin).Before(c.Expires) {
		return "", false
	}
	return c.Token, true
}

// writeCachedSecretToken stores a token for later commands. A failed write
// only costs the next command an exchange.
func writeCachedSecretToken(root *os.Root, subj exchangeSubject, token string, expires time.Time) {
	now := secretNow()
	if !now.Add(secretTokenMargin).Before(expires) {
		return
	}
	if limit := now.Add(secretCacheMaxAge + secretTokenMargin); expires.After(limit) {
		expires = limit
	}
	data, err := json.Marshal(cachedSecretToken{Token: token, Expires: expires})
	if err != nil {
		return
	}
	_ = jsonutil.WriteFileAtomicIn(root, secretCacheFile(subj), data, 0o600) //nolint:errcheck // best effort; see doc
}

// ForgetServiceAccountSecretToken drops the cached token for secret, so the
// next ResolveEnvTokenContext exchanges again. Call it when a server rejects
// the exchanged token. It takes the exchange lock, so it cannot race another
// process writing a token back. A failure is ignored: the entry still expires.
func ForgetServiceAccountSecretToken(secret string) {
	forgetSecretToken(secret, "")
}

// forgetSecretToken is ForgetServiceAccountSecretToken. With a non-empty
// rejected token it drops the cache entry only while it still holds that
// token, so a 401 for a token another process already replaced does not
// discard the replacement.
func forgetSecretToken(secret, rejected string) {
	secret = strings.TrimSpace(secret)
	if !IsServiceAccountSecret(secret) {
		return
	}
	subj, err := secretExchangeSubject(secret)
	if err != nil {
		return
	}
	root, ok := secretCacheRoot()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), secretExchangeTimeout)
	defer cancel()
	release, err := lockSecretCache(ctx, root, subj)
	if err != nil {
		return
	}
	defer release()
	if rejected != "" {
		data, err := osroot.ReadFileNoFollow(root, secretCacheFile(subj))
		if err != nil {
			return
		}
		var c cachedSecretToken
		if json.Unmarshal(data, &c) == nil && c.Token != rejected {
			return
		}
	}
	_ = root.Remove(secretCacheFile(subj)) //nolint:errcheck // best effort; see doc
}

// EnvTokenSource returns the per-request bearer source for an ENTIRE_TOKEN
// value holding a service account secret, or nil for any other value, whose
// bearer is fixed. The exchanged token lives minutes, so a client built once
// and used for longer (a reconnecting stream, a long fan-out) must ask the
// source for every request: it answers from the shared cache and exchanges
// again as the token nears expiry. A 401 drops the cached token, so the next
// request exchanges instead of reusing a token the server refused.
func EnvTokenSource(raw string) api.TokenSource {
	secret := strings.TrimSpace(raw)
	if !IsServiceAccountSecret(secret) {
		return nil
	}
	return envSecretSource{secret: secret}
}

type envSecretSource struct{ secret string }

func (s envSecretSource) Token(ctx context.Context) (string, error) {
	return ResolveEnvTokenContext(ctx, s.secret)
}

func (s envSecretSource) Unauthorized(token string) {
	forgetSecretToken(s.secret, token)
}

// EnvTokenCoreURL returns the control-plane core an ENTIRE_TOKEN value
// authenticates against, without exchanging a service account secret: a
// secret names its jurisdiction, and the exchanged token must name the same
// core as its audience (checkExchangedToken). Any other value goes through
// ParseEnvToken. Use it where only the origin is needed.
func EnvTokenCoreURL(raw string) (string, error) {
	secret := strings.TrimSpace(raw)
	if !IsServiceAccountSecret(secret) {
		core, _, err := ParseEnvToken(raw)
		return core, err
	}
	subj, err := secretExchangeSubject(secret)
	if err != nil {
		return "", err
	}
	return subj.core, nil
}
