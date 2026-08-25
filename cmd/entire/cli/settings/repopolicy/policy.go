package repopolicy

import (
	"context"
	"errors"
)

// ClassifyRepoPolicy resolves one read-only repository-policy snapshot.
func ClassifyRepoPolicy(ctx context.Context) (RepoPolicy, error) {
	repository, err := ResolveRepository(ctx)
	if err != nil {
		return inactiveGlobalPolicy(InactiveReasonGlobalOff), err
	}
	base := inactivePolicyForRepository(repository)
	existingRoute, routeFound, err := ReadRuntimeRoute(repository)
	if err != nil {
		return base, err
	}
	state, err := ReadLocalActivation(repository)
	if err != nil {
		return base, err
	}
	if state == ActivationDisabled {
		policy := policyForRepository(repository)
		policy.ActivationSource = ActivationInactive
		policy.InactiveReason = InactiveReasonRepoDisabled
		policy.Route = routeForClassification(repository, existingRoute, routeFound, RuntimeWorktree)
		return policy, nil
	}
	veto, err := repositoryDisabledVeto(ctx, repository)
	if err != nil {
		return base, err
	}
	if veto {
		policy := policyForRepository(repository)
		policy.ActivationSource = ActivationInactive
		policy.InactiveReason = InactiveReasonRepoDisabled
		policy.Route = routeForClassification(repository, existingRoute, routeFound, RuntimeWorktree)
		return policy, nil
	}
	if state == ActivationEnabled {
		policy := policyForRepository(repository)
		policy.Active = true
		policy.ActivationSource = ActivationLocal
		policy.Route = routeForClassification(repository, existingRoute, routeFound, RuntimeWorktree)
		return policy, nil
	}

	userSettings, err := LoadUserSettings(ctx)
	if err != nil {
		return base, err
	}
	policy, err := ClassifyGlobalConfig(ctx, userSettings.Global, func(context.Context) (Repository, error) {
		return repository, nil
	})
	if policy.WorktreeRoot == "" {
		policy.WorktreeRoot = repository.WorktreeRoot
		policy.GitCommonDir = repository.GitCommonDir
		policy.WorktreeKey = repository.WorktreeKey
	}
	if policy.Active {
		policy.Route = routeForClassification(repository, existingRoute, routeFound, RuntimeGitCommon)
	} else {
		policy.Route = routeForClassification(repository, existingRoute, routeFound, RuntimeWorktree)
	}
	return policy, err
}

func inactivePolicyForRepository(repository Repository) RepoPolicy {
	return RepoPolicy{
		ActivationSource: ActivationInactive,
		InactiveReason:   InactiveReasonGlobalOff,
		WorktreeRoot:     repository.WorktreeRoot,
		GitCommonDir:     repository.GitCommonDir,
		WorktreeKey:      repository.WorktreeKey,
	}
}

func routeForClassification(repository Repository, existing RuntimeRoute, found bool, proposed RuntimeLayout) RuntimeRoute {
	if found {
		return existing
	}
	return proposedRoute(repository, proposed)
}

func proposedRoute(repository Repository, layout RuntimeLayout) RuntimeRoute {
	return RuntimeRoute{
		Version:            recordVersion,
		Layout:             layout,
		CanonicalWorktree:  repository.WorktreeRoot,
		CanonicalGitCommon: repository.GitCommonDir,
	}
}

func repositoryDisabledVeto(ctx context.Context, repository Repository) (bool, error) {
	enabled, err := effectiveEnabledSetting(ctx, repository)
	if errors.Is(err, errInvalidActivationSettings) {
		// Invalid repository content cannot establish a trusted veto. Hook-time
		// settings loading still rejects malformed allowed fields before capture.
		return false, nil
	}
	return enabled != nil && !*enabled, err
}

type repoPolicyContextKey struct{}

// WithRepoPolicy returns a context carrying an immutable policy snapshot.
func WithRepoPolicy(ctx context.Context, policy RepoPolicy) context.Context {
	return context.WithValue(ctx, repoPolicyContextKey{}, policy)
}

// RepoPolicyFromContext returns a policy snapshot previously attached to ctx.
//
//nolint:revive // The task's public compatibility API requires this exact name.
func RepoPolicyFromContext(ctx context.Context) (RepoPolicy, bool) {
	policy, ok := ctx.Value(repoPolicyContextKey{}).(RepoPolicy)
	return policy, ok
}
