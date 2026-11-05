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

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// ServiceAccountSecretPrefix starts a service account secret. An ENTIRE_TOKEN
// with this prefix is never sent as a bearer: it is exchanged at a Core for a
// short-lived service account access token, and that token is the bearer.
const ServiceAccountSecretPrefix = "ent_svc_" //nolint:gosec // G101: a prefix, not a credential

// SecretCoreURLVar names the Core that exchanges a service account secret.
// A secret carries no audience, so the CLI cannot derive one from it.
const SecretCoreURLVar = "ENTIRE_CORE_URL" //nolint:gosec // G101: a variable name, not a credential

// defaultSecretCoreURL exchanges a secret when SecretCoreURLVar is unset. Any
// regional Core can verify a secret: secrets live in the global database.
const defaultSecretCoreURL = "https://us.auth.entire.io" //nolint:gosec // G101: a URL, not a credential

const (
	// secretClientID is the public OAuth client the exchange names.
	secretClientID = "entire-cli"
	// secretTokenMargin discards a cached token this long before it expires,
	// so a long command never starts with a token about to lapse.
	secretTokenMargin = 2 * time.Minute
	// secretExchangeTimeout bounds the exchange; ParseEnvToken has no caller
	// context.
	secretExchangeTimeout = 30 * time.Second
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
	accountID, err := serviceAccountFromSecret(secret)
	if err != nil {
		return "", err
	}
	core, err := secretCoreURL()
	if err != nil {
		return "", err
	}
	return strings.Join([]string{"env-sa", core, accountID}, "|"), nil
}

// serviceAccountFromSecret returns the svc_ account a secret names. The
// format is ent_svc_<account ULID>_<secret ULID>_<random>.
func serviceAccountFromSecret(secret string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(secret, ServiceAccountSecretPrefix), "_")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("%s holds a malformed service account secret", EnvTokenVar)
	}
	return "svc_" + parts[0], nil
}

// secretCoreURL returns the Core origin for the exchange. The secret goes to
// it, so it must be a bare https origin.
func secretCoreURL() (string, error) {
	raw := strings.TrimSpace(os.Getenv(SecretCoreURLVar))
	if raw == "" {
		return defaultSecretCoreURL, nil
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

func exchangeServiceAccountSecret(ctx context.Context, secret string) (string, error) {
	accountID, err := serviceAccountFromSecret(secret)
	if err != nil {
		return "", err
	}
	core, err := secretCoreURL()
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
	data, err := json.Marshal(cachedSecretToken{Token: token, Expires: secretNow().Add(ttl)})
	if err != nil {
		return
	}
	_ = jsonutil.WriteFileAtomicIn(root, secretCacheDir+"/"+key+".json", data, 0o600) //nolint:errcheck // best effort; see doc
}
