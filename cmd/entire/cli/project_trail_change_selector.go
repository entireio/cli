package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// projectTrailChangeSelector is the <repo>/<number> form a change's web URL
// ends in (…/trails/2074/changes/cli/1503): one repository's branch work,
// addressed by its repository-local number inside the selected project.
// Only project mode accepts it. In legacy mode a selector containing a slash
// is a branch name, and a bare number already means repository-local work.
type projectTrailChangeSelector struct {
	Repo   string
	Number int
}

func (s projectTrailChangeSelector) String() string {
	return s.Repo + "/" + strconv.Itoa(s.Number)
}

// parseProjectTrailChangeSelector accepts exactly <repo>/<digits>. Anything
// else is not a change selector and falls through to trail-selector checks.
func parseProjectTrailChangeSelector(selector string) (projectTrailChangeSelector, bool) {
	repo, number, ok := strings.Cut(strings.TrimSpace(selector), "/")
	if !ok || !validTrailChangeRepoName(repo) || number == "" || strings.Trim(number, "0123456789") != "" {
		return projectTrailChangeSelector{}, false
	}
	n, err := strconv.Atoi(number)
	if err != nil || n <= 0 {
		return projectTrailChangeSelector{}, false
	}
	return projectTrailChangeSelector{Repo: repo, Number: n}, true
}

func validTrailChangeRepoName(repo string) bool {
	if repo == "" || repo == "." || repo == ".." {
		return false
	}
	for _, c := range repo {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

// projectTrailChange is a change selector resolved to its repository and work.
type projectTrailChange struct {
	Host, Owner, Repo string
	Client            *api.Client
	RepoID            string
	BasePath          string
	Work              *api.TrailResource
}

// resolveProjectTrailChange looks the change up by its repository-local
// number, in the project named by --project or, without it, origin's owner.
// localOnly callers (checkout, resume) act on this clone, so the change must
// belong to origin's repository. An explicit --repo must name the same one.
func resolveProjectTrailChange(cmd *cobra.Command, sel projectTrailChangeSelector, localOnly bool) (*projectTrailChange, error) {
	ctx := cmd.Context()
	var host, owner string
	if ref := projectTrailProjectFlag(cmd); ref != "" {
		var err error
		if host, owner, err = parseTrailProjectRef(ref); err != nil {
			return nil, err
		}
	}
	if localOnly || host == "" {
		originHost, originOwner, originRepo, err := resolveTrailRemote(ctx)
		if err != nil {
			return nil, err
		}
		if localOnly && (!strings.EqualFold(originRepo, sel.Repo) || (host != "" && (host != originHost || !strings.EqualFold(owner, originOwner)))) {
			return nil, fmt.Errorf("%s is not in this clone's repository (%s/%s/%s); run this from a clone of %s", sel, originHost, originOwner, originRepo, sel.Repo)
		}
		host, owner = originHost, originOwner
	}
	if repoFlag := trailRepoFlag(cmd); repoFlag != "" && !localOnly {
		flagHost, flagOwner, flagRepo, err := parseTrailRepoArg(repoFlag)
		if err != nil {
			return nil, err
		}
		if flagHost != host || !strings.EqualFold(flagOwner, owner) || !strings.EqualFold(flagRepo, sel.Repo) {
			return nil, fmt.Errorf("--repo %s does not match %s in %s/%s", repoFlag, sel, host, owner)
		}
	}
	client, repoID, err := newTrailAPIClient(ctx, trailInsecureHTTP(cmd), host, owner, sel.Repo)
	if err != nil {
		return nil, err
	}
	base, err := trailRepoBasePath(host, owner, sel.Repo, repoID)
	if err != nil {
		return nil, err
	}
	work, err := findTrailByNumberAtPath(ctx, client, base, sel.Number)
	if err != nil {
		return nil, err
	}
	if work == nil {
		return nil, fmt.Errorf("no change %s found in %s/%s", sel, host, owner)
	}
	if !looksLikeULID(work.ID) {
		return nil, fmt.Errorf("change %s has no valid backing identity", sel)
	}
	return &projectTrailChange{Host: host, Owner: owner, Repo: sel.Repo, Client: client, RepoID: repoID, BasePath: base, Work: work}, nil
}

// errChangeSelectorWithBranch rejects naming the branch work twice.
var errChangeSelectorWithBranch = errors.New("pass <repo>/<number> or --branch, not both")
