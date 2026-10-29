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
	"io/fs"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/entireio/auth-go/sts"
	"github.com/oklog/ulid/v2"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// Service-account keys: the non-interactive credential for a machine that has
// no workload OIDC issuer (a dev box, a self-hosted runner). The private key is
// generated on the machine and never leaves it; only its public half is
// registered on the service account. Each use signs a short-lived assertion and
// exchanges it (RFC 8693, same endpoint as workload identity federation) for a
// regular sa-session JWT, which then flows through every ENTIRE_TOKEN path.
//
// PROTOTYPE: the server side (key registration on the service account and the
// assertion branch of the STS) does not exist yet. The wire format here is the
// proposal; see ServiceAccountAssertionAudience and MintServiceAccountToken.

// ServiceAccountEnvVar selects the service account whose locally stored key
// authenticates this process. It is the opt-in: a key on disk is never used
// unless this names it, so a machine with both a human login and a key keeps
// acting as the human by default.
const ServiceAccountEnvVar = "ENTIRE_SERVICE_ACCOUNT"

const (
	serviceAccountIDPrefix = "svc_"
	serviceAccountKeyType  = "entire_service_account_key"

	// saSessionTokenType is the requested_token_type the STS issues an
	// sa-session JWT for (entiredb docs/entire-core/workload-identity-federation.md).
	saSessionTokenType = "urn:entire:token-type:sa-session"

	// assertionTTL bounds how long a stolen assertion stays usable. The server
	// is expected to also refuse a reused jti inside that window.
	assertionTTL = 5 * time.Minute

	// saSessionRefreshSkew re-exchanges this long before the session expires so
	// a request started on a cached token does not die mid-flight.
	saSessionRefreshSkew = 2 * time.Minute
)

// ServiceAccountKey is the on-disk key file, stored 0600 in the per-user
// config directory as service-account-<id>.json.
type ServiceAccountKey struct {
	Type           string `json:"type"`
	ServiceAccount string `json:"service_account"`
	// Server is the regional login server that owns the service account
	// (service accounts are regional), e.g. https://us.auth.entire.io.
	Server     string    `json:"server"`
	KeyID      string    `json:"key_id"`
	PrivateKey string    `json:"private_key"`
	CreatedAt  time.Time `json:"created_at"`

	signer ed25519.PrivateKey
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
// server. It does not persist it; see SaveServiceAccountKey.
func GenerateServiceAccountKey(serviceAccount, server string, now time.Time) (*ServiceAccountKey, error) {
	id, err := ParseServiceAccountID(serviceAccount)
	if err != nil {
		return nil, err
	}
	k, err := generateUnboundKey(server, now)
	if err != nil {
		return nil, err
	}
	k.ServiceAccount = id
	return k, nil
}

// generateUnboundKey creates a key not yet tied to a service account: the
// enrollment flow learns the account only when an admin approves.
func generateUnboundKey(server string, now time.Time) (*ServiceAccountKey, error) {
	origin, err := normalizeServiceAccountServer(server)
	if err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("encode private key: %w", err)
	}
	return &ServiceAccountKey{
		Type:       serviceAccountKeyType,
		Server:     origin,
		KeyID:      ed25519Thumbprint(pub),
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		CreatedAt:  now.UTC(),
		signer:     priv,
	}, nil
}

// PublicKeyPEM is the PKIX PEM of the key's public half: what gets registered
// on the service account (the same encoding plugin keys use).
func (k *ServiceAccountKey) PublicKeyPEM() (string, error) {
	der, err := x509.MarshalPKIXPublicKey(k.signer.Public())
	if err != nil {
		return "", fmt.Errorf("encode public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

func serviceAccountKeyFileName(id string) string {
	return "service-account-" + id + ".json"
}

func serviceAccountTokenCacheName(id string) string {
	return "service-account-" + id + ".session.json"
}

// ErrServiceAccountKeyExists is returned by SaveServiceAccountKey when a key
// for that service account is already stored and overwrite was not requested.
var ErrServiceAccountKeyExists = errors.New("a key for this service account already exists")

// SaveServiceAccountKey writes k to the per-user config directory, 0600.
// Replacing an existing key needs overwrite: the old key may still be the one
// registered on the server.
func SaveServiceAccountKey(k *ServiceAccountKey, overwrite bool) (string, error) {
	root, err := userdirs.ConfigRoot()
	if err != nil {
		return "", fmt.Errorf("open config directory: %w", err)
	}
	name := serviceAccountKeyFileName(k.ServiceAccount)
	if !overwrite {
		if _, err := root.Lstat(name); err == nil {
			return "", fmt.Errorf("%w (%s); pass --force to replace it", ErrServiceAccountKeyExists, name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("check existing key: %w", err)
		}
	}
	data, err := jsonutil.MarshalIndentWithNewline(k, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode key file: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, name, data, 0o600); err != nil {
		return "", fmt.Errorf("write key file: %w", err)
	}
	return name, nil
}

// LoadServiceAccountKey reads the stored key for serviceAccount. A missing key
// is reported with the command that creates one.
func LoadServiceAccountKey(serviceAccount string) (*ServiceAccountKey, error) {
	id, err := ParseServiceAccountID(serviceAccount)
	if err != nil {
		return nil, err
	}
	root, err := userdirs.ConfigRootForRead()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("open config directory: %w", err)
	}
	var data []byte
	if err == nil {
		data, err = osroot.ReadFileNoFollow(root, serviceAccountKeyFileName(id))
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no key stored for service account %s; create one with `entire auth service-account key create --service-account %s --server <login-server>`", id, id)
	}
	if err != nil {
		return nil, fmt.Errorf("read key for service account %s: %w", id, err)
	}
	var k ServiceAccountKey
	if err := json.Unmarshal(data, &k); err != nil {
		return nil, fmt.Errorf("parse key for service account %s: %w", id, err)
	}
	if k.Type != serviceAccountKeyType || k.ServiceAccount != id {
		return nil, fmt.Errorf("key file for service account %s is not a key for that service account", id)
	}
	if k.Server, err = normalizeServiceAccountServer(k.Server); err != nil {
		return nil, fmt.Errorf("key for service account %s: %w", id, err)
	}
	block, _ := pem.Decode([]byte(k.PrivateKey))
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("key for service account %s has no PEM private key", id)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key for service account %s: %w", id, err)
	}
	signer, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key for service account %s is not an Ed25519 key", id)
	}
	pub, ok := signer.Public().(ed25519.PublicKey)
	if !ok || ed25519Thumbprint(pub) != k.KeyID {
		return nil, fmt.Errorf("key for service account %s does not match its key_id", id)
	}
	k.signer = signer
	return &k, nil
}

// normalizeServiceAccountServer accepts a bare https origin (or loopback http,
// or any http under --insecure-http-auth) and returns scheme://host.
func normalizeServiceAccountServer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("login server %q must be an absolute URL such as https://us.auth.entire.io", raw)
	}
	switch {
	case u.Scheme == schemeHTTPS:
	case u.Scheme == schemeHTTP && (isLoopbackHTTP(u.String()) || insecureHTTPEnabled()):
	default:
		return "", fmt.Errorf("login server %q must use https", u.Redacted())
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("login server %q must be a bare origin", u.Redacted())
	}
	return u.Scheme + "://" + u.Host, nil
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
	return k.signAssertion(now, k.ServiceAccount, ServiceAccountAssertionAudience(k.Server))
}

// signAssertion signs a proof-of-possession JWT for one call. aud names that
// call (the STS, or a key-management endpoint URL) so an assertion captured for
// one cannot be replayed against another. iss/sub is the service account, or
// the key id while the key is not yet bound to one.
func (k *ServiceAccountKey) signAssertion(now time.Time, subject, aud string) (string, error) {
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", fmt.Errorf("generate assertion id: %w", err)
	}
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": k.KeyID})
	if err != nil {
		return "", fmt.Errorf("encode assertion header: %w", err)
	}
	claims, err := json.Marshal(map[string]any{
		"iss": subject,
		"sub": subject,
		"aud": aud,
		"iat": now.Unix(),
		"exp": now.Add(assertionTTL).Unix(),
		"jti": base64.RawURLEncoding.EncodeToString(jti),
	})
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
// an exchange round trip.
type serviceAccountSession struct {
	KeyID       string    `json:"key_id"`
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// serviceAccountExchange is the STS call; a var so tests can count calls
// without a server. Production goes through mintServiceAccountSession.
var serviceAccountExchange = mintServiceAccountSession

// ServiceAccountToken returns a valid sa-session JWT for k, from the cache when
// it is still fresh, otherwise by signing an assertion and exchanging it.
func ServiceAccountToken(ctx context.Context, k *ServiceAccountKey) (string, error) {
	now := nowFunc()
	if cached, ok := readServiceAccountSession(k); ok && cached.ExpiresAt.After(now.Add(saSessionRefreshSkew)) {
		return cached.AccessToken, nil
	}
	session, err := serviceAccountExchange(ctx, k, now)
	if err != nil {
		return "", err
	}
	// The cache is an optimisation; failing to write it must not fail auth.
	writeServiceAccountSession(k, session)
	return session.AccessToken, nil
}

func mintServiceAccountSession(ctx context.Context, k *ServiceAccountKey, now time.Time) (serviceAccountSession, error) {
	assertion, err := k.SignAssertion(now)
	if err != nil {
		return serviceAccountSession{}, err
	}
	client, err := sts.New(&sts.Client{
		BaseURL:           k.Server,
		Path:              oauthTokenPath,
		AllowInsecureHTTP: isLoopbackHTTP(k.Server) || insecureHTTPEnabled(),
		UserAgent:         oauthClientID,
	})
	if err != nil {
		return serviceAccountSession{}, fmt.Errorf("service account %s: %w", k.ServiceAccount, err)
	}
	set, err := client.Exchange(ctx, sts.ExchangeRequest{
		SubjectToken:       assertion,
		SubjectTokenType:   sts.SubjectTokenTypeJWT,
		RequestedTokenType: saSessionTokenType,
		Audience:           k.Server,
		Resource:           k.ServiceAccount,
		ClientID:           oauthClientID,
	})
	if err != nil {
		return serviceAccountSession{}, fmt.Errorf("service account %s: exchange key assertion at %s: %w", k.ServiceAccount, k.Server, err)
	}
	if set.AccessToken == "" {
		return serviceAccountSession{}, fmt.Errorf("service account %s: %s returned no access token", k.ServiceAccount, k.Server)
	}
	return serviceAccountSession{KeyID: k.KeyID, AccessToken: set.AccessToken, ExpiresAt: set.ExpiresAt}, nil
}

func readServiceAccountSession(k *ServiceAccountKey) (serviceAccountSession, bool) {
	root, err := userdirs.CacheRoot()
	if err != nil {
		return serviceAccountSession{}, false
	}
	data, err := osroot.ReadFileNoFollow(root, serviceAccountTokenCacheName(k.ServiceAccount))
	if err != nil {
		return serviceAccountSession{}, false
	}
	var s serviceAccountSession
	if json.Unmarshal(data, &s) != nil || s.KeyID != k.KeyID || s.AccessToken == "" {
		return serviceAccountSession{}, false
	}
	return s, true
}

func writeServiceAccountSession(k *ServiceAccountKey, s serviceAccountSession) {
	if s.ExpiresAt.IsZero() {
		return
	}
	root, err := userdirs.CacheRoot()
	if err != nil {
		return
	}
	data, err := json.Marshal(s) //nolint:gosec // G117: this 0600 cache exists to hold the short-lived session token
	if err != nil {
		return
	}
	_ = jsonutil.WriteFileAtomicIn(root, serviceAccountTokenCacheName(k.ServiceAccount), data, 0o600) //nolint:errcheck // best-effort cache
}

// EnvCredentialConfigured reports, without touching the network, whether this
// process authenticates non-interactively: ENTIRE_TOKEN or ENTIRE_SERVICE_ACCOUNT
// is set. Callers use it for wording ("context provided by …"), never as
// permission to skip LookupEnvToken's errors.
func EnvCredentialConfigured() bool {
	if _, ok := os.LookupEnv(EnvTokenVar); ok {
		return true
	}
	_, ok := os.LookupEnv(ServiceAccountEnvVar)
	return ok
}

// EnvCredentialSource names where the non-interactive bearer comes from, for
// status output. Only meaningful when EnvCredentialConfigured is true.
func EnvCredentialSource() string {
	if _, ok := os.LookupEnv(EnvTokenVar); !ok {
		if id := strings.TrimSpace(os.Getenv(ServiceAccountEnvVar)); id != "" {
			return "service account " + id + " (local key, " + ServiceAccountEnvVar + ")"
		}
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
	token, err := target.TokenSource(ctx)
	return token, true, err
}

// ServiceAccountTarget is the control-plane target for ENTIRE_SERVICE_ACCOUNT:
// the key's login server, and a bearer minted lazily on first use (so commands
// that never authenticate never exchange). ok=false when the variable is unset
// or ENTIRE_TOKEN, which takes precedence, is set. Loading the key does not
// touch the network.
func ServiceAccountTarget() (ControlPlaneTarget, bool, error) {
	if _, ok := os.LookupEnv(EnvTokenVar); ok {
		return ControlPlaneTarget{}, false, nil
	}
	id, ok := os.LookupEnv(ServiceAccountEnvVar)
	if !ok {
		return ControlPlaneTarget{}, false, nil
	}
	if strings.TrimSpace(id) == "" {
		return ControlPlaneTarget{}, true, fmt.Errorf("%s is set but blank", ServiceAccountEnvVar)
	}
	k, err := LoadServiceAccountKey(id)
	if err != nil {
		return ControlPlaneTarget{}, true, fmt.Errorf("%s: %w", ServiceAccountEnvVar, err)
	}
	return ControlPlaneTarget{
		CoreURL: k.Server,
		TokenSource: func(ctx context.Context) (string, error) {
			token, err := ServiceAccountToken(ctx, k)
			if err != nil {
				return "", fmt.Errorf("%s: %w", ServiceAccountEnvVar, err)
			}
			return token, nil
		},
	}, true, nil
}
