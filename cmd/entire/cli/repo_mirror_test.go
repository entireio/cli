package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
)

func TestExplainSuspendedMirror(t *testing.T) {
	t.Parallel()
	const id = "01KS6KFJR2XS6PZ188MVYE07AN"

	t.Run("suspended mirror is explained with resume command", func(t *testing.T) {
		t.Parallel()
		// Wrap the sentinel the way RepoScopedToken/waitForMirrorClone do, to
		// prove detection survives the wrapping chain.
		err := fmt.Errorf("authorize clone probe: %w", fmt.Errorf("repo-scoped token exchange: %w", auth.ErrRepoTargetUnknown))
		var buf bytes.Buffer
		handled, serr := explainSuspendedMirror(&buf, id, false, err)
		if !handled {
			t.Fatal("expected handled=true for ErrRepoTargetUnknown")
		}
		var silent *SilentError
		if !errors.As(serr, &silent) {
			t.Errorf("expected a SilentError, got %T: %v", serr, serr)
		}
		out := buf.String()
		if !strings.Contains(out, id) {
			t.Errorf("message %q omits the mirror id", out)
		}
		if !strings.Contains(out, "entire-core admin mirrors resume "+id) {
			t.Errorf("message %q omits the resume command", out)
		}
	})

	t.Run("fresh create passes invalid_target through as propagation lag", func(t *testing.T) {
		t.Parallel()
		// Same invalid_target signature, but on a just-created placement it's
		// eventual-consistency lag, not suspension — don't misdirect to resume.
		err := fmt.Errorf("authorize clone probe: %w", fmt.Errorf("repo-scoped token exchange: %w", auth.ErrRepoTargetUnknown))
		var buf bytes.Buffer
		handled, serr := explainSuspendedMirror(&buf, id, true, err)
		if handled {
			t.Error("expected handled=false for a fresh create")
		}
		if serr != nil {
			t.Errorf("expected nil error, got %v", serr)
		}
		if buf.Len() != 0 {
			t.Errorf("expected no output, got %q", buf.String())
		}
	})

	t.Run("unrelated error passes through untouched", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		handled, serr := explainSuspendedMirror(&buf, id, false, errors.New("timed out waiting for initial clone"))
		if handled {
			t.Error("expected handled=false for an unrelated error")
		}
		if serr != nil {
			t.Errorf("expected nil error, got %v", serr)
		}
		if buf.Len() != 0 {
			t.Errorf("expected no output, got %q", buf.String())
		}
	})
}

// TestFinishMirrorCreate exercises the post-create branching: when the
// upstream is empty we must skip the HEAD-poll loop (an empty repo never
// advertises a HEAD), yet an *existing* empty placement must still go through
// the token exchange so a suspended mirror surfaces its resume guidance
// instead of a success-style "nothing to clone" note.
func TestFinishMirrorCreate(t *testing.T) {
	t.Parallel()

	const id = "01KS6KFJR2XS6PZ188MVYE07AN"
	const mirrorURL = "entire://eu-west-1.entire.io/gh/octocat/hello-world"
	// The error shape RepoScopedToken/waitForMirrorClone produce for a
	// suspended (non-servable) placement.
	suspended := fmt.Errorf("repo-scoped token exchange: %w", auth.ErrRepoTargetUnknown)

	// seen records whether each injected operation ran, so we can assert the
	// empty path never polls and a fresh create never probes.
	type call struct{ probed, waited bool }

	t.Run("fresh empty create skips both probe and poll", func(t *testing.T) {
		t.Parallel()
		var seen call
		var out, errW bytes.Buffer
		created := &coreapi.CreatedMirror{Created: true, Empty: true, MirrorId: id, MirrorUrl: mirrorURL}
		err := finishMirrorCreate(&out, &errW, created, false,
			func() error { seen.probed = true; return nil },
			func() error { seen.waited = true; return nil },
		)
		require.NoError(t, err)
		require.False(t, seen.probed, "a fresh create can't be suspended; must not probe")
		require.False(t, seen.waited, "empty upstream has nothing to clone; must not poll")
		require.Contains(t, out.String(), "nothing to clone")
		require.Empty(t, errW.String())
	})

	t.Run("existing empty healthy probes but does not poll", func(t *testing.T) {
		t.Parallel()
		var seen call
		var out, errW bytes.Buffer
		created := &coreapi.CreatedMirror{Created: false, Empty: true, MirrorId: id, MirrorUrl: mirrorURL}
		err := finishMirrorCreate(&out, &errW, created, false,
			func() error { seen.probed = true; return nil },
			func() error { seen.waited = true; return nil },
		)
		require.NoError(t, err)
		require.True(t, seen.probed, "existing empty placement must probe for suspension")
		require.False(t, seen.waited, "empty upstream has nothing to clone; must not poll")
		require.Contains(t, out.String(), "nothing to clone")
	})

	t.Run("existing empty suspended surfaces resume guidance", func(t *testing.T) {
		t.Parallel()
		var seen call
		var out, errW bytes.Buffer
		created := &coreapi.CreatedMirror{Created: false, Empty: true, MirrorId: id, MirrorUrl: mirrorURL}
		err := finishMirrorCreate(&out, &errW, created, false,
			func() error { seen.probed = true; return suspended },
			func() error { seen.waited = true; return nil },
		)
		var silent *SilentError
		require.ErrorAs(t, err, &silent, "suspended mirror must return a SilentError")
		require.True(t, seen.probed)
		require.False(t, seen.waited, "must not poll a suspended empty mirror")
		require.Contains(t, errW.String(), "entire-core admin mirrors resume "+id)
		require.NotContains(t, out.String(), "nothing to clone",
			"a suspended mirror must not get the success-style empty note")
	})

	t.Run("existing empty transient probe error is non-fatal", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		created := &coreapi.CreatedMirror{Created: false, Empty: true, MirrorId: id, MirrorUrl: mirrorURL}
		err := finishMirrorCreate(&out, &errW, created, false,
			func() error { return errors.New("dial tcp: connection refused") },
			func() error { t.Fatal("must not poll an empty mirror"); return nil },
		)
		require.NoError(t, err, "a non-suspension probe error must not fail a create whose placement exists")
		require.Contains(t, out.String(), "nothing to clone")
	})

	t.Run("non-empty no-wait skips both probe and poll", func(t *testing.T) {
		t.Parallel()
		var seen call
		var out, errW bytes.Buffer
		created := &coreapi.CreatedMirror{Created: true, Empty: false, MirrorId: id, MirrorUrl: mirrorURL}
		err := finishMirrorCreate(&out, &errW, created, true,
			func() error { seen.probed = true; return nil },
			func() error { seen.waited = true; return nil },
		)
		require.NoError(t, err)
		require.False(t, seen.probed)
		require.False(t, seen.waited, "--no-wait must not poll")
		require.Contains(t, out.String(), "still be in progress")
	})

	t.Run("non-empty waits for clone then prints clone hint", func(t *testing.T) {
		t.Parallel()
		var seen call
		var out, errW bytes.Buffer
		created := &coreapi.CreatedMirror{Created: true, Empty: false, MirrorId: id, MirrorUrl: mirrorURL}
		err := finishMirrorCreate(&out, &errW, created, false,
			func() error { seen.probed = true; return nil },
			func() error { seen.waited = true; return nil },
		)
		require.NoError(t, err)
		require.False(t, seen.probed, "non-empty path detects suspension through waitClone, not a separate probe")
		require.True(t, seen.waited)
		require.Contains(t, out.String(), "git clone "+mirrorURL)
	})

	t.Run("non-empty existing suspended surfaces resume guidance", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		created := &coreapi.CreatedMirror{Created: false, Empty: false, MirrorId: id, MirrorUrl: mirrorURL}
		err := finishMirrorCreate(&out, &errW, created, false,
			func() error { return nil },
			func() error { return suspended },
		)
		var silent *SilentError
		require.ErrorAs(t, err, &silent)
		require.Contains(t, errW.String(), "entire-core admin mirrors resume "+id)
		require.NotContains(t, out.String(), "git clone")
	})

	t.Run("non-empty wait error other than suspension propagates", func(t *testing.T) {
		t.Parallel()
		var out, errW bytes.Buffer
		created := &coreapi.CreatedMirror{Created: true, Empty: false, MirrorId: id, MirrorUrl: mirrorURL}
		wantErr := errors.New("timed out waiting for initial clone")
		err := finishMirrorCreate(&out, &errW, created, false,
			func() error { return nil },
			func() error { return wantErr },
		)
		require.ErrorIs(t, err, wantErr)
		require.Empty(t, errW.String())
	})
}

// TestParseGitHubURL is ported from entiredb's cmd/entire-repo/cli
// mirror_test.go, since parseGitHubURL was carried over verbatim.
func TestParseGitHubURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{name: "HTTPS", url: "https://github.com/entirehq/entiredb", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "HTTPS with .git", url: "https://github.com/entirehq/entiredb.git", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "SSH", url: "git@github.com:entirehq/entiredb", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "SSH with .git", url: "git@github.com:entirehq/entiredb.git", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "HTTP", url: "http://github.com/owner/repo", wantOwner: "owner", wantRepo: "repo"},
		{name: "bare with github.com prefix", url: "github.com/octocat/hello-world", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "bare github.com prefix with .git", url: "github.com/octocat/hello-world.git", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "bare owner/repo", url: "octocat/hello-world", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "bare lowercased", url: "OctoCat/Hello-World", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "repo with dot", url: "github.com/octocat/hello.world", wantOwner: "octocat", wantRepo: "hello.world"},
		{name: "repo with underscore", url: "octocat/hello_world", wantOwner: "octocat", wantRepo: "hello_world"},
		{name: "GitLab", url: "https://gitlab.com/owner/repo", wantErr: true},
		{name: "missing repo", url: "https://github.com/owner", wantErr: true},
		{name: "not a URL", url: "not-a-url", wantErr: true},
		{name: "entire URL", url: "entire://host/git/owner/repo", wantErr: true},
		// Parameter-smuggling shapes the tightened owner/repo charset rejects:
		// these would otherwise mutate the audience / probe URL built from
		// owner/repo.
		{name: "repo with query smuggle", url: "octocat/repo?bypass=1", wantErr: true},
		{name: "repo with fragment", url: "octocat/repo#anchor", wantErr: true},
		{name: "owner with at-sign", url: "a@b/repo", wantErr: true},
		{name: "repo with encoded slash", url: "octocat/repo%2fevil", wantErr: true},
		{name: "owner with dot-dot", url: "../repo", wantErr: true},
		{name: "owner with underscore (not a GitHub login)", url: "oct_cat/repo", wantErr: true},
		// Dot-only repo names pass the gitHubRepoPat charset (which allows
		// dots) but would embed a literal "." or ".." in the audience and
		// probe URL — reject at the boundary.
		{name: "dot-only repo", url: "github.com/owner/..", wantErr: true},
		{name: "single-dot repo", url: "github.com/owner/.", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := parseGitHubURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseGitHubURL(%q) expected error, got %q/%q", tt.url, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitHubURL(%q) unexpected error: %v", tt.url, err)
			}
			if owner != tt.wantOwner || repo != tt.wantRepo {
				t.Errorf("parseGitHubURL(%q) = %q/%q, want %q/%q", tt.url, owner, repo, tt.wantOwner, tt.wantRepo)
			}
		})
	}
}

func TestMirrorRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mirror coreapi.Mirror
		want   []string
	}{
		{
			name:   "private mirror synthesises clone URL",
			mirror: coreapi.Mirror{Owner: "entirehq", Repo: "entire.io", ClusterHost: "aws-us-east-2.entire.io", IsPrivate: coreapi.NewOptBool(true)},
			want:   []string{"entirehq/entire.io", "entire://aws-us-east-2.entire.io/gh/entirehq/entire.io", "yes"},
		},
		{
			name:   "public mirror, unset IsPrivate defaults to no",
			mirror: coreapi.Mirror{Owner: "octocat", Repo: "hello", ClusterHost: "eu-west-1.entire.io"},
			want:   []string{"octocat/hello", "entire://eu-west-1.entire.io/gh/octocat/hello", "no"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mirrorRow(tt.mirror)
			if len(got) != len(tt.want) {
				t.Fatalf("mirrorRow len = %d, want %d (%v)", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("mirrorRow[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestClusterArg(t *testing.T) {
	t.Parallel()
	if got := clusterArg([]string{"github.com/o/r", "eu-west-1.entire.io"}); got != "eu-west-1.entire.io" {
		t.Errorf("explicit cluster = %q, want eu-west-1.entire.io", got)
	}
	if got := clusterArg([]string{"github.com/o/r"}); got != defaultClusterHost {
		t.Errorf("omitted cluster = %q, want default %q", got, defaultClusterHost)
	}
}

func TestValidateClusterHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{name: "default cluster", host: defaultClusterHost},
		{name: "other region", host: "eu-west-1.entire.io"},
		{name: "single label", host: "localhost"},
		{name: "host with port", host: "localhost:8080"},
		{name: "ipv4", host: "10.0.0.1"},
		{name: "ipv4 with port", host: "10.0.0.1:8080"},
		// IPv6 takes a different path through validateClusterHost: the
		// host must be bracketed for url.Parse to round-trip, and
		// u.Hostname() strips the brackets before net.ParseIP sees it.
		{name: "ipv6 with port", host: "[::1]:8080"},
		// The token-leak primitive: userinfo demotes the real cluster so the
		// request (and basic-auth token) targets evil.com.
		{name: "userinfo smuggle", host: "aws-us-east-2.entire.io@evil.com", wantErr: true},
		{name: "path smuggle", host: "aws-us-east-2.entire.io/../evil", wantErr: true},
		{name: "query smuggle", host: "aws-us-east-2.entire.io?x=1", wantErr: true},
		{name: "fragment smuggle", host: "aws-us-east-2.entire.io#x", wantErr: true},
		{name: "scheme prefix", host: "https://evil.com", wantErr: true},
		{name: "empty", host: "", wantErr: true},
		{name: "whitespace", host: "   ", wantErr: true},
		{name: "leading hyphen label", host: "-bad.entire.io", wantErr: true},
		{name: "space in host", host: "evil .com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateClusterHost(tt.host)
			if tt.wantErr && err == nil {
				t.Errorf("validateClusterHost(%q) = nil, want error", tt.host)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateClusterHost(%q) = %v, want nil", tt.host, err)
			}
		})
	}
}

// TestMirrorAdvertisesHead_ReusesConnection is the regression test for the
// body drain in mirrorAdvertisesHead. The probe runs every 2s for up to 30m,
// and probeClient keeps a small idle pool specifically to reuse the TLS
// session across ticks — but Go only returns a connection to that pool when
// the response body is read to EOF before Close. If the drain is removed (or
// re-capped shorter than the body), the body is left partially read and the
// transport closes the connection instead, so every probe pays a fresh
// handshake.
//
// We serve a non-200 with a non-empty body: mirrorAdvertisesHead returns at
// the status check without reading the body itself, so the deferred drain is
// the *only* thing that consumes it. Counting StateNew transitions then
// distinguishes "drained → one reused connection" from "not drained → a new
// connection per call".
func TestMirrorAdvertisesHead_ReusesConnection(t *testing.T) {
	t.Parallel()

	// Larger than any plausible "small cap" someone might reintroduce, so a
	// capped drain would stop before EOF and fail this test too.
	body := strings.Repeat("x", 64<<10)

	var newConns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 503 = mirror reachable but not ready; the function returns at the
		// status check below http.StatusOK without touching the body.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(body)) //nolint:errcheck // test server write; failure surfaces as a client error
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	// Isolated client (not the package-global probeClient) so the idle pool
	// is private to this test and the assertion stays deterministic under
	// t.Parallel(). Keep-alives and idle pooling are on by default.
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{MaxIdleConns: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 90 * time.Second},
	}

	const probes = 5
	for range probes {
		ready, status := mirrorAdvertisesHead(context.Background(), client, srv.URL, "tok")
		require.False(t, ready)
		require.Equal(t, http.StatusServiceUnavailable, status)
	}

	require.Equal(t, int64(1), newConns.Load(),
		"expected the drained body to let all %d probes share one connection; got %d new connections (body not drained to EOF?)",
		probes, newConns.Load())
}

// pktLine encodes s as a git pkt-line (4-hex length prefix including the
// prefix itself), mirroring what a smart-HTTP server writes.
func pktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// uploadPackAdvertisement returns a minimal but valid git-upload-pack
// info/refs body: the "# service" banner, a flush, a HEAD line carrying the
// symref capability, a refs/heads/main line, and a trailing flush. It decodes
// to an AdvRefs whose HEAD resolves to refs/heads/main.
func uploadPackAdvertisement() string {
	const sha = "d9a69831082341eab799c062e10ad28b3204c08a"
	return pktLine("# service=git-upload-pack\n") +
		"0000" +
		pktLine(sha+" HEAD\x00symref=HEAD:refs/heads/main\n") +
		pktLine(sha+" refs/heads/main\n") +
		"0000"
}

// TestMirrorAdvertisesHead_FollowsNodeRedirect is the regression test for the
// infinite cloning-dots bug. The cluster front door 307-redirects info/refs to
// the node holding the mirror; git follows that to clone. The probe used to
// refuse all redirects (http.ErrUseLastResponse), so it saw the 307 as "not
// 200, not ready" and printed cloning-dots forever even after the clone had
// landed. With checkProbeRedirect the probe follows same-host redirects and
// reaches the advertisement, so it reports ready.
func TestMirrorAdvertisesHead_FollowsNodeRedirect(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/node/info/refs" {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = w.Write([]byte(uploadPackAdvertisement())) //nolint:errcheck // test server write
			return
		}
		// Front door: route to the backing node on the same host, the way the
		// real cluster front door 307s to bishop.<cluster-host>.
		http.Redirect(w, r, "/node/info/refs", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	// srv.Client() trusts the test cert; layer on the production redirect
	// policy so we exercise the real follow path.
	client := srv.Client()
	client.CheckRedirect = checkProbeRedirect

	ready, status := mirrorAdvertisesHead(context.Background(), client, srv.URL+"/info/refs", "tok")
	require.True(t, ready, "probe should follow the node redirect and see HEAD")
	require.Equal(t, http.StatusOK, status)
}

func TestCheckProbeRedirect(t *testing.T) {
	t.Parallel()

	orig := mustReq(t, "https://aws-us-east-2.entire.io/gh/o/r/info/refs")
	tests := []struct {
		name    string
		target  string
		via     int
		wantErr bool
	}{
		{name: "same host", target: "https://aws-us-east-2.entire.io/node/info/refs", via: 1},
		{name: "subdomain node", target: "https://bishop.aws-us-east-2.entire.io/gh/o/r/info/refs", via: 1},
		{name: "cross host leaks token", target: "https://evil.example.com/info/refs", via: 1, wantErr: true},
		{name: "sibling suffix trick", target: "https://aws-us-east-2.entire.io.evil.com/x", via: 1, wantErr: true},
		{name: "non-https", target: "http://bishop.aws-us-east-2.entire.io/x", via: 1, wantErr: true},
		{name: "too many hops", target: "https://bishop.aws-us-east-2.entire.io/x", via: maxProbeRedirects, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			via := make([]*http.Request, tt.via)
			via[0] = orig
			err := checkProbeRedirect(mustReq(t, tt.target), via)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func mustReq(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return &http.Request{URL: u}
}
