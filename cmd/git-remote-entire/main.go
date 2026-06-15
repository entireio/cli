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
// from local contexts. It then mints repo-scoped tokens by exchanging that
// context's login JWT. A pre-contexts.json login is migrated at read-time so
// existing users don't have to re-authenticate.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/entireclient/repocreds"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"github.com/entireio/cli/internal/remotehelper"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
	"github.com/entireio/cli/internal/remotehelper/githelper"
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
	// The repo-scoped token's audience is <clusterBaseURL><repoSlug>. The
	// audience pins to the cluster entry URL (not a replica node), matching
	// what the server validates the exchanged token against.
	clusterBaseURL := nodeCfg.EntryURL
	repoSlug := parsedURL.Path

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &httpclient.UserAgentTransport{
			Next: httpclient.NewTransport(skipTLS),
			UA:   httpUserAgent,
		},
	}

	creds, err := resolveCreds(ctx, parsedURL, clusterBaseURL, skipTLS, httpClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		return 128
	}

	setAuth := func(req *http.Request) error {
		action := gitActionFromRequest(req)
		if action == "" {
			return fmt.Errorf("cannot classify git op for %s %s; scoped-token exchange requires a recognised smart-HTTP endpoint", req.Method, req.URL.Path)
		}
		token, err := creds.Token(req.Context(), repoSlug, action)
		if err != nil {
			return fmt.Errorf("repo-scoped token exchange: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}

	var onNodeFailed func(string)
	if nodeCfg.Caching() {
		onNodeFailed = func(string) { replicas.Invalidate(nodeCfg.ClusterHost, nodeCfg.RepoPath) }
	}

	proxy := transport.New(transport.Config{
		Nodes:        nodeCfg,
		Path:         parsedURL.Path,
		SkipTLS:      skipTLS,
		SetAuth:      setAuth,
		OnNodeFailed: onNodeFailed,
		UserAgent:    httpUserAgent,
	})

	protocolVersion := resolveProtocolVersion()
	debuglog.Printf("git protocol.version=%d (v2 advertises stateless-connect + push; v0/v1 advertises connect)", protocolVersion)

	if err := githelper.Run(ctx, proxy, protocolVersion, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		return 128
	}
	return 0
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

// resolveCreds builds the repo-scoped token cache, choosing the auth source:
//
//   - ENTIRE_TOKEN set: use the env JWT verbatim as the login token, deriving
//     the login server URL from its aud claim. Skips contexts.json and the keyring
//     entirely — the CI / workload-identity path. A non-URL aud is a hard
//     error, never a silent fallback to context resolution.
//   - otherwise: resolve the login context for this cluster from contexts.json
//     (migrating any pre-contexts.json login first) and exchange its stored
//     login JWT.
func resolveCreds(ctx context.Context, parsedURL *url.URL, clusterBaseURL string, skipTLS bool, httpClient *http.Client) (*repocreds.Cache, error) {
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
		return resolveEnvTokenCreds(ctx, envToken, parsedURL.Host, clusterBaseURL, userdirs.Cache(), httpClient)
	}

	// Bridge any pre-contexts.json login so the resolver can find it.
	if _, err := auth.MigrateLegacyLoginContext(); err != nil {
		debuglog.Printf("legacy login migration: %v", err)
	}

	// Resolve which login context authenticates this cluster: the cluster's
	// login servers are taken from the cluster_cores.json cache (or a live
	// /.well-known fetch on miss/expiry), then the account is selected from
	// local contexts — active context if eligible, else the sole eligible
	// one, else an explicit-choice error.
	cfgDir := userdirs.Config()
	clusterCtx, err := clusterdiscovery.ResolveContextForCluster(ctx, cfgDir, userdirs.Cache(), parsedURL.Host, httpClient, debuglog.Printf)
	if err != nil {
		return nil, err //nolint:wrapcheck // ResolveContextForCluster already returns a user-facing error; preserved verbatim for the "fatal: <msg>" surface
	}

	// The login-JWT provider transparently refreshes an expired login JWT
	// from the stored refresh token (serialised across processes, rotated
	// tokens persisted) before repocreds exchanges it for repo-scoped tokens.
	loginProvider, err := auth.NewRefreshingLoginProvider(clusterCtx, httpClient.Transport, skipTLS)
	if err != nil {
		return nil, err //nolint:wrapcheck // NewRefreshingLoginProvider already returns a user-facing error
	}

	// Mint repo-scoped tokens by exchanging the context's login JWT at its
	// login server's /oauth/token, cached per (repo, action) for this invocation.
	return repocreds.New(clusterCtx.CoreURL, clusterBaseURL, loginProvider, httpClient), nil
}

// resolveEnvTokenCreds builds the repo-cred cache for the ENTIRE_TOKEN path.
// Split out of resolveCreds with explicit clusterHost/cacheDir params (no
// os.Getenv / userdirs.Cache globals) so the trust gate below is unit-testable
// against a fake well-known server.
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
// CI / workload-identity environments.
func resolveEnvTokenCreds(ctx context.Context, envToken, clusterHost, clusterBaseURL, cacheDir string, httpClient *http.Client) (*repocreds.Cache, error) {
	coreURL, err := auth.CoreURLFromEnvToken(envToken)
	if err != nil {
		return nil, err //nolint:wrapcheck // CoreURLFromEnvToken already returns a user-facing, ENTIRE_TOKEN-prefixed error
	}
	cores, err := clusterdiscovery.ResolveClusterCores(ctx, cacheDir, clusterHost, httpClient, debuglog.Printf)
	if err != nil {
		return nil, err //nolint:wrapcheck // ResolveClusterCores returns a user-facing discovery error
	}
	if !coreTrusted(coreURL, cores) {
		return nil, fmt.Errorf("%s aud %q is not a trusted login server for cluster %s (advertised: %s); the token belongs to a different cluster",
			auth.EnvTokenVar, coreURL, clusterHost, strings.Join(cores, ", "))
	}
	debuglog.Printf("authenticating via %s; core=%s", auth.EnvTokenVar, coreURL)
	return repocreds.New(coreURL, clusterBaseURL, func(context.Context) (string, error) {
		return envToken, nil
	}, httpClient), nil
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

// gitActionFromRequest classifies a smart-HTTP request as "pull" or "push"
// so the right repo-scoped token can be minted. Returns "" when the
// endpoint isn't a recognised git smart-HTTP route.
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
