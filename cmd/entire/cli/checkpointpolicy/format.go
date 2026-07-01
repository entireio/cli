package checkpointpolicy

import (
	"fmt"
	"slices"
	"strings"

	semver "github.com/Masterminds/semver/v3"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

const (
	LogicalCheckpointVersionV1       = "1.0.0"
	DefaultCheckpointVersionSelector = LogicalCheckpointVersionV1
)

func ParseCheckpointVersionSelector(raw string) (*semver.Constraints, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("checkpoint_version %q is not a valid SemVer constraint", raw)
	}
	constraint, err := semver.NewConstraint(raw)
	if err != nil {
		return nil, fmt.Errorf("checkpoint_version %q is not a valid SemVer constraint: %w", raw, err)
	}
	return constraint, nil
}

func ResolveCheckpointVersionSelector(raw string) (string, error) {
	version, err := resolveCheckpointVersionSelector(raw, supportedCheckpointVersions)
	if err != nil {
		return "", err
	}
	return version.String(), nil
}

func CanReadVersion(raw string) bool {
	version, err := semver.StrictNewVersion(raw)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(supportedCheckpointVersions, version.Equal)
}

func LogicalVersionForCheckpointMetadata(raw string) (string, error) {
	switch raw {
	case "", checkpoint.CheckpointVersionBranchV1:
		return LogicalCheckpointVersionV1, nil
	}
	version, err := semver.StrictNewVersion(raw)
	if err != nil {
		return "", fmt.Errorf("invalid checkpoint_version %q", raw)
	}
	return version.String(), nil
}

func MetadataVersionForWriteVersion(raw string) (string, error) {
	switch raw {
	case LogicalCheckpointVersionV1:
		return checkpoint.CheckpointVersionBranchV1, nil
	default:
		return "", fmt.Errorf("checkpoint_version %q is not writable by this Entire CLI", raw)
	}
}

func resolveCheckpointVersionSelector(raw string, candidates []*semver.Version) (*semver.Version, error) {
	constraint, err := ParseCheckpointVersionSelector(raw)
	if err != nil {
		return nil, err
	}
	var best *semver.Version
	for _, candidate := range candidates {
		if !constraint.Check(candidate) {
			continue
		}
		if best == nil || best.LessThan(candidate) {
			best = candidate
		}
	}
	if best == nil {
		return nil, fmt.Errorf("checkpoint_version %q is not writable by this Entire CLI", raw)
	}
	return best, nil
}

func mustSemver(raw string) *semver.Version {
	version, err := semver.StrictNewVersion(raw)
	if err != nil {
		panic(err)
	}
	return version
}

var supportedCheckpointVersions = []*semver.Version{
	mustSemver(LogicalCheckpointVersionV1),
}
