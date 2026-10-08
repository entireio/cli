package versioninfo

import (
	"net/http"

	"github.com/entireio/cli/internal/entireclient/httpclient"
)

// WrapTransport returns next wrapped so every request sent through it carries
// UserAgent(). A nil next means Go's default transport, matching how
// http.Client treats a nil Transport.
//
// Prefer this over setting the header at a call site whenever the *http.Client
// is handed to a helper that builds its own requests (clusterdiscovery's
// well-known fetch, coreapi's token exchange): those requests never pass
// through the caller's code, so only the transport can stamp them.
//
// Wrap innermost. When a client layers transports, this belongs at the base of
// the chain rather than on top: a transport above it may synthesize requests of
// its own and send them straight to its base, bypassing anything wrapped
// outside it. Stamping at the base catches every request that actually leaves
// the process.
//
// The User-Agent is resolved per request rather than captured here, because
// construction can happen before the version is known: main() calls Load() to
// recover it from the binary's build info, and any client built during package
// initialization — pluginHTTPClient is one — would otherwise pin
// "entire-cli/dev" for the life of the process on every build without ldflags
// (`go install ...@<version>`). Binding late makes construction order
// irrelevant.
func WrapTransport(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return userAgentTransport{next: next}
}

// userAgentTransport supplies the User-Agent later than construction and leaves
// the stamping itself to httpclient.UserAgentTransport, so the
// clone-before-mutate contract lives in exactly one place.
type userAgentTransport struct{ next http.RoundTripper }

func (t userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	//nolint:wrapcheck // thin passthrough; the wrapped transport owns the error semantics.
	return (&httpclient.UserAgentTransport{Next: t.next, UA: UserAgent()}).RoundTrip(req)
}
