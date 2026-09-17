package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/internal/entireclient/contexts"
)

// reservedHostSuffixes mirrors regional.reservedHostSuffixes in entiredb
// (core/regional/reserved_host.go). Both must change together; the server
// re-applies its copy, so drift costs a 400 the CLI renders, never a wrong
// declaration.
var reservedHostSuffixes = []string{"local", "localdomain", "localhost", "internal", "lan", "home", "home.arpa", "test", "example", "invalid"}

// normalizeDeclaredEmail is regional.NormalizeDeclaredEmail's twin: lowercase,
// trim, strip one trailing dot.
func normalizeDeclaredEmail(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// reservedHostEmail reports whether the address's host is one nobody owns —
// the shape git synthesizes when user.email is unset. Syntactic only.
func reservedHostEmail(email string) bool {
	email = normalizeDeclaredEmail(email)
	at := strings.IndexByte(email, '@')
	if at <= 0 || strings.Count(email, "@") != 1 {
		return false
	}
	host := email[at+1:]
	if host == "" {
		return false
	}
	for _, r := range host {
		if r >= 0x80 {
			return false
		}
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || strings.HasPrefix(label, "xn--") {
			return false
		}
	}
	for _, s := range reservedHostSuffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}

// normalizeOSUsername lowercases and strips a Windows "DOMAIN\" prefix so it
// compares against git's synthesized local part.
func normalizeOSUsername(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndexByte(name, '\\'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// defaultOSUsername is the production value of unattributedAuthorsDeps.username.
// "" when os/user fails → nothing is offered (spec: no user to match).
func defaultOSUsername() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return normalizeOSUsername(u.Username)
}

// authorsFromShortlog extracts the <email> from each `git shortlog -se` line.
func authorsFromShortlog(out []byte) []string {
	var emails []string
	for _, line := range strings.Split(string(out), "\n") {
		lt, gt := strings.LastIndexByte(line, '<'), strings.LastIndexByte(line, '>')
		if lt < 0 || gt <= lt {
			continue
		}
		if email := line[lt+1 : gt]; email != "" {
			emails = append(emails, email)
		}
	}
	return emails
}

// filterCandidateAuthors is the authorship gate (spec: "Doctor offers only
// addresses whose local part is the current OS username"). It returns the
// normalized, deduplicated reserved-host addresses whose local part equals
// username, in first-seen order. An empty username yields nothing.
func filterCandidateAuthors(authors []string, username string) []string {
	username = normalizeOSUsername(username)
	if username == "" {
		return nil
	}
	var out []string
	for _, a := range authors {
		if !reservedHostEmail(a) {
			continue
		}
		n := normalizeDeclaredEmail(a)
		// reservedHostEmail guarantees exactly one '@' at index > 0.
		if n[:strings.IndexByte(n, '@')] != username {
			continue
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

type unattributedAuthor struct {
	Email string `json:"email"`
	Count int    `json:"count"`
}

type unattributedAuthorsWire struct {
	Authors []struct {
		Email string `json:"email"`
		N     int    `json:"unattributedCommits"`
	} `json:"authors"`
}

// nonZero drops addresses the cell holds no unattributed commits for: an
// address present locally but not broken on entire.io is not offered.
func (w unattributedAuthorsWire) nonZero() []unattributedAuthor {
	var out []unattributedAuthor
	for _, a := range w.Authors {
		if a.N > 0 {
			out = append(out, unattributedAuthor{Email: a.Email, Count: a.N})
		}
	}
	return out
}

// localCandidateAuthors lists the reserved-host author addresses in this
// repo's history that pass the OS-username gate. Never errors: a git failure
// (including the output cap) is logged and treated as "no candidates" — local
// detection must not fail doctor or slow status. shortlog applies .mailmap
// unconditionally; a mailmap that rewrites a reserved-host address hides it
// here while the cell still holds the raw author — accepted. The 1 MiB output
// cap admits roughly 15-20k distinct authors; beyond that, detection silently
// degrades to "no candidates" (debug-logged) — deliberate, since a repo with
// that many distinct reserved-host authors is not one this feature is aimed at.
func localCandidateAuthors(ctx context.Context, username string) []string {
	// Walk the user's refs only. --all would also walk Entire's own refs —
	// refs/entire/checkpoints/* and refs/entire/policies/* (git-refs store,
	// potentially thousands of refs), the entire/checkpoints/v1 branch and
	// entire/<sha>-<hash> shadow branches (git-branch store), and their
	// remote-tracking copies. Checkpoint commits are written with the USER'S
	// git identity (checkpoint.GetGitAuthorFromRepo; fallback
	// "Unknown <unknown@local>"), so a user whose user.email was unset has
	// checkpoint commits under the very reserved-host address this detects —
	// and the cell never counts checkpoint commits, only code commits.
	// Excluding these refs is therefore correctness, not just speed.
	// --exclude must precede --all to apply to it.
	out, err := runGitQuiet(ctx, 1<<20, "shortlog", "-se",
		"--exclude=refs/entire/*",
		"--exclude=refs/heads/entire/*",
		"--exclude=refs/remotes/*/entire/*",
		"--all")
	if err != nil {
		logging.Debug(ctx, "unattributed authors: git shortlog failed; treating as none", "error", err)
		return nil
	}
	return filterCandidateAuthors(authorsFromShortlog(out), username)
}

const maxUnattributedAuthorEmails = 50 // the cell rejects more with 422

func fetchUnattributedAuthors(ctx context.Context, client *api.Client, repoID string, emails []string) ([]unattributedAuthor, error) {
	if len(emails) == 0 {
		return nil, nil
	}
	if len(emails) > maxUnattributedAuthorEmails {
		logging.Debug(ctx, "unattributed authors: truncating candidates to the cell's cap", "candidates", len(emails))
		emails = emails[:maxUnattributedAuthorEmails]
	}
	path := "/api/v1/repos/" + url.PathEscape(repoID) + "/authors/unattributed"
	resp, err := client.Post(ctx, path, map[string]any{"emails": emails})
	if err != nil {
		return nil, fmt.Errorf("cell: unattributed authors: %w", err)
	}
	defer resp.Body.Close()
	if err := api.CheckResponse(resp); err != nil {
		return nil, fmt.Errorf("cell: unattributed authors: %w", err)
	}
	var wire unattributedAuthorsWire
	if err := api.DecodeJSON(resp, &wire); err != nil {
		return nil, fmt.Errorf("cell: unattributed authors: %w", err)
	}
	return wire.nonZero(), nil
}

// shortNetErr renders a network/auth failure as the one-line reason doctor
// shows after "skipped (": a deadline is named as such; otherwise the first
// line of the error, capped at 80 runes.
func shortNetErr(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out reaching Entire"
	}
	msg, _, _ := strings.Cut(err.Error(), "\n")
	if r := []rune(msg); len(r) > 80 {
		return string(r[:79]) + "…"
	}
	return msg
}

// unattributedAuthorsDeps is the dependency seam detectUnattributedAuthors
// composes: doctor and status both call detectUnattributedAuthors(ctx,
// defaultUnattributedAuthorsDeps(...)) in production, while tests inject every
// network/auth edge as a func field instead of touching a package-level
// variable (the pattern of identityProfileDependencies, setup_identity.go).
type unattributedAuthorsDeps struct {
	username         func() string
	localAuthors     func(ctx context.Context, username string) []string
	lookupEnv        func(string) (string, bool)
	activeContext    func() (*contexts.Context, bool, error)
	commonDir        func(ctx context.Context) (string, error)
	originTip        func(ctx context.Context) string
	readCache        func(commonDir, tip, repoID string, now time.Time) (cachedDetection, bool)
	writeCache       func(commonDir string, c cachedDetection) error
	resolvePlacement func(ctx context.Context) (repoCellPlacement, error)
	cellClient       func(ctx context.Context, target *auth.CellTarget) (*api.Client, error)
	fetch            func(ctx context.Context, c *api.Client, repoID string, emails []string) ([]unattributedAuthor, error)
	now              func() time.Time
	networkTimeout   time.Duration
}

func defaultUnattributedAuthorsDeps(insecure bool, networkTimeout time.Duration) unattributedAuthorsDeps {
	return unattributedAuthorsDeps{
		username:      defaultOSUsername,
		localAuthors:  localCandidateAuthors,
		lookupEnv:     os.LookupEnv,
		activeContext: auth.ActiveContext,
		commonDir:     strategy.GetGitCommonDir,
		originTip:     originDefaultTip,
		readCache:     readUnattributedAuthorsCache,
		writeCache:    writeUnattributedAuthorsCache,
		resolvePlacement: func(ctx context.Context) (repoCellPlacement, error) {
			_, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
			if err != nil {
				return repoCellPlacement{}, fmt.Errorf("resolve repo from origin: %w", err)
			}
			return resolveRepoCellPlacement(ctx, owner, repo)
		},
		cellClient: func(ctx context.Context, t *auth.CellTarget) (*api.Client, error) {
			// Bare return, no nolint: wrapcheck does not analyse returns inside
			// function literals, so a //nolint:wrapcheck here is unused and
			// nolintlint fails the build.
			return auth.NewEntireAPICellClient(ctx, insecure, t)
		},
		fetch:          fetchUnattributedAuthors,
		now:            time.Now,
		networkTimeout: networkTimeout,
	}
}

// detectionOutcome is what doctor and status render from.
type detectionOutcome struct {
	Candidates []string             // local, gated; present even when logged out
	LoggedIn   bool                 // ENTIRE_TOKEN set or an active context
	RepoID     string               // placement id; set whenever placement resolved (so whenever Authors is)
	Authors    []unattributedAuthor // Entire's counts (>0 only); nil when not fetched
	Skipped    string               // one-line reason the cell step was skipped ("" = not skipped)
}

// detectUnattributedAuthors never fails: every network/auth problem lands in
// Skipped, every local problem in an empty Candidates. Order: local
// candidates → logged-in → cache (by tip; stored RepoID comes back with it)
// → placement → cell → cache write.
func detectUnattributedAuthors(ctx context.Context, d unattributedAuthorsDeps) detectionOutcome {
	username := d.username()
	if username == "" {
		logging.Debug(ctx, "unattributed authors: no OS username; offering nothing")
		return detectionOutcome{}
	}
	out := detectionOutcome{Candidates: d.localAuthors(ctx, username)}
	if len(out.Candidates) == 0 {
		return out
	}
	if _, ok := d.lookupEnv(auth.EnvTokenVar); ok {
		out.LoggedIn = true
	} else if _, ok, err := d.activeContext(); err != nil {
		logging.Debug(ctx, "unattributed authors: active context unavailable; treating as logged out", "error", err)
	} else {
		out.LoggedIn = ok
	}
	if !out.LoggedIn {
		return out
	}

	commonDir, cdErr := d.commonDir(ctx)
	tip := d.originTip(ctx)
	if cdErr != nil {
		logging.Debug(ctx, "unattributed authors: git common dir unavailable; cache disabled", "error", cdErr)
	} else if c, hit := d.readCache(commonDir, tip, "", d.now()); hit {
		out.RepoID, out.Authors, out.Skipped = c.RepoID, c.Authors, c.Skipped
		return out
	}

	nctx, cancel := context.WithTimeout(ctx, d.networkTimeout)
	defer cancel()
	placement, err := d.resolvePlacement(nctx)
	switch {
	case err != nil:
		out.Skipped = shortNetErr(err)
	default:
		out.RepoID = placement.RepoID
		client, err := d.cellClient(nctx, placement.Target)
		if err != nil {
			out.Skipped = shortNetErr(err)
			break
		}
		authors, err := d.fetch(nctx, client, placement.RepoID, out.Candidates)
		if err != nil {
			out.Skipped = shortNetErr(err)
			break
		}
		out.Authors = authors
	}
	if cdErr == nil && tip != "" {
		c := cachedDetection{Tip: tip, RepoID: out.RepoID, Authors: out.Authors, Skipped: out.Skipped, FetchedAt: d.now()}
		if err := d.writeCache(commonDir, c); err != nil {
			logging.Debug(ctx, "unattributed authors: cache write failed", "error", err)
		}
	}
	return out
}
