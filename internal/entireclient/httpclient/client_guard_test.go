package httpclient_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const clientBypassPattern = `&[[:space:]]*http\.Client[[:space:]]*\{|http\.Client\{|new[[:space:]]*\([[:space:]]*http\.Client|` +
	`(^|[;{])[[:space:]]*(var[[:space:]]+)?[[:alnum:]_,[:space:]]+[[:space:]]+http\.Client([[:space:];=]|$)|` +
	`http\.(DefaultClient|Get|Head|Post|PostForm)([^[:alnum:]_]|$)`

// TestHTTPClientsUseSecureConstructor catches client allocations, zero-value
// declarations and package-level helpers that bypass the shared constructor.
func TestHTTPClientsUseSecureConstructor(t *testing.T) {
	t.Parallel()
	root, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}
	out := testutil.GitGrepGuard(t, root, "-l", "--extended-regexp", "--",
		clientBypassPattern, "--", ":(glob)**/*.go", ":(exclude,glob)**/*_test.go")
	// Explicit exceptions, not a blanket exemption for generated code:
	// - generated coreapi config has a default, overridden by CLI constructors;
	// - discovery copies its fallback client and forbids every redirect;
	// - the constructor owns the sole allocation;
	// - the wrapper copies DefaultClient and installs the secure policy.
	require.Equal(t, []string{
		"internal/coreapi/oas_cfg_gen.go",
		"internal/entireclient/clusterdiscovery/discovery.go",
		"internal/entireclient/httpclient/client.go",
		"internal/entireclient/httpclient/redirect.go",
	}, strings.Fields(out), "use httpclient.New; audit any new bypass (zero matches also fails)")
}

const clientMutationPattern = `\.(CheckRedirect|Transport)[[:space:]]*=[^=]`

// TestHTTPClientsAreNotRewiredAfterConstruction catches assignments that
// replace a constructed client's redirect callback or transport. Either can
// defeat the secure constructor: a nil CheckRedirect drops the policy, and a
// transport that rewrites the URL hides a downgrade from it.
func TestHTTPClientsAreNotRewiredAfterConstruction(t *testing.T) {
	t.Parallel()
	root, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}
	out := testutil.GitGrepGuard(t, root, "-l", "--extended-regexp", "--",
		clientMutationPattern, "--", ":(glob)**/*.go", ":(exclude,glob)**/*_test.go")
	// Explicit exceptions:
	// - plain-HTTP data-API discovery rewrites only its own data origin's host;
	// - discovery copies its fallback client and forbids every redirect;
	// - the wrapper installs the secure policy on its copy.
	require.Equal(t, []string{
		"cmd/entire/cli/auth/data_api.go",
		"internal/entireclient/clusterdiscovery/discovery.go",
		"internal/entireclient/httpclient/redirect.go",
	}, strings.Fields(out), "do not rewire a constructed client; audit any new assignment (zero matches also fails)")
}

func TestClientMutationPattern(t *testing.T) {
	t.Parallel()
	pattern := regexp.MustCompile(clientMutationPattern)
	for _, source := range []string{"c.CheckRedirect = nil", "client.Transport = rt", "p.client.Transport=rt"} {
		require.True(t, pattern.MatchString(source), source)
	}
	for _, source := range []string{"Transport: rt,", "if c.Transport == nil {", "CheckRedirect: policy,"} {
		require.False(t, pattern.MatchString(source), source)
	}
}

func TestClientBypassPattern(t *testing.T) {
	t.Parallel()
	pattern := regexp.MustCompile(clientBypassPattern)
	require.False(t, pattern.MatchString("// checkRedirect is the http.Client CheckRedirect hook."))
	for _, source := range []string{
		"c := &http.Client{}", "c := http.Client{}", "c := new(http.Client)",
		"var c http.Client", "var c, d http.Client", " c http.Client", // grouped var or embedded value field
		"http.Get(url)", "http.Head(url)", "http.Post(url, kind, body)", "http.PostForm(url, values)",
		"c := http.DefaultClient", "http.DefaultClient.Do(req)",
	} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			require.True(t, pattern.MatchString(source))
		})
	}
}
