// Command git-remote-entire is the git remote helper for entire:// URLs.
//
// Git resolves `git clone entire://host/project/repo` by exec'ing a binary
// named git-remote-entire on PATH, handing it the remote-helper protocol on
// stdin and reading responses from stdout. This is a small, dedicated
// binary (no cobra command tree) that shares the protocol, transport, and
// auth packages with the main entire CLI.
//
// IMPORTANT: nothing here may write to stdout except the helper protocol
// itself — git parses stdout as a strict pkt-line stream, so a stray banner
// or log line corrupts the transfer. Diagnostics go to stderr (and the
// ENTIRE_DEBUG-gated debuglog).
//
// Authentication acts as the active login context from the shared
// contexts.json (or ENTIRE_TOKEN in CI), and confirms the target cluster is one
// that identity's control-plane core actually fronts by consulting the core's
// cluster registry (GET /api/v1/clusters) — the same authoritative source the
// cell-routed commands resolve against. It then uses that context's login JWT
// (or the env token) directly as the git-transport bearer.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/entireclient/httputil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"github.com/entireio/cli/internal/remotehelper"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
	"github.com/entireio/cli/internal/remotehelper/githelper"
	"github.com/entireio/cli/internal/remotehelper/httpdebug"
	"github.com/entireio/cli/internal/remotehelper/replicas"
	"github.com/entireio/cli/internal/remotehelper/transport"
)

func main() {
	os.Exit(run(os.Args))
}

func run(args []string) int {
	// --version / --help only activate as the sole argument (so os.Args has
	// length 2). Git always invokes the helper as
	// `git-remote-entire <remote-name> <url>` (os.Args length 3), so these can
	// never collide with a real remote-helper invocation.
	if len(args) == 2 {
		if text, ok := infoFlagText(args[1], loadedVersion()); ok {
			fmt.Fprint(os.Stdout, text)
			return 0
		}
	}

	if len(args) < 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <remote-name> <url>\n", remotehelper.BinaryName)
		return 128
	}

	// Build info drives the identifier the helper advertises upstream.
	// One string covers both surfaces:
	//   - githelper.Agent rides in the git protocol pkt-line agent=
	//     capability appended to upload-pack / receive-pack / v2 requests.
	//   - httpUserAgent rides in the HTTP User-Agent header on every
	//     outbound request so server access logs can attribute traffic.
	// Using the same value keeps the two log surfaces correlatable.
	versioninfo.Load()
	helperAgent := remotehelper.BinaryName + "/" + versioninfo.Version
	githelper.Agent = helperAgent
	httpUserAgent := helperAgent

	rawURL := args[2]
	parsedURL, err := url.Parse(rawURL)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "fatal: invalid URL %q: %v\n", rawURL, err)
		return 128
	case parsedURL.Scheme != "entire":
		fmt.Fprintf(os.Stderr, "fatal: unsupported URL scheme %q (expected 'entire')\n", parsedURL.Scheme)
		return 128
	case parsedURL.Host == "" || gitremote.IsSupportedForge(parsedURL.Host):
		// Cluster host absent (empty, or a forge id in its slot);
		// missingClusterHostMessage renders the actionable hint.
		fmt.Fprint(os.Stderr, missingClusterHostMessage(parsedURL, rawURL))
		return 128
	}

	ctx, stop := installSignals()
	defer stop()

	skipTLS := os.Getenv("ENTIRE_TLS_SKIP_VERIFY") == "true"

	nodeCfg := replicas.Resolve(parsedURL)

	// This client drives the auth path only: cluster /.well-known discovery
	// and the token exchange. Both talk to a single control-plane host with no
	// failover to fall back on, so they get the patient discovery dial budget
	// (DiscoveryDialTimeout, i.e. DefaultDiscoveryDialTimeout unless
	// ENTIRE_CONNECT_TIMEOUT_SECONDS overrides it) rather than the short failover
	// one — a slow cold connect here would otherwise fail the whole clone/fetch.
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &httpclient.UserAgentTransport{
			Next: &httpdebug.TimingRoundTripper{
				Next:  httpclient.NewDiscoveryTransport(skipTLS),
				Label: "auth",
			},
			UA: httpUserAgent,
		},
	}

	creds, onUnauthorized, err := resolveCreds(ctx, parsedURL, skipTLS, httpClient, userdirs.Cache(), defaultClusterRegistry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		return 128
	}

	setAuth := setAuthWithProvider(creds)

	var onNodeFailed func(string)
	if nodeCfg.Caching() {
		onNodeFailed = func(string) { replicas.Invalidate(nodeCfg.ClusterHost, nodeCfg.RepoPath) }
	}

	proxy := transport.New(transport.Config{
		Nodes:          nodeCfg,
		Path:           parsedURL.Path,
		SkipTLS:        skipTLS,
		SetAuth:        setAuth,
		OnUnauthorized: onUnauthorized,
		OnNodeFailed:   onNodeFailed,
		UserAgent:      httpUserAgent,
	})

	protocolVersion := resolveProtocolVersion()
	debuglog.Printf("git protocol.version=%d (v2 advertises stateless-connect + push; v0/v1 advertises connect)", protocolVersion)

	helperStart := time.Now()
	if err := githelper.Run(ctx, proxy, protocolVersion, os.Stdin, os.Stdout); err != nil {
		fmt.Fprint(os.Stderr, fatalMessage(err, parsedURL))
		return 128
	}
	debuglog.Printf("timing: helper-session dur_ms=%d", time.Since(helperStart).Milliseconds())
	return 0
}

type credentialProvider func(context.Context) (string, error)

type refreshableCredential interface {
	Token(ctx context.Context) (string, error)
	ForceRefresh(ctx context.Context, staleToken string) (string, error)
}

// refreshingProvider defers reactive refresh work until the transport rebuilds
// a request after a 401, so the network call uses that request's context. The
// observer itself only marks the last bearer stale.
func refreshingProvider(credential refreshableCredential) (credentialProvider, func()) {
	var mu sync.Mutex
	var lastToken, rejectedToken string

	provider := func(ctx context.Context) (string, error) {
		mu.Lock()
		stale := rejectedToken
		rejectedToken = ""
		mu.Unlock()

		var token string
		var err error
		if stale != "" {
			token, err = credential.ForceRefresh(ctx, stale)
		} else {
			token, err = credential.Token(ctx)
		}
		if err != nil {
			if stale != "" {
				mu.Lock()
				if rejectedToken == "" {
					rejectedToken = stale
				}
				mu.Unlock()
			}
			return "", fmt.Errorf("resolve login credential: %w", err)
		}

		mu.Lock()
		lastToken = token
		mu.Unlock()
		return token, nil
	}

	onUnauthorized := func() {
		mu.Lock()
		rejectedToken = lastToken
		mu.Unlock()
		debuglog.Printf("data plane rejected login bearer; marked it stale for the transport's retry")
	}
	return provider, onUnauthorized
}

func setAuthWithProvider(provider credentialProvider) transport.SetAuthFunc {
	return func(req *http.Request) error {
		// Refuse to attach credentials to a request we can't classify as a
		// known git smart-HTTP endpoint. Sending a bearer to an unexpected
		// endpoint is never right.
		if gitActionFromRequest(req) == "" {
			return fmt.Errorf("refusing to attach credentials: %s %s is not a recognised git smart-HTTP endpoint", req.Method, req.URL.Path)
		}
		token, err := provider(req.Context())
		if err != nil {
			return fmt.Errorf("resolve git credential: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
}

// wrongClusterRe extracts the host that actually serves the repo from the
// data plane's `invalid_target` error_description (RFC 8693). The data plane
// emits this when the audience host doesn't host the repo but a sibling
// cluster does, naming the correct host so we can point the user at it. The
// phrasing is "… it lives on \"<host>\" …"; anchoring on "lives on" keeps the
// match tied to this specific, actionable case rather than other
// invalid_target variants (e.g. a suspended mirror).
var wrongClusterRe = regexp.MustCompile(`lives on "([^"]+)"`)

// fatalMessage renders the stderr "fatal: …" line for a transfer error. When
// the failure is the data plane reporting that the repo lives on a different
// cluster, it special-cases the raw OAuth chain into an actionable message
// naming the correct host (and the corrected entire:// URL). Everything else
// falls back to the verbatim error.
func fatalMessage(err error, parsedURL *url.URL) string {
	var oe *httputil.OAuthError
	if errors.As(err, &oe) && oe.Code == "invalid_target" {
		if m := wrongClusterRe.FindStringSubmatch(oe.Description); m != nil {
			host := m[1]
			// Copy the URL the user typed and swap only the host, so any
			// escaped path (RawPath) or query stays byte-identical to what
			// they originally ran.
			correctedURL := *parsedURL
			correctedURL.Scheme = "entire"
			correctedURL.Host = host
			correctedURL.User = nil
			corrected := correctedURL.String()
			return fmt.Sprintf("fatal: this repository is not hosted on %s; it lives on %s.\n"+
				"Re-run against the correct host, e.g.:\n\n    git clone %s\n",
				parsedURL.Host, host, corrected)
		}
	}
	return fmt.Sprintf("fatal: %v\n", err)
}

// missingClusterHostMessage renders the stderr "fatal: …" line for an entire://
// URL that omits its cluster host. Two shapes reach here: a forge id typed
// where the host belongs (entire://gh/owner/repo, Host="gh") and an empty host
// (entire:///gh/owner/repo, Host=""). When the reconstructed shorthand is a
// complete forge/owner/repo triple that `entire repo clone` can resolve, it
// points at the interactive picker; a partial path (entire://gh,
// entire://gh/owner) or a non-forge segment falls back to the plain
// missing-host error rather than suggesting a clone command that would reject
// the ref. Kept pure so it's unit-testable.
func missingClusterHostMessage(parsedURL *url.URL, rawURL string) string {
	// Reconstruct the forge/owner/repo shorthand the user likely intended: a
	// forge id in the host slot sits in front of the path; an empty host
	// already has it there.
	shorthand := strings.TrimPrefix(parsedURL.Path, "/")
	if parsedURL.Host != "" {
		shorthand = parsedURL.Host + "/" + shorthand
	}
	// Only point at `entire repo clone` for a complete forge/owner/repo triple
	// (the shape parseMirrorCloneRef accepts); anything shorter would relocate
	// the failure into a clone command that rejects the ref.
	seg := strings.Split(strings.Trim(shorthand, "/"), "/")
	if len(seg) != 3 || seg[0] == "" || seg[1] == "" || seg[2] == "" || !gitremote.IsSupportedForge(seg[0]) {
		return fmt.Sprintf("fatal: missing host in URL %q\n", rawURL)
	}
	return fmt.Sprintf(
		"fatal: entire:// URL is missing its cluster host (%q is a forge id, not a host).\n"+
			"The full form is entire://<cluster-host>/%s/<owner>/<repo>.\n"+
			"To pick a mirror interactively, run:\n\n    entire repo clone /%s\n",
		seg[0], seg[0], strings.Join(seg, "/"))
}

// loadedVersion populates the build info and returns the resolved version.
func loadedVersion() string {
	versioninfo.Load()
	return versioninfo.Version
}

// infoFlagText renders the output for the standalone --version / --help flags,
// returning false for anything else. Kept pure (version passed in, no globals)
// so it's unit-testable.
func infoFlagText(flag, version string) (string, bool) {
	switch flag {
	case "--version":
		return fmt.Sprintf("%s %s\nGo version: %s\nOS/Arch: %s/%s\n",
			remotehelper.BinaryName, version, runtime.Version(), runtime.GOOS, runtime.GOARCH), true
	case "--help":
		return fmt.Sprintf("%s %s\n\n"+
			"This is a helper which Git calls when encountering entire://... URLs.  "+
			"For more information see https://github.com/entireio/cli.\n",
			remotehelper.BinaryName, version), true
	}
	return "", false
}

// resolveProtocolVersion reads the effective protocol.version from
// the GIT_PROTOCOL environment variable. The value is a colon-
// separated list of key=value pairs (e.g. "version=2"). We accept
// 0, 1, or 2; any other value emits a stderr warning and falls
// back to 2 — upstream Git's default since 2.26.
func resolveProtocolVersion() int {
	return parseProtocolVersion(os.Getenv("GIT_PROTOCOL"), os.Stderr)
}

func parseProtocolVersion(raw string, warn io.Writer) int {
	const defaultVersion = 2
	for kv := range strings.SplitSeq(raw, ":") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k != "version" {
			continue
		}
		switch v {
		case "0":
			return 0
		case "1":
			return 1
		case "2":
			return 2
		}
		fmt.Fprintf(warn, "git-remote-entire: ignoring unrecognised protocol.version=%q; defaulting to %d\n", v, defaultVersion)
		return defaultVersion
	}
	return defaultVersion
}

// clusterRegistryFactory builds the control-plane client the auth path asks
// "is this a cluster you front?". Injected (rather than called directly) so the
// credential gate is unit-testable against a fake registry, with no network and
// no keyring.
type clusterRegistryFactory func(coreURL string, token credentialProvider, skipTLS bool) (coreapi.ClusterLister, error)

// defaultClusterRegistry dials the core with the same credential the git
// transport will use, so the registry lookup shares its silent re-mint instead
// of pinning a token snapshot.
func defaultClusterRegistry(coreURL string, token credentialProvider, skipTLS bool) (coreapi.ClusterLister, error) {
	return coreapi.NewWithTokenSource(coreURL, token, skipTLS)
}

// resolveCreds returns the credential provider used by the git transport:
//
//   - ENTIRE_TOKEN set: use the env JWT verbatim. Skips contexts.json and the
//     keyring entirely — the CI / workload path. A non-URL aud is a hard error,
//     never a silent fallback to context resolution.
//   - otherwise: use the active login context from contexts.json and its
//     refreshed login JWT.
//
// Either way the target cluster host is confirmed against the *core's* cluster
// registry before any credential is handed out — see
// coreapi.VerifyClusterRegistered for why the cluster's own
// /.well-known/entire-cluster.json no longer decides this. cacheDir carries the
// positive-answer cache that keeps a warm git op off the control plane
// (userdirs.Cache() in production); a confirmed cluster therefore costs no
// round trip on the next fetch, and survives a brief core outage.
func resolveCreds(ctx context.Context, parsedURL *url.URL, skipTLS bool, httpClient *http.Client, cacheDir string, newRegistry clusterRegistryFactory) (credentialProvider, func(), error) {
	// Presence of ENTIRE_TOKEN is the signal: if it's set at all (LookupEnv,
	// not Getenv, so we can tell set-empty from unset), we commit to the
	// env-token path and any failure to use it is fatal — never a silent
	// fallback to context auth, which would mask a misconfigured CI runner.
	// Read and trim once here, the only place we touch it, so every downstream
	// consumer (aud derivation and the exchanged subject_token) sees the
	// cleaned value; a trailing newline from $(cat token) is common. An empty
	// or whitespace-only value fails closed.
	if raw, ok := os.LookupEnv(auth.EnvTokenVar); ok {
		envToken := strings.TrimSpace(raw)
		if envToken == "" {
			return nil, nil, fmt.Errorf("%s is set but blank", auth.EnvTokenVar)
		}
		return resolveEnvTokenCreds(ctx, envToken, parsedURL.Host, skipTLS, cacheDir, newRegistry)
	}

	// The acting identity is the ACTIVE context — `--context`/$ENTIRE_CONTEXT
	// for one invocation, else the stored current_context, and nothing else. No
	// other saved login is substituted, so which identity pushed or fetched is
	// always readable from current_context. Its core is then the core we ask
	// about the cluster.
	clusterCtx, err := activeLoginContext(parsedURL.Host)
	if err != nil {
		return nil, nil, err
	}

	// The login-JWT provider transparently refreshes an expired login JWT
	// from the stored refresh token (serialised across processes, rotated
	// tokens persisted) before the git transport uses it as the bearer.
	loginCredential, err := auth.NewRefreshingLoginCredential(clusterCtx, httpClient.Transport, skipTLS)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // NewRefreshingLoginCredential already returns a user-facing error
	}
	provider, onUnauthorized := refreshingProvider(loginCredential)

	coreURL := strings.TrimRight(clusterCtx.CoreURL, "/")
	registry, err := newRegistry(coreURL, provider, skipTLS)
	if err != nil {
		return nil, nil, err
	}
	if err := coreapi.VerifyClusterRegistered(ctx, registry, cacheDir, coreURL, parsedURL.Host); err != nil {
		return nil, nil, err
	}

	debuglog.Printf("auth: login token bearer (core=%s)", coreURL)
	return provider, onUnauthorized, nil
}

// activeLoginContext returns the active contexts.json login, or an error
// naming the cluster we were about to authenticate against. A context with no
// CoreURL is an unusable pointer: it names no core to consult, so it is
// reported as "no active login" rather than dialing an empty host.
func activeLoginContext(clusterHost string) (*contexts.Context, error) {
	f, err := contexts.Load(userdirs.Config())
	if err != nil {
		return nil, fmt.Errorf("load contexts: %w", err)
	}
	sel, err := f.Active()
	if err != nil {
		return nil, err //nolint:wrapcheck // UnknownContextError is already a complete operator message
	}
	if sel.Context == nil || sel.Context.CoreURL == "" {
		return nil, errors.New(noActiveContextHint(clusterHost))
	}
	return sel.Context, nil
}

// noActiveContextHint renders the "nothing to authenticate as" message. Built
// as a string so the multi-line, punctuated operator text stays out of an
// error-string literal.
func noActiveContextHint(clusterHost string) string {
	return fmt.Sprintf("no active auth context for cluster %s.\nRun `entire login`, or select a saved login with `entire auth use <context>`.", clusterHost)
}

// resolveEnvTokenCreds returns a fixed ENTIRE_TOKEN provider after confirming
// the target cluster is one the token's own core fronts. Split out of
// resolveCreds with explicit clusterHost / cacheDir / registry-factory params
// (no os.Getenv, no userdirs globals, no network) so the trust gate below is
// unit-testable.
//
// SECURITY: coreURL is derived from the env token's *unverified* aud claim, so
// the gate can't stop there — an attacker-minted aud would otherwise pick its
// own core. What anchors it is that the core named by aud must ALSO claim the
// cluster host the user typed: a token pointing at an attacker core reaches a
// registry that does not list the real cluster, and the clone fails before any
// credential is attached. The previous shape asked the target host which cores
// to trust, which inverted the trust: the host under scrutiny got to nominate
// its own auditors.
func resolveEnvTokenCreds(ctx context.Context, envToken, clusterHost string, skipTLS bool, cacheDir string, newRegistry clusterRegistryFactory) (credentialProvider, func(), error) {
	coreURL, err := auth.CoreURLFromEnvToken(envToken)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // CoreURLFromEnvToken already returns a user-facing, ENTIRE_TOKEN-prefixed error
	}
	provider := func(context.Context) (string, error) { return envToken, nil }
	registry, err := newRegistry(coreURL, provider, skipTLS)
	if err != nil {
		return nil, nil, err
	}
	if err := coreapi.VerifyClusterRegistered(ctx, registry, cacheDir, coreURL, clusterHost); err != nil {
		return nil, nil, fmt.Errorf("%s aud %q does not front cluster %s: %w", auth.EnvTokenVar, coreURL, clusterHost, err)
	}
	debuglog.Printf("auth: %s bearer (core=%s)", auth.EnvTokenVar, coreURL)
	onUnauthorized := func() {
		debuglog.Printf("data plane rejected static %s bearer; transport will retry once with the configured token", auth.EnvTokenVar)
	}
	return provider, onUnauthorized, nil
}

// gitActionFromRequest classifies a smart-HTTP request as "pull" or "push".
// The jurisdiction token doesn't vary by action, but the classification
// still gates which endpoints may carry credentials (and labels the timing
// logs). Returns "" when the endpoint isn't a recognised git smart-HTTP
// route.
func gitActionFromRequest(req *http.Request) string {
	path := req.URL.Path
	switch req.Method {
	case http.MethodPost:
		switch {
		case strings.HasSuffix(path, "/git-receive-pack"):
			return "push"
		case strings.HasSuffix(path, "/git-upload-pack"):
			return "pull"
		}
	case http.MethodGet:
		if strings.HasSuffix(path, "/info/refs") {
			switch req.URL.Query().Get("service") {
			case "git-receive-pack":
				return "push"
			case "git-upload-pack":
				return "pull"
			}
		}
	}
	return ""
}

// installSignals ties HTTP request lifetimes to the parent git process.
// Ctrl-C delivers SIGINT to the whole foreground process group (us
// included); cancelling ctx aborts in-flight transfers instead of waiting
// out the read timeout. After the first signal we unhook so a second
// Ctrl-C hits the runtime default and hard-exits.
func installSignals() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
		time.Sleep(2 * time.Second)
		fmt.Fprintln(os.Stderr, "git-remote-entire: shutdown taking longer than expected; press Ctrl-C again to force-quit")
	}()
	return ctx, stop
}
