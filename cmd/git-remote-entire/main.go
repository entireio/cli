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
// Authentication resolves the login context for the target cluster from the
// shared contexts.json: the cluster's cores come from the cluster_cores.json
// cache (or a live /.well-known fetch on miss), then the account is selected
// from local contexts. It then mints a jurisdiction access token by
// exchanging that context's login JWT (or ENTIRE_TOKEN in CI).
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
	"syscall"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
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
	case parsedURL.Host == "":
		fmt.Fprintf(os.Stderr, "fatal: missing host in URL %q\n", rawURL)
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

	creds, err := resolveCreds(ctx, parsedURL, skipTLS, httpClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		return 128
	}

	setAuth := func(req *http.Request) error {
		// Refuse to attach credentials to a request we can't classify as a
		// known git smart-HTTP endpoint. The jurisdiction token doesn't
		// need the action, but sending a bearer to an unexpected endpoint
		// is never right.
		action := gitActionFromRequest(req)
		if action == "" {
			return fmt.Errorf("refusing to attach credentials: %s %s is not a recognised git smart-HTTP endpoint", req.Method, req.URL.Path)
		}
		start := time.Now()
		token, err := creds.Token(req.Context())
		if err != nil {
			return fmt.Errorf("git credential exchange: %w", err)
		}
		debuglog.Printf("timing: token-acquire action=%s dur_ms=%d (keychain + login refresh + exchange; ~0 when memoized)", action, time.Since(start).Milliseconds())
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}

	var onNodeFailed func(string)
	if nodeCfg.Caching() {
		onNodeFailed = func(string) { replicas.Invalidate(nodeCfg.ClusterHost, nodeCfg.RepoPath) }
	}

	proxy := transport.New(transport.Config{
		Nodes:   nodeCfg,
		Path:    parsedURL.Path,
		SkipTLS: skipTLS,
		SetAuth: setAuth,
		// A 401 means the data plane rejected the credential itself (e.g.
		// signing-key rotation invalidated a persisted jurisdiction token) —
		// drop it so the next invocation re-mints instead of failing until
		// the token's recorded expiry. The env-token source persists
		// nothing; it drops only its in-process memo.
		OnUnauthorized: creds.Invalidate,
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

// resolveCreds builds the git-credential source. Both paths mint a
// jurisdiction access token; they differ in what session credential seeds
// the exchange and where the token is cached:
//
//   - ENTIRE_TOKEN set: use the env JWT verbatim as the session credential,
//     exchanging it at the core derived from its aud claim. Skips
//     contexts.json and the keyring entirely — the CI / workload path,
//     memoized in-process only. A non-URL aud is a hard error, never a
//     silent fallback to context resolution.
//   - otherwise: resolve the login context for this cluster from contexts.json
//     and mint a keychain-persisted jurisdiction access token from its
//     stored login JWT.
func resolveCreds(ctx context.Context, parsedURL *url.URL, skipTLS bool, httpClient *http.Client) (*jurisdictionTokenSource, error) {
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
			return nil, fmt.Errorf("%s is set but blank", auth.EnvTokenVar)
		}
		return resolveEnvTokenCreds(ctx, envToken, parsedURL.Host, userdirs.Cache(), httpClient)
	}

	// Resolve which login context authenticates this cluster: the cluster's
	// login servers are taken from the cluster_cores.json cache (or a live
	// /.well-known fetch on miss/expiry), then the account is selected from
	// local contexts — active context if eligible, else the sole eligible
	// one, else an explicit-choice error.
	cfgDir := userdirs.Config()
	clusterAuth, err := clusterdiscovery.ResolveClusterAuth(ctx, cfgDir, userdirs.Cache(), parsedURL.Host, httpClient, debuglog.Printf)
	if err != nil {
		return nil, err //nolint:wrapcheck // ResolveClusterAuth already returns a user-facing error; preserved verbatim for the "fatal: <msg>" surface
	}
	clusterCtx := clusterAuth.Context

	// The login-JWT provider transparently refreshes an expired login JWT
	// from the stored refresh token (serialised across processes, rotated
	// tokens persisted) before the jurisdiction-token exchange consumes it.
	loginProvider, err := auth.NewRefreshingLoginProvider(clusterCtx, httpClient.Transport, skipTLS)
	if err != nil {
		return nil, err //nolint:wrapcheck // NewRefreshingLoginProvider already returns a user-facing error
	}

	// One keychain-persisted jurisdiction access token authenticates every
	// repo (authorized live at the data plane) — there is no repo-scoped
	// fallback on this path. The cluster must advertise its jurisdiction
	// audience.
	audience := clusterAuth.JurisdictionAudience
	if audience == "" {
		return nil, missingJurisdictionAudienceErr(parsedURL.Host)
	}
	debuglog.Printf("auth: jurisdiction access token (aud=%s, core=%s)", audience, clusterCtx.CoreURL)
	return newJurisdictionTokenSource(clusterCtx.CoreURL, audience, clusterAuth.JurisdictionCoreURL, clusterCtx.Handle, loginProvider, httpClient), nil
}

// resolveEnvTokenCreds builds the jurisdiction-token source for the
// ENTIRE_TOKEN path. Split out of resolveCreds with explicit
// clusterHost/cacheDir params (no os.Getenv / userdirs.Cache globals) so the
// trust gate below is unit-testable against a fake well-known server.
//
// The exchange runs at the env token's own core with the cluster's advertised
// jurisdiction audience. No home/cross-jurisdiction routing is needed: the
// trust gate guarantees that core fronts the target cluster, so it owns the
// cluster's jurisdiction audience.
//
// SECURITY: coreURL is derived from the env token's *unverified* aud claim, and
// it becomes the host the token is POSTed to as a subject_token during
// exchange. Before trusting it, we confirm the core is one the target cluster
// actually advertises — anchored to the clone URL's host the user typed (TLS to
// its /.well-known/entire-cluster.json), not to the token's own claims. Without
// this gate a forged aud could redirect the token to an attacker-chosen host.
//
// The gate is only as strong as that TLS verification: with
// ENTIRE_TLS_SKIP_VERIFY=true (a local-dev escape hatch) the well-known fetch
// is no longer authenticated, so a MITM could advertise an attacker host as a
// trusted core. Do not combine ENTIRE_TOKEN with ENTIRE_TLS_SKIP_VERIFY in
// CI / workload environments.
func resolveEnvTokenCreds(ctx context.Context, envToken, clusterHost, cacheDir string, httpClient *http.Client) (*jurisdictionTokenSource, error) {
	coreURL, err := auth.CoreURLFromEnvToken(envToken)
	if err != nil {
		return nil, err //nolint:wrapcheck // CoreURLFromEnvToken already returns a user-facing, ENTIRE_TOKEN-prefixed error
	}
	cluster, err := clusterdiscovery.ResolveClusterCores(ctx, cacheDir, clusterHost, httpClient, debuglog.Printf)
	if err != nil {
		return nil, err //nolint:wrapcheck // ResolveClusterCores returns a user-facing discovery error
	}
	if !coreTrusted(coreURL, cluster.CoreURLs) {
		return nil, fmt.Errorf("%s aud %q is not a trusted login server for cluster %s (advertised: %s); the token belongs to a different cluster",
			auth.EnvTokenVar, coreURL, clusterHost, strings.Join(cluster.CoreURLs, ", "))
	}
	if cluster.JurisdictionAudience == "" {
		return nil, missingJurisdictionAudienceErr(clusterHost)
	}
	hint := crossJurisdictionHint(coreURL, clusterHost, cluster.JurisdictionCoreURL)
	if hint != "" {
		debuglog.Printf("auth: %s core %s differs from cluster %s's jurisdiction core %s; the exchange will likely be refused", auth.EnvTokenVar, coreURL, clusterHost, cluster.JurisdictionCoreURL)
	}
	debuglog.Printf("auth: %s jurisdiction access token (aud=%s, core=%s)", auth.EnvTokenVar, cluster.JurisdictionAudience, coreURL)
	return newEnvJurisdictionTokenSource(coreURL, cluster.JurisdictionAudience, envToken, hint, httpClient), nil
}

// crossJurisdictionHint pre-computes the actionable message for the one
// misconfiguration the trust gate can't catch: an ENTIRE_TOKEN minted at a
// core of a different jurisdiction than the cluster's. Clusters advertise
// every jurisdiction's cores as trusted login servers, so such a token
// passes the gate — but the exchange is doomed, because only the cluster's
// own jurisdiction core mints for its audience, and the core-side refusal
// is an opaque invalid_target. Returned as a suffix for that eventual
// exchange error rather than a hard pre-flight failure: clusters may
// advertise several same-jurisdiction core URLs, so inequality here is a
// strong hint, not proof.
func crossJurisdictionHint(coreURL, clusterHost, jurisdictionCoreURL string) string {
	if jurisdictionCoreURL == "" {
		return ""
	}
	if strings.EqualFold(strings.TrimRight(coreURL, "/"), strings.TrimRight(jurisdictionCoreURL, "/")) {
		return ""
	}
	return fmt.Sprintf("\n%s was minted at %s, but cluster %s's jurisdiction core is %s — point your CI auth url at %s",
		auth.EnvTokenVar, coreURL, clusterHost, jurisdictionCoreURL, jurisdictionCoreURL)
}

// missingJurisdictionAudienceErr names the one condition both auth paths
// refuse on: a cluster that predates jurisdiction-token git auth.
func missingJurisdictionAudienceErr(clusterHost string) error {
	return fmt.Errorf("cluster %s advertises no jurisdiction_audience at %s; its entire-server predates jurisdiction-token git auth", clusterHost, clusterdiscovery.Path)
}

// coreTrusted reports whether coreURL is in the cluster's advertised core
// set, comparing on trailing-slash-insensitive equality to match how core
// URLs are compared elsewhere (contexts.ContextsForIssuer, auth.sameIssuer).
func coreTrusted(coreURL string, trusted []string) bool {
	want := strings.TrimRight(coreURL, "/")
	for _, t := range trusted {
		if strings.TrimRight(t, "/") == want {
			return true
		}
	}
	return false
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
