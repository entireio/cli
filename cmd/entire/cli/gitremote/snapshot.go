package gitremote

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// RemoteURLs is where one remote fetches from and pushes to.
//
// Fetch is remote.<name>.url (the first, when several are configured) — the same
// value GetRemoteURL reports. Push is every destination a push delivers to, in
// git's order — the same set GetPushURLs reports. Either may be empty: a remote
// configured with only a pushurl has no fetch URL, and `git remote -v` prints an
// empty field for it.
type RemoteURLs struct {
	Fetch string
	Push  []string
}

// Snapshot is every remote in a repository and its URLs, read in one pass.
//
// It exists because the URLs of one remote are needed to answer questions about
// another — the checkpoint-remote ownership test compares a remote's push URLs
// against origin's — so asking git per remote means asking the same questions
// several times per command, and getting answers that can disagree when a
// subprocess fails in one call and succeeds in the next. One `git remote -v`
// carries everything those per-remote reads were fetching, so callers that need
// more than one answer should take a Snapshot and derive the rest from it.
//
// A Snapshot is a point-in-time read. That is the point: every claim derived
// from it describes the same moment. Code that must act on current state (the
// pre-push hook deciding where this push goes) should read live instead.
type Snapshot struct {
	// order is remote names, sorted, so callers render them deterministically.
	order []string
	urls  map[string]RemoteURLs
}

// LoadSnapshot reads every remote and its URLs from dir with one `git remote -v`.
//
// `git remote -v` is used rather than a `git remote get-url` per remote because
// it already applies git's pushurl-replaces-url rule and lists every push URL in
// order, so N+1 subprocesses collapse to one. It also applies url.<base>.insteadOf
// rewrites exactly as `git remote get-url` does (verified against git 2.55), so
// the values here are the values those calls would return.
//
// An empty dir runs in the process working directory. A repository with no
// remotes is a successful empty Snapshot, not an error — callers distinguish
// "no remotes" from "could not read" by the error.
func LoadSnapshot(ctx context.Context, dir string) (Snapshot, error) {
	cmd := exec.CommandContext(ctx, "git", "remote", "-v")
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return Snapshot{}, fmt.Errorf("list git remotes: %w", err)
	}
	return parseRemoteVerbose(string(out)), nil
}

// parseRemoteVerbose turns `git remote -v` output into a Snapshot.
//
// Each line is "<name>\t<url> (fetch)" or "<name>\t<url> (push)". A remote with
// no fetch URL still gets a line, with an empty URL field.
func parseRemoteVerbose(out string) Snapshot {
	urls := make(map[string]RemoteURLs)
	for line := range strings.SplitSeq(out, "\n") {
		name, rest, found := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		entry := urls[name]
		switch {
		case strings.HasSuffix(rest, "(push)"):
			if url := strings.TrimSpace(strings.TrimSuffix(rest, "(push)")); url != "" {
				entry.Push = append(entry.Push, url)
			}
		case strings.HasSuffix(rest, "(fetch)"):
			// First wins: git prints one fetch line per remote, but a
			// malformed read must not silently prefer a later value.
			if entry.Fetch == "" {
				entry.Fetch = strings.TrimSpace(strings.TrimSuffix(rest, "(fetch)"))
			}
		default:
			// Not a URL line; the remote itself is still known to exist.
		}
		urls[name] = entry
	}

	order := make([]string, 0, len(urls))
	for name := range urls {
		order = append(order, name)
	}
	sort.Strings(order)
	return Snapshot{order: order, urls: urls}
}

// Names lists the configured remotes in sorted order.
func (s Snapshot) Names() []string { return s.order }

// Get returns a remote's URLs. The second result reports whether the remote is
// configured at all, which an empty RemoteURLs cannot express — a remote with
// only a pushurl has no fetch URL, and one whose url is empty has neither.
func (s Snapshot) Get(name string) (RemoteURLs, bool) {
	u, ok := s.urls[name]
	return u, ok
}

// OriginURL is origin's fetch URL, or empty when there is no origin. It is
// singled out because the checkpoint-remote ownership test asks "which project
// is this?" of origin specifically, whatever remote is being pushed.
func (s Snapshot) OriginURL() string {
	u, ok := s.urls[originRemote]
	if !ok {
		return ""
	}
	return u.Fetch
}

// originRemote is the conventional name for the remote identifying the project.
const originRemote = "origin"
