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
	"strings"
	"syscall"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/discovery"
	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/entireclient/repocreds"
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
	if len(args) < 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <remote-name> <url>\n", remotehelper.BinaryName)
		return 128
	}

	// Build info drives the agent string the helper advertises upstream.
	versioninfo.Load()
	githelper.Agent = remotehelper.BinaryName + "/" + versioninfo.Commit

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
		Timeout:   30 * time.Second,
		Transport: httpclient.NewTransport(skipTLS),
	}

	// Bridge any pre-contexts.json login so the resolver can find it.
	if _, err := auth.MigrateLegacyLoginContext(); err != nil {
		debuglog.Printf("legacy login migration: %v", err)
	}

	// Resolve which login context authenticates this cluster: the cluster's
	// cores are taken from the cluster_cores.json cache (or a live
	// /.well-known fetch on miss/expiry), then the account is selected from
	// local contexts — active context if eligible, else the sole eligible
	// one, else an explicit-choice error.
	cfgDir := contexts.DefaultConfigDir()
	clusterCtx, err := clusterdiscovery.ResolveContextForCluster(ctx, cfgDir, discovery.DefaultCacheDir(), parsedURL.Host, httpClient, debuglog.Printf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		return 128
	}

	// The login-JWT provider transparently refreshes an expired login JWT
	// from the stored refresh token (serialised across processes, rotated
	// tokens persisted) before repocreds exchanges it for repo-scoped
	// tokens.
	loginProvider, err := auth.NewRefreshingLoginProvider(clusterCtx, httpClient.Transport, skipTLS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		return 128
	}

	// Mint repo-scoped tokens by exchanging the context's login JWT at its
	// core's /oauth/token, cached per (repo, action) for this invocation.
	creds := repocreds.New(clusterCtx.CoreURL, clusterBaseURL, loginProvider, httpClient)

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
	})

	protocolVersion := resolveProtocolVersion()
	debuglog.Printf("git protocol.version=%d (v2 advertises stateless-connect + push; v0/v1 advertises connect)", protocolVersion)

	if err := githelper.Run(ctx, proxy, protocolVersion, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		return 128
	}
	return 0
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
