package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// Users and agents paste trail URLs from the web app. Project trail commands
// take them anywhere a selector goes: the URL's project becomes --project and
// its path becomes the selector, so the trail resolves where it lives rather
// than in origin's namespace. Only selectors containing "://" are URLs; a
// scheme-less "entire.io/3915" is already a valid change selector for a
// repository named entire.io.

// entireTrailURLPath splits an http(s) Entire URL into its lowercased host and
// decoded path segments. Query and fragment are ignored.
func entireTrailURLPath(raw string) (string, []string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", nil, fmt.Errorf("invalid URL %q", raw)
	}
	if u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP {
		return "", nil, fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	if u.User != nil {
		return "", nil, errors.New("trail URLs must not carry credentials")
	}
	return strings.ToLower(u.Hostname()), strings.Split(strings.Trim(u.Path, "/"), "/"), nil
}

// projectTrailURLSelector is a trail page URL mapped to a selector and the
// project it lives in.
type projectTrailURLSelector struct {
	Project  string // <forge>/<project>, normalized by parseTrailProjectRef
	Selector string // <P> for a project trail, <repo>/<n> for a change
}

const projectTrailURLForms = "<forge>/<project>/-/trails/<P>, <forge>/<project>/-/trails/<P>/changes/<repo>/<n>, " +
	"<forge>/<project>/<repo>/-/trails/<P>/changes/<n>, or <forge>/<project>/<repo>/trails/<n>"

// parseProjectTrailURL maps the path of a trail page URL to a selector. The
// forms are the ones the web app links and `entire trail show` prints.
func parseProjectTrailURL(parts []string) (projectTrailURLSelector, error) {
	for _, p := range parts {
		if p == "" {
			return projectTrailURLSelector{}, errors.New("empty path segment")
		}
	}
	if len(parts) < 4 {
		return projectTrailURLSelector{}, errors.New("not a trail page")
	}
	host, project, err := parseTrailProjectRef(parts[0] + "/" + parts[1])
	if err != nil {
		return projectTrailURLSelector{}, err
	}
	ref := host + "/" + project
	rest := parts[2:]
	change := func(repo, number string) (projectTrailURLSelector, error) {
		sel, ok := parseProjectTrailChangeSelector(repo + "/" + number)
		if !ok {
			return projectTrailURLSelector{}, fmt.Errorf("invalid change %s/%s", repo, number)
		}
		return projectTrailURLSelector{Project: ref, Selector: sel.String()}, nil
	}
	switch {
	case len(rest) == 3 && rest[0] == "-" && rest[1] == groupTrails:
		if _, ok := parseTrailNumberSelector(rest[2]); !ok {
			return projectTrailURLSelector{}, fmt.Errorf("invalid trail number %q", rest[2])
		}
		return projectTrailURLSelector{Project: ref, Selector: rest[2]}, nil
	case len(rest) == 6 && rest[0] == "-" && rest[1] == groupTrails && rest[3] == "changes":
		return change(rest[4], rest[5])
	case len(rest) == 6 && rest[1] == "-" && rest[2] == groupTrails && rest[4] == "changes":
		return change(rest[0], rest[5])
	case len(rest) == 3 && rest[1] == groupTrails:
		return change(rest[0], rest[2])
	}
	return projectTrailURLSelector{}, errors.New("not a trail page")
}

// projectTrailWebBaseURL is the active login's web origin. Command tests
// replace it (serially); unit tests pass their own to the resolver.
var projectTrailWebBaseURL = auth.WebBaseURL

// projectTrailURLHostAllowed accepts URLs for the site the active login
// targets, so a URL from another site never silently resolves the same path
// on this one. entire.io is the web app's former host and stays an alias.
func projectTrailURLHostAllowed(host string, webBaseURL func() (string, error)) error {
	web, err := webBaseURL()
	if err != nil {
		return fmt.Errorf("resolve web origin: %w", err)
	}
	webURL, err := url.Parse(web)
	if err != nil {
		return fmt.Errorf("parse web origin %q: %w", web, err)
	}
	webHost := strings.ToLower(webURL.Hostname())
	if host == webHost {
		return nil
	}
	if webHost == "entire.com" && (host == "entire.io" || host == "www.entire.io" || host == "www.entire.com") {
		return nil
	}
	return fmt.Errorf("trail URL is for %s, but your login targets %s", host, webHost)
}

// resolveProjectTrailURLSelector maps one URL selector, or reports that raw
// is not a URL. webBaseURL is the active login's web origin.
func resolveProjectTrailURLSelector(raw string, webBaseURL func() (string, error)) (projectTrailURLSelector, bool, error) {
	if !strings.Contains(raw, "://") {
		return projectTrailURLSelector{}, false, nil
	}
	host, parts, err := entireTrailURLPath(raw)
	if err != nil {
		return projectTrailURLSelector{}, true, err
	}
	if !isEntireWebHost(host) {
		return projectTrailURLSelector{}, true, fmt.Errorf("%s is not an Entire trail URL", raw)
	}
	sel, err := parseProjectTrailURL(parts)
	if err != nil {
		return projectTrailURLSelector{}, true, fmt.Errorf("unrecognized Entire trail URL %s (%w); use a URL of the form https://<host>/%s", raw, err, projectTrailURLForms)
	}
	if err := projectTrailURLHostAllowed(host, webBaseURL); err != nil {
		return projectTrailURLSelector{}, true, err
	}
	return sel, true, nil
}

// normalizeProjectTrailURLArgs rewrites URL selectors, positional or --trail,
// to the selectors they name, and sets --project to the URL's project. It runs
// as the project trail tree's persistent pre-run, so every subcommand and every
// retry hint built from the parsed args sees the rewritten selector.
func normalizeProjectTrailURLArgs(cmd *cobra.Command, args []string) error {
	var project string
	adopt := func(raw string) (string, error) {
		sel, isURL, err := resolveProjectTrailURLSelector(raw, projectTrailWebBaseURL)
		if err != nil || !isURL {
			return raw, err
		}
		if project != "" && project != sel.Project {
			return "", fmt.Errorf("trail URLs name different projects (%s and %s)", project, sel.Project)
		}
		project = sel.Project
		return sel.Selector, nil
	}
	for i, arg := range args {
		rewritten, err := adopt(arg)
		if err != nil {
			return err
		}
		args[i] = rewritten
	}
	if flag := cmd.Flags().Lookup("trail"); flag != nil && flag.Value.String() != "" {
		rewritten, err := adopt(flag.Value.String())
		if err != nil {
			return err
		}
		if rewritten != flag.Value.String() {
			if err := cmd.Flags().Set("trail", rewritten); err != nil {
				return fmt.Errorf("set --trail from URL: %w", err)
			}
		}
	}
	if project == "" {
		return nil
	}
	if given := projectTrailProjectFlag(cmd); given != "" {
		host, name, err := parseTrailProjectRef(given)
		if err != nil {
			return err
		}
		if host+"/"+name != project {
			return fmt.Errorf("the trail URL is in project %s, but --project is %s", project, given)
		}
		return nil
	}
	if err := cmd.Flags().Set("project", project); err != nil {
		return fmt.Errorf("set --project from trail URL: %w", err)
	}
	return nil
}
