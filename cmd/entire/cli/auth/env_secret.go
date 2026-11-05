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

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// ServiceAccountSecretPrefix starts a service account secret. An ENTIRE_TOKEN
// with this prefix is never sent as a bearer: it is exchanged at a Core for a
// short-lived service account access token, and that token is the bearer.
const ServiceAccountSecretPrefix = "ent_svc_" //nolint:gosec // G101: a prefix, not a credential

// SecretCoreURLVar overrides the Core that exchanges a service account
// secret, for local development and staging. Unset, the Core is derived
// from the jurisdiction the secret names.
const SecretCoreURLVar = "ENTIRE_CORE_URL" //nolint:gosec // G101: a variable name, not a credential

const (
	// secretClientID is the public OAuth client the exchange names.
	secretClientID = "entire-cli"
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
	core, accountID, err := secretExchangeTarget(secret)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{"env-sa", core, accountID}, "|"), nil
}

// secretExchangeTarget parses secret and returns the Core that exchanges it
// and the svc_ account it names. The exchange, the cache keys and
// ForgetServiceAccountSecretToken all derive the Core here, so they agree.
func secretExchangeTarget(secret string) (core, accountID string, err error) {
	parsed, err := parseServiceAccountSecret(secret)
	if err != nil {
		return "", "", err
	}
	core, err = secretCoreURL(parsed.jurisdiction)
	if err != nil {
		return "", "", err
	}
	return core, parsed.accountID, nil
}

// secretCoreURL returns the Core origin for the exchange: SecretCoreURLVar
// when set, otherwise the regional Core of jurisdiction, which holds the
// secret. The secret goes to it, so an override must be a bare https origin.
// An override is not checked against jurisdiction: Core refuses a secret from
// another jurisdiction with an error that names the right Core.
func secretCoreURL(jurisdiction string) (string, error) {
	raw := strings.TrimSpace(os.Getenv(SecretCoreURLVar))
	if raw == "" {
		return jurisdictionCoreURL(jurisdiction), nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("%s %q is not a Core URL", SecretCoreURLVar, raw)
	}
	if u.Scheme != httpclient.SchemeHTTPS && !insecureHTTPEnabled() {
		return "", fmt.Errorf("%s %q must use https; the service account secret is sent to it", SecretCoreURLVar, u.Redacted())
	}
	return u.Scheme + "://" + u.Host, nil
}

// jurisdictionCoreURL returns https://<jurisdiction>.<default login host>,
// for example https://eu.auth.entire.io.
func jurisdictionCoreURL(jurisdiction string) string {
	return httpclient.SchemeHTTPS + "://" + jurisdiction + "." + strings.TrimPrefix(api.DefaultAuthBaseURL, httpclient.SchemeHTTPS+"://")
}

func exchangeServiceAccountSecret(ctx context.Context, secret string) (string, error) {
	core, accountID, err := secretExchangeTarget(secret)
	if err != nil {
		return "", err
	}
	// The key covers the Core too: a token from one Core is not reused
	// after the variable points at another.
	key := secretCacheKey(core, secret)
	if token, ok := readCachedSecretToken(key); ok {
		return token, nil
	}
	token, ttl, err := postSecretExchange(ctx, core, accountID, secret)
	if err != nil {
		return "", err
	}
	writeCachedSecretToken(key, token, ttl)
	return token, nil
}

func postSecretExchange(ctx context.Context, core, accountID, secret string) (string, time.Duration, error) {
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"subject_token":      {secret},
		"audience":           {core},
		"resource":           {accountID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, core+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("build %s exchange: %w", EnvTokenVar, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(secretClientID, "")
	client := httpclient.New(httpclient.Options{Transport: secretExchangeTransport, Timeout: secretExchangeTimeout})
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("exchange %s service account secret at %s: %w", EnvTokenVar, core, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxExchangeBody))
	if err != nil {
		return "", 0, fmt.Errorf("read %s exchange response: %w", EnvTokenVar, err)
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
			return "", 0, fmt.Errorf("%s service account secret refused by %s: %s: %s", EnvTokenVar, core, out.Error, out.ErrorDescription)
		}
		return "", 0, fmt.Errorf("%s service account secret refused by %s: HTTP %d", EnvTokenVar, core, resp.StatusCode)
	}
	if out.AccessToken == "" {
		return "", 0, errors.New("service account token exchange returned no access token")
	}
	return out.AccessToken, time.Duration(out.ExpiresIn) * time.Second, nil
}

// secretCacheKey names a cache file without storing the secret.
func secretCacheKey(core, secret string) string {
	sum := sha256.Sum256([]byte(core + "\x00" + secret))
	return hex.EncodeToString(sum[:16])
}

type cachedSecretToken struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// readCachedSecretToken returns a cached token that is still fresh. Every
// failure is a miss: the caller exchanges again.
func readCachedSecretToken(key string) (string, bool) {
	root, err := userdirs.CacheRoot()
	if err != nil {
		return "", false
	}
	data, err := osroot.ReadFileNoFollow(root, secretCacheDir+"/"+key+".json")
	if err != nil {
		return "", false
	}
	var c cachedSecretToken
	if json.Unmarshal(data, &c) != nil || c.Token == "" {
		return "", false
	}
	if !secretNow().Add(secretTokenMargin).Before(c.Expires) {
		return "", false
	}
	return c.Token, true
}

// writeCachedSecretToken stores a token for later commands. A failed write
// only costs the next command an exchange.
func writeCachedSecretToken(key, token string, ttl time.Duration) {
	if ttl <= secretTokenMargin {
		return
	}
	root, err := userdirs.CacheRoot()
	if err != nil {
		return
	}
	if osroot.MkdirAllNoSymlink(root, secretCacheDir, 0o700) != nil {
		return
	}
	if ttl > secretCacheMaxAge+secretTokenMargin {
		ttl = secretCacheMaxAge + secretTokenMargin
	}
	data, err := json.Marshal(cachedSecretToken{Token: token, Expires: secretNow().Add(ttl)})
	if err != nil {
		return
	}
	_ = jsonutil.WriteFileAtomicIn(root, secretCacheDir+"/"+key+".json", data, 0o600) //nolint:errcheck // best effort; see doc
}

// ForgetServiceAccountSecretToken drops the cached token for secret, so the
// next ResolveEnvTokenContext exchanges again. Call it when a server rejects
// the exchanged token. A failure is ignored: the entry still expires.
func ForgetServiceAccountSecretToken(secret string) {
	secret = strings.TrimSpace(secret)
	if !IsServiceAccountSecret(secret) {
		return
	}
	core, _, err := secretExchangeTarget(secret)
	if err != nil {
		return
	}
	root, err := userdirs.CacheRoot()
	if err != nil {
		return
	}
	_ = root.Remove(secretCacheDir + "/" + secretCacheKey(core, secret) + ".json") //nolint:errcheck // best effort; see doc
}
