package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/internal/coreapi"
)

// mirrorStatusUnknown is the status shown when the mirror's live state can't be
// read — either the caller isn't logged in, or a best-effort lookup failed.
const mirrorStatusUnknown = "unknown"

// mirrorStatusTimeout bounds the best-effort control-plane lookup so `entire
// status` can never hang on a slow network. On timeout the status degrades to
// "unknown" like any other lookup failure.
const mirrorStatusTimeout = 4 * time.Second

// originRemote is git's default remote name, preferred when scanning for the
// mirror remote so the reported remote matches what "use the mirror" points at.
const originRemote = "origin"

// mirrorClone describes a clone whose git remote points at an Entire mirror,
// detected purely from local git config (no network, no auth).
type mirrorClone struct {
	Remote  string // git remote name pointing at the mirror
	Cluster string // Entire cluster host serving the mirror
	Owner   string // forge repo owner
	Repo    string // forge repo name
	URL     string // the entire:// clone URL as configured
}

// mirrorJSON is the `mirror` object in `entire status --json`. Omitted entirely
// when the clone does not point at a mirror.
type mirrorJSON struct {
	Remote   string `json:"remote"`
	Cluster  string `json:"cluster"`
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	URL      string `json:"url"`
	Status   string `json:"status"`
	LoggedIn bool   `json:"logged_in"`
}

// detectMirrorClone inspects the repo's git remotes for an entire:// URL and, if
// found, returns what can be known offline about the mirror this clone points
// at. `origin` is preferred; otherwise remotes are considered in name order so
// the reported remote is stable across runs. Returns nil when no remote points
// at a mirror (the common case for an ordinary clone).
func detectMirrorClone(ctx context.Context, repoRoot string) *mirrorClone {
	for _, name := range orderedRemoteNames(ctx, repoRoot) {
		rawURL, gerr := gitremote.GetRemoteURLInDir(ctx, repoRoot, name)
		if gerr != nil {
			continue
		}
		info, perr := gitremote.ParseURL(rawURL)
		if perr != nil || info.Protocol != gitremote.ProtocolEntire {
			continue
		}
		return &mirrorClone{
			Remote:  name,
			Cluster: info.Host,
			Owner:   info.Owner,
			Repo:    info.Repo,
			URL:     rawURL,
		}
	}
	return nil
}

// mirrorableForgeHost returns the git host this clone fetches from directly
// (e.g. "github.com") when that host is one Entire can mirror — the case where
// pointing the clone at a mirror is a meaningful next step. ok is false when no
// remote names a mirrorable forge (an already-mirrored entire:// remote, or a
// host mirrors don't support). The host is read straight from the remote URL so
// the hint names the real provider rather than a hardcoded one, and gating on
// gitremote.IsSupportedForge means the trigger widens automatically if more
// forges become mirrorable. This is a purely local check: it cannot tell
// whether a mirror already exists server-side, which is why the hint points at
// `mirror use` (which resolves that, or tells the user to `mirror create` when
// there is none) rather than asserting either way.
func mirrorableForgeHost(ctx context.Context, repoRoot string) (host string, ok bool) {
	for _, name := range orderedRemoteNames(ctx, repoRoot) {
		rawURL, gerr := gitremote.GetRemoteURLInDir(ctx, repoRoot, name)
		if gerr != nil {
			continue
		}
		info, perr := gitremote.ParseURL(rawURL)
		if perr != nil {
			continue
		}
		if info.Protocol != gitremote.ProtocolEntire && gitremote.IsSupportedForge(info.Forge) {
			return info.Host, true
		}
	}
	return "", false
}

// orderedRemoteNames lists the repo's git remotes with origin first, then the
// rest by name, so both the mirror scan and the hint report a stable,
// origin-preferring result. Returns nil on any error (treated as "no remotes").
func orderedRemoteNames(ctx context.Context, repoRoot string) []string {
	remotes, err := gitremote.ListRemotesInDir(ctx, repoRoot)
	if err != nil || len(remotes) == 0 {
		return nil
	}
	var hasOrigin bool
	rest := make([]string, 0, len(remotes))
	for _, name := range remotes {
		if name == originRemote {
			hasOrigin = true
			continue
		}
		rest = append(rest, name)
	}
	sort.Strings(rest)
	if hasOrigin {
		return append([]string{originRemote}, rest...)
	}
	return rest
}

// statusLoggedIn reports whether a usable control-plane credential exists,
// without prompting or touching the network: an ENTIRE_TOKEN env token, or an
// active login context. It gates whether `entire status` attempts a live mirror
// lookup at all.
func statusLoggedIn() bool {
	if strings.TrimSpace(os.Getenv(auth.EnvTokenVar)) != "" {
		return true
	}
	_, err := auth.ResolveControlPlaneTarget()
	return err == nil
}

// resolveMirrorStatus enriches a locally-detected mirror clone with its live
// server-side status. A package var so tests inject deterministic results
// without network or auth. When the caller is logged out it returns
// ("unknown", false) with no network call; when logged in it does a best-effort,
// time-bounded lookup, degrading to ("unknown", true) on any failure.
var resolveMirrorStatus = func(ctx context.Context, m *mirrorClone) (status string, loggedIn bool) {
	if !statusLoggedIn() {
		return mirrorStatusUnknown, false
	}
	fctx, cancel := context.WithTimeout(ctx, mirrorStatusTimeout)
	defer cancel()
	st, err := fetchMirrorStatus(fctx, m.Cluster, m.Owner, m.Repo)
	if err != nil {
		return mirrorStatusUnknown, true
	}
	return st, true
}

// fetchMirrorStatus dials the core fronting clusterHost and reads the mirror's
// clone-lifecycle status (processing / ready / failed / suspended). The cluster
// core is dialed rather than the active context's so the lookup resolves even
// when the mirror lives in a federation other than the active login. Best-effort:
// any failure is the caller's cue to fall back to "unknown".
func fetchMirrorStatus(ctx context.Context, clusterHost, owner, repo string) (string, error) {
	c, err := clusterCoreClient(ctx, clusterHost)
	if err != nil {
		return "", err
	}
	mirrorID, err := resolveMirrorRef(ctx, c, mirrorCloneURL(clusterHost, owner, repo))
	if err != nil {
		return "", err
	}
	m, err := c.GetMirror(ctx, coreapi.GetMirrorParams{MirrorId: mirrorID})
	if err != nil {
		return "", err
	}
	if st, ok := m.Status.Get(); ok {
		return string(st), nil
	}
	return mirrorStatusUnknown, nil
}

// formatMirrorStatusLine renders the one-line mirror summary for the human
// status output.
func formatMirrorStatusLine(m *mirrorClone, status string, loggedIn bool, sty statusStyles) string {
	var state string
	switch {
	case status == mirrorStatusUnknown && !loggedIn:
		state = "status unknown — run `entire login`"
	case status == mirrorStatusUnknown:
		state = "status unknown"
	default:
		state = status
	}
	return fmt.Sprintf("%s Mirror: %s · %s  %s",
		sty.render(sty.dim, "⇄"),
		sty.render(sty.bold, m.Cluster),
		state,
		sty.render(sty.dim, fmt.Sprintf("(remote: %s)", m.Remote)),
	)
}

// formatMirrorHint renders the one-line hint shown when the clone fetches from
// the forge directly. It describes only what is locally true — that this clone
// isn't using a mirror — and points at `mirror use`, which repoints the clone
// at an existing mirror or, when none exists, tells the user to create one.
func formatMirrorHint(host string, sty statusStyles) string {
	return fmt.Sprintf("%s This clone pulls directly from %s, not through an Entire mirror. Switch it with:\n    %s",
		sty.render(sty.dim, "⇄"),
		host,
		sty.render(sty.bold, "entire repo mirror use"),
	)
}

// writeMirrorStatus renders the mirror line when this clone points at a mirror,
// or a hint on how to switch to one when it targets a GitHub forge directly.
// Both paths are decided from local git config; the live-status lookup only
// fires for mirror-using repos, and the hint triggers no network at all.
func writeMirrorStatus(ctx context.Context, w io.Writer, repoRoot string, sty statusStyles) {
	if m := detectMirrorClone(ctx, repoRoot); m != nil {
		status, loggedIn := resolveMirrorStatus(ctx, m)
		fmt.Fprintln(w, formatMirrorStatusLine(m, status, loggedIn, sty))
		return
	}
	if host, ok := mirrorableForgeHost(ctx, repoRoot); ok {
		fmt.Fprintln(w, formatMirrorHint(host, sty))
	}
}

// mirrorStatusJSON builds the `mirror` field for `entire status --json`, or nil
// when this clone does not point at a mirror.
func mirrorStatusJSON(ctx context.Context, repoRoot string) *mirrorJSON {
	m := detectMirrorClone(ctx, repoRoot)
	if m == nil {
		return nil
	}
	status, loggedIn := resolveMirrorStatus(ctx, m)
	return &mirrorJSON{
		Remote:   m.Remote,
		Cluster:  m.Cluster,
		Owner:    m.Owner,
		Repo:     m.Repo,
		URL:      m.URL,
		Status:   status,
		LoggedIn: loggedIn,
	}
}
