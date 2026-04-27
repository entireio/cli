package dispatch

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var githubRepoSlugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._-]+$`)

func ResolveOptions(
	flagLocal bool,
	flagSince string,
	flagUntil string,
	flagAllBranches bool,
	flagRepos []string,
	flagVoice string,
	flagInsecureHTTPAuth bool,
	currentBranch func() (string, error),
) (Options, error) {
	flagRepos = normalizeScopeValues(flagRepos)

	if flagLocal && len(flagRepos) > 0 {
		return Options{}, errors.New("--repos cannot be used with --local")
	}
	if !flagLocal && flagAllBranches {
		return Options{}, errors.New("--all-branches only applies to --local (cloud dispatch uses each repo's default branch)")
	}
	if !flagLocal {
		if err := validateRepoSlugs(flagRepos); err != nil {
			return Options{}, err
		}
	}
	if !flagLocal && len(flagRepos) > CloudRepoLimit {
		return Options{}, fmt.Errorf("--repos supports at most %d repos per dispatch", CloudRepoLimit)
	}

	mode := ModeServer
	if flagLocal {
		mode = ModeLocal
	}

	var branches []string
	implicitCurrentBranch := false
	if flagLocal && !flagAllBranches {
		currentBranchName, err := currentBranch()
		if err != nil {
			return Options{}, err
		}
		branches = []string{currentBranchName}
		implicitCurrentBranch = true
	}

	return Options{
		Mode:                  mode,
		RepoPaths:             flagRepos,
		Since:                 flagSince,
		Until:                 flagUntil,
		Branches:              branches,
		AllBranches:           flagAllBranches,
		ImplicitCurrentBranch: implicitCurrentBranch,
		Voice:                 flagVoice,
		InsecureHTTPAuth:      flagInsecureHTTPAuth,
	}, nil
}

func normalizeScopeValues(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized
}

func validateRepoSlugs(values []string) error {
	for _, value := range values {
		if !githubRepoSlugPattern.MatchString(value) {
			return fmt.Errorf("invalid repo %q: expected owner/repo", value)
		}
	}
	return nil
}
