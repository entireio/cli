package checkpointpolicy

import (
	"fmt"
	"strings"

	semver "github.com/Masterminds/semver/v3"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

const (
	LogicalCheckpointVersionV1       = "1.0.0"
	DefaultCheckpointVersionSelector = LogicalCheckpointVersionV1
)

type CheckpointFeature string

const FeatureBranchMetadata CheckpointFeature = "branch_metadata"

type supportedCheckpointVersion struct {
	version         *semver.Version
	metadataVersion string
	features        []CheckpointFeature
}

var supportedCheckpointVersions = []supportedCheckpointVersion{
	{
		version:         mustSemver(LogicalCheckpointVersionV1),
		metadataVersion: checkpoint.CheckpointVersionBranchV1,
		features:        []CheckpointFeature{FeatureBranchMetadata},
	},
}

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

// ValidateCheckpointVersionSelector reports whether raw is a selector this CLI can satisfy.
func ValidateCheckpointVersionSelector(raw string) error {
	_, err := resolveCheckpointVersionSelector(raw, supportedCheckpointVersions)
	return err
}

// IsSupportedCheckpointVersion reports whether raw is a logical checkpoint version this CLI supports.
func IsSupportedCheckpointVersion(raw string) bool {
	version, err := semver.StrictNewVersion(raw)
	if err != nil {
		return false
	}
	_, ok := findSupportedCheckpointVersion(version)
	return ok
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

func resolveCheckpointVersionSelector(raw string, candidates []supportedCheckpointVersion) (supportedCheckpointVersion, error) {
	constraint, err := ParseCheckpointVersionSelector(raw)
	if err != nil {
		return supportedCheckpointVersion{}, err
	}
	var best supportedCheckpointVersion
	var found bool
	for _, candidate := range candidates {
		if !constraint.Check(candidate.version) {
			continue
		}
		if !found || best.version.LessThan(candidate.version) {
			best = candidate
			found = true
		}
	}
	if found {
		return best, nil
	}
	return supportedCheckpointVersion{}, fmt.Errorf("checkpoint_version %q is not supported by this Entire CLI", raw)
}

func mustSemver(raw string) *semver.Version {
	version, err := semver.StrictNewVersion(raw)
	if err != nil {
		panic(err)
	}
	return version
}

func findSupportedCheckpointVersion(version *semver.Version) (supportedCheckpointVersion, bool) {
	for _, candidate := range supportedCheckpointVersions {
		if candidate.version.Equal(version) {
			return candidate, true
		}
	}
	return supportedCheckpointVersion{}, false
}
