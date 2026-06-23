package checkpointpolicy

import (
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

type Policy struct {
	CheckpointVersion    string `json:"checkpoint_version"`
	CheckpointMinVersion string `json:"checkpoint_min_version"`
}

func DefaultPolicy() Policy {
	return Policy{
		CheckpointVersion:    checkpoint.CheckpointVersionBranchV1,
		CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
	}
}

func Normalize(policy Policy) Policy {
	if policy.CheckpointVersion == "" {
		policy.CheckpointVersion = checkpoint.CheckpointVersionBranchV1
	}
	if policy.CheckpointMinVersion == "" {
		policy.CheckpointMinVersion = checkpoint.CheckpointVersionBranchV1
	}
	return policy
}

func ValidatePolicy(policy Policy) error {
	policy = Normalize(policy)

	version, err := ParseFormat(policy.CheckpointVersion)
	if err != nil {
		return fmt.Errorf("checkpoint_version: %w", err)
	}
	if !CanWrite(version) {
		return fmt.Errorf("checkpoint_version %q is not write-supported by this Entire CLI", policy.CheckpointVersion)
	}

	minVersion, err := ParseFormat(policy.CheckpointMinVersion)
	if err != nil {
		return fmt.Errorf("checkpoint_min_version: %w", err)
	}
	if !CanRead(minVersion) {
		return fmt.Errorf("checkpoint_min_version %q is not read-supported by this Entire CLI", policy.CheckpointMinVersion)
	}
	if Compare(minVersion, version) > 0 {
		return fmt.Errorf("checkpoint_min_version %q is newer than checkpoint_version %q", policy.CheckpointMinVersion, policy.CheckpointVersion)
	}

	return nil
}

func RequiresUpgrade(policy Policy) bool {
	policy = Normalize(policy)
	minVersion, err := ParseFormat(policy.CheckpointMinVersion)
	if err != nil {
		return true
	}
	return !CanRead(minVersion)
}

func UnsupportedWrite(policy Policy) bool {
	policy = Normalize(policy)
	version, err := ParseFormat(policy.CheckpointVersion)
	if err != nil {
		return true
	}
	return !CanWrite(version)
}

func UpgradeWarning(updateCommand string) string {
	return fmt.Sprintf("[entire] This repository requires checkpoint support newer than this Entire CLI.\n[entire] Upgrade Entire, then rerun the command:\n[entire]   %s\n", updateCommand)
}

// IsUnsupportedVersion reports whether err wraps a checkpoint version incompatibility.
func IsUnsupportedVersion(err error) bool {
	var unsupported *unsupportedVersionError
	return errors.As(err, &unsupported)
}

func EnsureCanReadVersion(checkpointID, version string) error {
	policy := Normalize(Policy{CheckpointMinVersion: version})
	format, err := ParseFormat(policy.CheckpointMinVersion)
	if err != nil {
		return fmt.Errorf("checkpoint %s has invalid checkpoint_version %q: %w", checkpointID, policy.CheckpointMinVersion, err)
	}
	if !CanRead(format) {
		return &unsupportedVersionError{
			CheckpointID: checkpointID,
			Version:      policy.CheckpointMinVersion,
			Err:          errors.New("not read-supported by this Entire CLI"),
		}
	}
	return nil
}

type unsupportedVersionError struct {
	CheckpointID string
	Version      string
	Err          error
}

func (e unsupportedVersionError) Error() string {
	return fmt.Sprintf("checkpoint %s uses unsupported checkpoint_version %q: %v", e.CheckpointID, e.Version, e.Err)
}

func (e unsupportedVersionError) Unwrap() error {
	return e.Err
}
