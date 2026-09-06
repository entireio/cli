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
// from local contexts. It uses that context's login JWT (or ENTIRE_TOKEN in
// CI) directly as the git-transport bearer.
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

	"github.com/entireio/auth-go/sts"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/httpclient"
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
	case parsedURL.Host == "" || gitremote.IsForgePathToken(parsedURL.Host):
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

	creds, onUnauthorized, err := resolveCreds(ctx, parsedURL, skipTLS, httpClient)
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
	var oe *sts.ExchangeError
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
// points at that command; a partial path (entire://gh, entire://gh/owner) or a
// non-forge segment falls back to the plain missing-host error rather than
// suggesting a clone command that would reject the ref. Kept pure so it's
// unit-testable.
//
// The trailing segments are labelled per forge (gitremote.ForgePathLabels), so
// a native path reads <project>/<repo> rather than a mirror's <owner>/<repo>.
// The suggestion stays neutral about what `repo clone` will then do — it
// prompts between clusters for a multi-cluster mirror, and goes straight to the
// home cluster for a native ref — because naming the mirror picker here was
// wrong for half the forges.
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
	if len(seg) != 3 || seg[0] == "" || seg[1] == "" || seg[2] == "" || !gitremote.IsForgePathToken(seg[0]) {
		return fmt.Sprintf("fatal: missing host in URL %q\n", rawURL)
	}
	return fmt.Sprintf(
		"fatal: entire:// URL is missing its cluster host (%q is a forge id, not a host).\n"+
			"The full form is entire://<cluster-host>/%s/%s.\n"+
			"To clone it, run:\n\n    entire repo clone /%s\n",
		seg[0], seg[0], gitremote.ForgePathLabels(seg[0]), strings.Join(seg, "/"))
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

// resolveCreds returns the credential provider used by the git transport:
//
//   - ENTIRE_TOKEN set: use the env JWT verbatim. Skips contexts.json and the
//     keyring entirely — the CI / workload path. A non-URL aud is a hard error,
//     never a silent fallback to context resolution.
//   - otherwise: resolve the login context for this cluster from contexts.json
//     and use its refreshed login JWT.
func resolveCreds(ctx context.Context, parsedURL *url.URL, skipTLS bool, httpClient *http.Client) (credentialProvider, func(), error) {
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
		return resolveEnvTokenCreds(ctx, envToken, parsedURL.Host, userdirs.Cache(), httpClient)
	}

	// Resolve which login context authenticates this cluster: the cluster's
	// login servers are taken from the cluster_cores.json cache (or a live
	// /.well-known fetch on miss/expiry), and the ACTIVE context must be issued
	// by one of them. No other saved login is substituted, so which identity
	// pushed or fetched is always readable from current_context.
	cfgDir := userdirs.Config()
	clusterAuth, err := clusterdiscovery.ResolveClusterAuth(ctx, cfgDir, userdirs.Cache(), parsedURL.Host, httpClient, debuglog.Printf)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // ResolveClusterAuth already returns a user-facing error; preserved verbatim for the "fatal: <msg>" surface
	}
	clusterCtx := clusterAuth.Context

	// The login-JWT provider transparently refreshes an expired login JWT
	// from the stored refresh token (serialised across processes, rotated
	// tokens persisted) before the git transport uses it as the bearer.
	loginCredential, err := auth.NewRefreshingLoginCredential(clusterCtx, httpClient.Transport, skipTLS)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // NewRefreshingLoginCredential already returns a user-facing error
	}

	debuglog.Printf("auth: login token bearer (core=%s)", clusterCtx.CoreURL)
	provider, onUnauthorized := refreshingProvider(loginCredential)
	return provider, onUnauthorized, nil
}

// resolveEnvTokenCreds returns a fixed ENTIRE_TOKEN provider after validating
// its control-plane audience against the target cluster. Split out of
// resolveCreds with explicit clusterHost/cacheDir params (no os.Getenv /
// userdirs.Cache globals) so the trust gate below is unit-testable against a
// fake well-known server.
//
// SECURITY: coreURL is derived from the env token's *unverified* aud claim, and
// we confirm the core is one the target cluster actually advertises — anchored
// to the clone URL's host the user typed (TLS to its
// /.well-known/entire-cluster.json), not to the token's own claims.
//
// The gate is only as strong as that TLS verification: with
// ENTIRE_TLS_SKIP_VERIFY=true (a local-dev escape hatch) the well-known fetch
// is no longer authenticated, so a MITM could advertise an attacker host as a
// trusted core. Do not combine ENTIRE_TOKEN with ENTIRE_TLS_SKIP_VERIFY in
// CI / workload environments.
func resolveEnvTokenCreds(ctx context.Context, envToken, clusterHost, cacheDir string, httpClient *http.Client) (credentialProvider, func(), error) {
	coreURL, err := auth.CoreURLFromEnvToken(envToken)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // CoreURLFromEnvToken already returns a user-facing, ENTIRE_TOKEN-prefixed error
	}
	cluster, err := clusterdiscovery.ResolveClusterCores(ctx, cacheDir, clusterHost, httpClient, debuglog.Printf)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // ResolveClusterCores returns a user-facing discovery error
	}
	if !coreTrusted(coreURL, cluster.CoreURLs) {
		return nil, nil, fmt.Errorf("%s aud %q is not a trusted login server for cluster %s (advertised: %s); the token belongs to a different cluster",
			auth.EnvTokenVar, coreURL, clusterHost, strings.Join(cluster.CoreURLs, ", "))
	}
	debuglog.Printf("auth: %s bearer (core=%s)", auth.EnvTokenVar, coreURL)
	provider := func(context.Context) (string, error) { return envToken, nil }
	onUnauthorized := func() {
		debuglog.Printf("data plane rejected static %s bearer; transport will retry once with the configured token", auth.EnvTokenVar)
	}
	return provider, onUnauthorized, nil
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
