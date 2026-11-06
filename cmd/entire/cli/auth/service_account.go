package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/entireio/auth-go/sts"
	"github.com/entireio/auth-go/tokens"
	"github.com/oklog/ulid/v2"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// Service-account keys: the non-interactive credential for a machine that has
// no workload OIDC issuer (a dev box, a self-hosted runner). The private key is
// generated on the machine and never leaves it; only its public half is
// registered on the service account. Each use signs a short-lived assertion and
// exchanges it (RFC 8693, same endpoint as workload identity federation) for a
// regular sa-session JWT, which then flows through every ENTIRE_TOKEN path.
//
// PROTOTYPE: the server side does not exist yet. The proposed contract is
// docs/architecture/service-account-keys.md.

// ServiceAccountEnvVar selects the service account whose locally stored key
// authenticates this process. It is the opt-in: a key on disk is never used
// unless this names it, so a machine with both a human login and a key keeps
// acting as the human by default.
const ServiceAccountEnvVar = "ENTIRE_SERVICE_ACCOUNT"

const (
	serviceAccountIDPrefix = "svc_"

	// saSessionTokenType is the requested_token_type the STS issues an
	// sa-session JWT for (entiredb docs/entire-core/workload-identity-federation.md).
	saSessionTokenType = "urn:entire:token-type:sa-session"

	// assertionTTL bounds how long a stolen assertion stays usable. The server
	// is expected to also refuse a reused jti inside that window.
	assertionTTL = 5 * time.Minute

	// saSessionRefreshSkew re-exchanges this long before the session expires so
	// a request started on a cached token does not die mid-flight.
	saSessionRefreshSkew = 2 * time.Minute

	// maxSASessionLifetime caps what the CLI will cache, whatever the server
	// claims: a session that outlives this is treated as malformed.
	maxSASessionLifetime = 24 * time.Hour
)

// CredentialEnvVars lists every variable that selects a non-interactive
// credential. Test harnesses unset all of them so a developer's own
// credential never reaches a test or a spawned binary.
func CredentialEnvVars() []string {
	return []string{EnvTokenVar, ServiceAccountEnvVar}
}

// ParseServiceAccountID validates id as svc_<ULID> and returns its canonical
// spelling. The id becomes part of a file name, so nothing else is accepted.
func ParseServiceAccountID(id string) (string, error) {
	bare, ok := strings.CutPrefix(strings.TrimSpace(id), serviceAccountIDPrefix)
	if !ok {
		return "", fmt.Errorf("service account %q must look like svc_<id>", id)
	}
	parsed, err := ulid.ParseStrict(bare)
	if err != nil {
		return "", fmt.Errorf("service account %q must look like svc_<id>: %w", id, err)
	}
	return serviceAccountIDPrefix + parsed.String(), nil
}

// GenerateServiceAccountKey creates a fresh Ed25519 key for serviceAccount on
// server. It does not persist it; see CreateServiceAccountKey.
func GenerateServiceAccountKey(serviceAccount, server, name string, now time.Time) (*ServiceAccountKey, error) {
	id, err := ParseServiceAccountID(serviceAccount)
	if err != nil {
		return nil, err
	}
	k, err := generateUnboundKey(server, name, now)
	if err != nil {
		return nil, err
	}
	k.serviceAccount = id
	return k, nil
}

// generateUnboundKey creates a key not yet tied to a service account: the
// enrollment flow learns the account only when an admin approves.
func generateUnboundKey(server, name string, now time.Time) (*ServiceAccountKey, error) {
	origin, err := normalizeServiceAccountServer(server)
	if err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	return &ServiceAccountKey{
		keyID:     ed25519Thumbprint(pub),
		server:    origin,
		name:      sanitizeKeyLabel(name),
		createdAt: now.UTC(),
		signer:    priv,
	}, nil
}

// PublicKeyPEM is the PKIX PEM of the key's public half: what gets registered
// on the service account (the same encoding plugin keys use).
func (k *ServiceAccountKey) PublicKeyPEM() (string, error) {
	if err := k.usable(); err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(k.signer.Public())
	if err != nil {
		return "", fmt.Errorf("encode public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// normalizeServiceAccountServer accepts a bare https origin (or loopback http,
// or any http under --insecure-http-auth) and returns scheme://host. It runs
// when a key is created; network calls re-check the scheme on every use.
func normalizeServiceAccountServer(raw string) (string, error) {
	origin, err := parseStoredServer(raw)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(origin, schemeHTTP+"://") && !isLoopbackHTTP(origin) && !insecureHTTPEnabled() {
		return "", fmt.Errorf("login server %q must use https (pass --insecure-http-auth for local development)", origin)
	}
	return origin, nil
}

// parseStoredServer checks only the shape of a stored key's server: a bare
// http(s) origin. Whether plain HTTP may be used is decided per request, so a
// key created under --insecure-http-auth can still be listed, shown and
// deleted without the flag.
func parseStoredServer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP) {
		return "", fmt.Errorf("login server %q must be an absolute https URL such as https://us.auth.entire.io", raw)
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("login server %q must be a bare origin", u.Redacted())
	}
	return u.Scheme + "://" + u.Host, nil
}

// requireKeyTransport refuses a key-signed request over plain HTTP unless it
// targets loopback or the user opted in for this invocation.
func requireKeyTransport(endpoint string) error {
	if strings.HasPrefix(endpoint, schemeHTTPS+"://") || isLoopbackHTTP(endpoint) || insecureHTTPEnabled() {
		return nil
	}
	return fmt.Errorf("refusing to send a key-signed request over plain HTTP to %s (pass --insecure-http-auth for local development)", endpoint)
}

// ServiceAccountAssertionAudience is the aud a key-signed assertion carries:
// the regional STS, exactly the audience federated OIDC tokens are minted for,
// so the server reuses oidcfed.AudienceFromBaseURL and the existing
// cross-region replay defence.
func ServiceAccountAssertionAudience(server string) string {
	return strings.TrimRight(server, "/") + "/api/authz/sts"
}

// SignAssertion builds the compact EdDSA JWS the STS verifies against the
// service account's registered public keys: iss = sub = the service account,
// a short exp and a random jti for replay protection, and the key id in the
// header so the server need not try every registered key.
func (k *ServiceAccountKey) SignAssertion(now time.Time) (string, error) {
	if k.serviceAccount == "" {
		return "", fmt.Errorf("key %s is not bound to a service account yet", k.keyID)
	}
	return k.signAssertion(now, k.serviceAccount, ServiceAccountAssertionAudience(k.server), nil)
}

// signAssertion signs a proof-of-possession JWT for one call. aud names that
// call (the STS, or a key-management endpoint URL) so an assertion captured for
// one cannot be replayed against another. iss/sub is the service account, or
// the key id while the key is not yet bound to one.
//
// extra adds claims; key-management proofs use it to bind the HTTP method and
// the request body (see signKeyProof).
func (k *ServiceAccountKey) signAssertion(now time.Time, subject, aud string, extra map[string]any) (string, error) {
	if err := k.usable(); err != nil {
		return "", err
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", fmt.Errorf("generate assertion id: %w", err)
	}
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": k.keyID})
	if err != nil {
		return "", fmt.Errorf("encode assertion header: %w", err)
	}
	fields := map[string]any{
		"iss": subject,
		"sub": subject,
		"aud": aud,
		"iat": now.Unix(),
		"exp": now.Add(assertionTTL).Unix(),
		"jti": base64.RawURLEncoding.EncodeToString(jti),
	}
	for name, v := range extra {
		fields[name] = v
	}
	claims, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("encode assertion claims: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sig := ed25519.Sign(k.signer, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ed25519Thumbprint is the RFC 7638 JWK thumbprint of pub, which the server can
// recompute from the registered key without storing a separate id.
func ed25519Thumbprint(pub ed25519.PublicKey) string {
	canonical := `{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// serviceAccountSession is the cached sa-session JWT, kept in the per-user
// cache so every short-lived entire / git-remote-entire process does not pay
// an exchange round trip. It is keyed by key id and records the server and
// account it was minted for; a mismatch on any of them is a cache miss.
type serviceAccountSession struct {
	Server         string    `json:"server"`
	ServiceAccount string    `json:"service_account"`
	KeyID          string    `json:"key_id"`
	AccessToken    string    `json:"access_token"`
	ExpiresAt      time.Time `json:"expires_at"`
}

func serviceAccountSessionName(keyID string) string {
	return "service-account-session-" + keyID + ".json"
}

// serviceAccountExchange is the STS call; a var so tests can count calls
// without a server. Production goes through mintServiceAccountSession.
var serviceAccountExchange = mintServiceAccountSession

// ServiceAccountToken returns a valid sa-session JWT for k, from the cache when
// it is still fresh, otherwise by signing an assertion and exchanging it.
// Concurrent processes on the same key share one exchange: the first takes a
// per-key lock and the rest find its result in the cache.
func ServiceAccountToken(ctx context.Context, k *ServiceAccountKey) (string, error) {
	if err := k.usable(); err != nil {
		return "", err
	}
	if k.serviceAccount == "" {
		return "", fmt.Errorf("key %s is not bound to a service account yet", k.keyID)
	}
	if s, ok := readServiceAccountSession(k, nowFunc()); ok {
		return s.AccessToken, nil
	}
	root, err := userdirs.CacheRoot()
	if err != nil {
		// No cache directory: still authenticate, just without sharing.
		return exchangeServiceAccountSession(ctx, k, nil)
	}
	release, err := lockServiceAccountSession(ctx, root, k.keyID)
	if err != nil {
		return "", err
	}
	defer release()
	if s, ok := readServiceAccountSession(k, nowFunc()); ok {
		return s.AccessToken, nil
	}
	return exchangeServiceAccountSession(ctx, k, root)
}

// VerifyServiceAccountKey performs a fresh exchange with k, bypassing the
// cache, to prove the server accepts it before it is made the active key.
//
// The session it mints is not cached: verification must not leave a bearer
// behind (for example one a concurrent delete just removed).
func VerifyServiceAccountKey(ctx context.Context, k *ServiceAccountKey) error {
	_, err := exchangeServiceAccountSession(ctx, k, nil)
	return err
}

func exchangeServiceAccountSession(ctx context.Context, k *ServiceAccountKey, cache *os.Root) (string, error) {
	now := nowFunc()
	session, err := serviceAccountExchange(ctx, k, now)
	if err != nil {
		return "", err
	}
	if err := validateServiceAccountSession(k, &session, now); err != nil {
		return "", fmt.Errorf("service account %s: %s returned an unusable session token: %w", k.serviceAccount, k.server, err)
	}
	if cache != nil {
		// The cache is an optimisation; failing to write it must not fail auth.
		writeServiceAccountSession(cache, session)
	}
	return session.AccessToken, nil
}

// validateServiceAccountSession checks the minted token's unverified claims
// before it is cached and sent anywhere: it must be for this account, aimed at
// this server, and expire within maxSASessionLifetime. The server verifies the
// signature; this only stops a confused or malicious response from being
// cached as this account's identity or sent to another origin. The token's own
// exp wins over the response's expires_in when it is earlier.
func validateServiceAccountSession(k *ServiceAccountKey, s *serviceAccountSession, now time.Time) error {
	claims, err := tokens.ParseClaims(s.AccessToken)
	if err != nil {
		return fmt.Errorf("parse claims: %w", err)
	}
	if claims.Subject != k.serviceAccount {
		return fmt.Errorf("sub is %q, want %s", claims.Subject, k.serviceAccount)
	}
	// Exactly one URL-shaped audience, and it is the key's server: routing
	// downstream (CoreURLFromEnvToken) takes the first URL audience, so a
	// token naming another origin as well could carry this bearer there.
	var urlAuds []string
	for _, aud := range claims.Audience {
		if u, err := url.Parse(aud); err == nil && u.Scheme != "" {
			urlAuds = append(urlAuds, aud)
		}
	}
	if len(urlAuds) != 1 || urlAuds[0] != k.server {
		return fmt.Errorf("aud %v must name exactly one origin, %s", claims.Audience, k.server)
	}
	lineage, err := sessionKeyLineage(s.AccessToken)
	if err != nil {
		return err
	}
	if lineage != k.keyID {
		return fmt.Errorf("%s is %q, want this key %s", saKeyIDClaim, lineage, k.keyID)
	}
	if claims.ExpiresAt.IsZero() || !claims.ExpiresAt.After(now) || claims.ExpiresAt.After(now.Add(maxSASessionLifetime)) {
		return fmt.Errorf("exp %s is not within %s of now", claims.ExpiresAt.Format(time.RFC3339), maxSASessionLifetime)
	}
	if s.ExpiresAt.IsZero() || claims.ExpiresAt.Before(s.ExpiresAt) {
		s.ExpiresAt = claims.ExpiresAt
	}
	return nil
}

// saKeyIDClaim names the key that minted an sa-session (the key's RFC 7638
// thumbprint). The server stamps it on every session and on every token
// derived from one, and rejects them all once that key is revoked.
const saKeyIDClaim = "sa_key_id"

// sessionKeyLineage reads the sa_key_id claim from a JWT's unverified payload
// (tokens.ParseClaims does not expose custom claims).
func sessionKeyLineage(jwt string) (string, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "", errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode payload: %w", err)
	}
	var claims struct {
		KeyID *string `json:"sa_key_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parse payload: %w", err)
	}
	if claims.KeyID == nil {
		return "", fmt.Errorf("no %s claim: the session is not bound to its key, so revoking the key would not end it", saKeyIDClaim)
	}
	return *claims.KeyID, nil
}

func mintServiceAccountSession(ctx context.Context, k *ServiceAccountKey, now time.Time) (serviceAccountSession, error) {
	if err := requireKeyTransport(k.server); err != nil {
		return serviceAccountSession{}, err
	}
	assertion, err := k.SignAssertion(now)
	if err != nil {
		return serviceAccountSession{}, err
	}
	// sts.Client refuses cross-host redirects itself, so the assertion (a
	// bearer for its five minutes) never reaches another origin.
	client, err := sts.New(&sts.Client{
		BaseURL:           k.server,
		Path:              oauthTokenPath,
		AllowInsecureHTTP: isLoopbackHTTP(k.server) || insecureHTTPEnabled(),
		UserAgent:         oauthClientID,
	})
	if err != nil {
		return serviceAccountSession{}, fmt.Errorf("service account %s: %w", k.serviceAccount, err)
	}
	set, err := client.Exchange(ctx, sts.ExchangeRequest{
		SubjectToken:       assertion,
		SubjectTokenType:   sts.SubjectTokenTypeJWT,
		RequestedTokenType: saSessionTokenType,
		Audience:           k.server,
		Resource:           k.serviceAccount,
		ClientID:           oauthClientID,
	})
	if err != nil {
		return serviceAccountSession{}, fmt.Errorf("service account %s: exchange key assertion at %s: %w", k.serviceAccount, k.server, err)
	}
	if set.AccessToken == "" {
		return serviceAccountSession{}, fmt.Errorf("service account %s: %s returned no access token", k.serviceAccount, k.server)
	}
	return serviceAccountSession{
		Server:         k.server,
		ServiceAccount: k.serviceAccount,
		KeyID:          k.keyID,
		AccessToken:    set.AccessToken,
		ExpiresAt:      set.ExpiresAt,
	}, nil
}

// lockServiceAccountSession takes the per-key lock that serialises exchanges
// and cache removal for one key across processes.
func lockServiceAccountSession(ctx context.Context, cache *os.Root, keyID string) (func(), error) {
	release, err := flock.AcquireContextIn(ctx, cache, serviceAccountSessionName(keyID)+".lock")
	if err != nil {
		return nil, fmt.Errorf("lock session cache: %w", err)
	}
	return release, nil
}

// readServiceAccountSession returns the cached session for k if it was minted
// for k's server, account and key, its token still passes the same claim
// checks as a fresh one, and it is not within the refresh skew of expiry.
func readServiceAccountSession(k *ServiceAccountKey, now time.Time) (serviceAccountSession, bool) {
	root, err := userdirs.CacheRoot()
	if err != nil {
		return serviceAccountSession{}, false
	}
	data, err := osroot.ReadFileNoFollow(root, serviceAccountSessionName(k.keyID))
	if err != nil {
		return serviceAccountSession{}, false
	}
	var s serviceAccountSession
	if json.Unmarshal(data, &s) != nil ||
		s.KeyID != k.keyID || s.Server != k.server || s.ServiceAccount != k.serviceAccount ||
		s.AccessToken == "" {
		return serviceAccountSession{}, false
	}
	if err := validateServiceAccountSession(k, &s, now); err != nil || !s.ExpiresAt.After(now.Add(saSessionRefreshSkew)) {
		return serviceAccountSession{}, false
	}
	return s, true
}

func writeServiceAccountSession(root *os.Root, s serviceAccountSession) {
	data, err := json.Marshal(s) //nolint:gosec // G117: this 0600 cache exists to hold the short-lived session token
	if err != nil {
		return
	}
	_ = jsonutil.WriteFileAtomicIn(root, serviceAccountSessionName(s.KeyID), data, 0o600) //nolint:errcheck // best-effort cache
}

// removeServiceAccountSession deletes the cached session of keyID. Unlike the
// cache write, a failure is returned: a revoked key's session left on disk
// would keep authenticating until it expires.
func removeServiceAccountSession(keyID string) error {
	root, err := userdirs.CacheRoot()
	if err != nil {
		return fmt.Errorf("open cache directory: %w", err)
	}
	// Under the exchange lock, so an exchange already in flight cannot write
	// the session back after it was removed.
	release, err := lockServiceAccountSession(context.Background(), root, keyID)
	if err != nil {
		return err
	}
	defer release()
	if err := jsonutil.RemoveDurableIn(root, serviceAccountSessionName(keyID)); err != nil {
		return fmt.Errorf("remove cached session: %w", err)
	}
	return nil
}

// CredentialSource names which credential a process authenticates with, for
// status output and scripts.
type CredentialSource string

const (
	CredentialFromContext        CredentialSource = "context"
	CredentialFromEnvToken       CredentialSource = "env_token"
	CredentialFromServiceAccount CredentialSource = "service_account"
)

// EnvCredential reports, without touching the network, which non-interactive
// credential is configured: ENTIRE_TOKEN first, then ENTIRE_SERVICE_ACCOUNT,
// otherwise CredentialFromContext. Callers use it for wording and provenance,
// never as permission to skip LookupEnvToken's errors.
func EnvCredential() CredentialSource {
	if _, ok := os.LookupEnv(EnvTokenVar); ok {
		return CredentialFromEnvToken
	}
	if _, ok := os.LookupEnv(ServiceAccountEnvVar); ok {
		return CredentialFromServiceAccount
	}
	return CredentialFromContext
}

// EnvCredentialConfigured reports whether ENTIRE_TOKEN or
// ENTIRE_SERVICE_ACCOUNT is set.
func EnvCredentialConfigured() bool {
	return EnvCredential() != CredentialFromContext
}

// EnvCredentialNonEmpty reports whether ENTIRE_TOKEN or ENTIRE_SERVICE_ACCOUNT
// holds a value. Hints and notices use it, keeping their long-standing rule
// that an exported-but-empty variable means "not configured"; resolution
// (LookupEnvToken) still fails closed on a set-but-blank one.
func EnvCredentialNonEmpty() bool {
	return os.Getenv(EnvTokenVar) != "" || os.Getenv(ServiceAccountEnvVar) != ""
}

// EnvCredentialSource names where the non-interactive bearer comes from, for
// status output. Only meaningful when EnvCredentialConfigured is true.
func EnvCredentialSource() string {
	if EnvCredential() == CredentialFromServiceAccount {
		return "service account " + tuiutil.SanitizeDisplayText(strings.TrimSpace(os.Getenv(ServiceAccountEnvVar))) + " (local key, " + ServiceAccountEnvVar + ")"
	}
	return EnvTokenVar + " environment variable"
}

// LookupEnvToken returns the raw non-interactive bearer and whether one is
// configured. ENTIRE_TOKEN wins verbatim; otherwise ENTIRE_SERVICE_ACCOUNT
// selects a stored key whose assertion is exchanged for an sa-session JWT.
//
// ok=true commits the caller to the env path exactly as a set ENTIRE_TOKEN
// always has: any error is fatal, never a fall-back to a saved human login,
// which would silently act as the wrong identity. The returned value goes
// through ParseEnvToken like any ENTIRE_TOKEN.
func LookupEnvToken(ctx context.Context) (string, bool, error) {
	if raw, ok := os.LookupEnv(EnvTokenVar); ok {
		return raw, true, nil
	}
	target, ok, err := ServiceAccountTarget()
	if !ok || err != nil {
		return "", ok, err
	}
	// Callers treat the value like ENTIRE_TOKEN and route by its aud, which
	// must be https (CoreURLFromEnvToken refuses plain HTTP even with the
	// insecure opt-in). Say so up front rather than mint a session whose aud
	// fails later with a less helpful message.
	if !strings.HasPrefix(target.CoreURL, schemeHTTPS+"://") {
		return "", true, fmt.Errorf("%s: the key's login server %s is not https; plain-HTTP servers work only for key management and coreapi clients, not for the ENTIRE_TOKEN-compatible paths (auth status/token, data API, cells, git)", ServiceAccountEnvVar, target.CoreURL)
	}
	token, err := target.TokenSource(ctx)
	return token, true, err
}

// ServiceAccountTarget is the control-plane target for ENTIRE_SERVICE_ACCOUNT:
// the active key's login server, and a bearer minted lazily on first use (so
// commands that never authenticate never exchange). ok=false when the variable
// is unset or ENTIRE_TOKEN, which takes precedence, is set. Loading the key
// does not touch the network.
func ServiceAccountTarget() (ControlPlaneTarget, bool, error) {
	if EnvCredential() != CredentialFromServiceAccount {
		return ControlPlaneTarget{}, false, nil
	}
	id := os.Getenv(ServiceAccountEnvVar)
	if strings.TrimSpace(id) == "" {
		return ControlPlaneTarget{}, true, fmt.Errorf("%s is set but blank", ServiceAccountEnvVar)
	}
	k, err := LoadActiveServiceAccountKey(id)
	if err != nil {
		return ControlPlaneTarget{}, true, fmt.Errorf("%s: %w", ServiceAccountEnvVar, err)
	}
	return ControlPlaneTarget{
		CoreURL: k.server,
		TokenSource: func(ctx context.Context) (string, error) {
			token, err := ServiceAccountToken(ctx, k)
			if err != nil {
				return "", fmt.Errorf("%s: %w", ServiceAccountEnvVar, err)
			}
			return token, nil
		},
	}, true, nil
}
