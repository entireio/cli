package repopolicy

import "context"

// ClassifyRepoPolicy resolves one read-only repository-policy snapshot for
// the current directory. It never writes.
func ClassifyRepoPolicy(ctx context.Context) (RepoPolicy, error) {
	return ClassifyRepoPolicyAt(ctx, ".")
}

// ClassifyRepoPolicyAt classifies an explicit worktree. Precedence:
//
//  1. The repository's own settings files (repo-level activation). A
//     configured repo is active iff enabled — an explicit enabled:false is a
//     veto that also blocks the global tier. While the user tier is on, the
//     user's exclude lists outrank a committed settings.json: that file is
//     repository content and arrives by cloning, so a clone must not be able
//     to activate capture in a folder the user excluded. Only an untracked
//     settings.local.json with an explicit enabled key — the developer's own
//     action on this clone — keeps activating an excluded repo.
//  2. Otherwise the user-global tier: enabled, valid, and not excluding this
//     repo. Any error there fails closed (inactive + the error).
//
// Trust (checkpoint egress) is decided last from the same inputs.
func ClassifyRepoPolicyAt(ctx context.Context, dir string) (RepoPolicy, error) {
	repository, err := ResolveRepositoryAt(ctx, dir)
	if err != nil {
		return inactiveGlobalPolicy(InactiveReasonGlobalOff), err
	}
	policy := RepoPolicy{
		ActivationSource: ActivationInactive,
		InactiveReason:   InactiveReasonGlobalOff,
		WorktreeRoot:     repository.WorktreeRoot,
		GitCommonDir:     repository.GitCommonDir,
		WorktreeKey:      repository.WorktreeKey,
	}

	// Repo-level settings first: a repo the user enabled here must keep
	// capturing even when the user-global file is unreadable (main never
	// consulted that file at all). Only egress is affected in that case.
	activation, err := ReadRepoActivation(ctx, repository.WorktreeRoot)
	if err != nil {
		policy.InactiveReason = InactiveReasonRepoDisabled
		return policy, err
	}
	userSettings, settingsErr := LoadUserSettings(ctx)
	switch {
	case activation.Configured && activation.Enabled:
		policy.Active = true
		policy.ActivationSource = ActivationLocal
		policy.InactiveReason = InactiveReasonNone
		if settingsErr != nil {
			// Capture stays on; egress fails closed until the file is fixed.
			policy.Trust = TrustDecision{Source: TrustSourceNone, Reason: TrustReasonSettings}
			return policy, nil //nolint:nilerr // deliberate: repo-level activation survives an unreadable user settings file; only egress is held
		}
		if userSettings.GlobalEnabled() && !activation.LocalOverride {
			excluded, exclErr := ExcludedByGlobalConfig(ctx, userSettings.Global, repository)
			if exclErr != nil || excluded {
				policy.Active = false
				policy.ActivationSource = ActivationInactive
				policy.InactiveReason = InactiveReasonGlobalExcluded
				policy.Trust = DecideEgress(ctx, policy, userSettings.Global, repository)
				return policy, exclErr
			}
		}
	case activation.Configured:
		policy.InactiveReason = InactiveReasonRepoDisabled
		return policy, nil
	default:
		if settingsErr != nil {
			return policy, settingsErr
		}
		// ClassifyGlobalConfig's inactive returns carry an empty identity, so
		// copy only the activation fields; identity always comes from
		// repository.
		global, classifyErr := ClassifyGlobalConfig(ctx, userSettings.Global, func(context.Context) (Repository, error) {
			return repository, nil
		})
		policy.Active = global.Active
		policy.ActivationSource = global.ActivationSource
		policy.InactiveReason = global.InactiveReason
		if classifyErr != nil {
			return policy, classifyErr
		}
	}
	policy.Trust = DecideEgress(ctx, policy, userSettings.Global, repository)
	return policy, nil
}

type repoPolicyContextKey struct{}

// WithRepoPolicy returns a context carrying an immutable policy snapshot.
func WithRepoPolicy(ctx context.Context, policy RepoPolicy) context.Context {
	return context.WithValue(ctx, repoPolicyContextKey{}, policy)
}

// RepoPolicyFromContext returns a policy snapshot previously attached to ctx.
//
//nolint:revive // Established public name; every consumer imports it as repopolicy.RepoPolicyFromContext.
func RepoPolicyFromContext(ctx context.Context) (RepoPolicy, bool) {
	policy, ok := ctx.Value(repoPolicyContextKey{}).(RepoPolicy)
	return policy, ok
}
