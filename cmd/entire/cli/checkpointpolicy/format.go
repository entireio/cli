package checkpointpolicy

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	semver "github.com/Masterminds/semver/v3"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

const (
	LogicalCheckpointVersionV1       = "1.0.0"
	DefaultCheckpointVersionSelector = "1"
	DefaultCheckpointMinVersionRange = ">=1.0.0"
)

var checkpointVersionSelectorPattern = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+)(?:\.([0-9]+))?)?$`)

func ParseCheckpointVersionSelector(raw string) (*semver.Constraints, error) {
	if !checkpointVersionSelectorPattern.MatchString(raw) {
		return nil, fmt.Errorf("checkpoint_version %q is not a valid SemVer selector", raw)
	}
	constraint, err := semver.NewConstraint(raw)
	if err != nil {
		return nil, fmt.Errorf("checkpoint_version %q is not a valid SemVer selector: %w", raw, err)
	}
	return constraint, nil
}

func ResolveCheckpointVersionSelector(raw string) (string, error) {
	version, err := resolveCheckpointVersionSelector(raw, writableCheckpointVersions)
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
	return slices.ContainsFunc(readableCheckpointVersions, version.Equal)
}

func CanWriteVersion(raw string) bool {
	version, err := semver.StrictNewVersion(raw)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(writableCheckpointVersions, version.Equal)
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

func CheckpointMinVersionConstraint(raw string) (*semver.Constraints, error) {
	constraint, err := semver.NewConstraint(raw)
	if err != nil {
		return nil, fmt.Errorf("checkpoint_min_version %q is not a valid SemVer constraint: %w", raw, err)
	}
	return constraint, nil
}

func readableVersionSatisfies(constraint *semver.Constraints) bool {
	return slices.ContainsFunc(readableCheckpointVersions, constraint.Check)
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

var readableCheckpointVersions = []*semver.Version{
	mustSemver(LogicalCheckpointVersionV1),
}

var writableCheckpointVersions = []*semver.Version{
	mustSemver(LogicalCheckpointVersionV1),
}

type CheckpointFamily string

const (
	CheckpointFamilyBranch CheckpointFamily = "branch"
	CheckpointFamilyRefs   CheckpointFamily = "refs"
)

type CheckpointFormat struct {
	Family CheckpointFamily
	Major  int
}

func ParseFormat(raw string) (CheckpointFormat, error) {
	familyRaw, majorRaw, ok := strings.Cut(raw, "-v")
	if !ok || familyRaw == "" || majorRaw == "" {
		return CheckpointFormat{}, fmt.Errorf("invalid checkpoint format %q", raw)
	}

	major, err := strconv.Atoi(majorRaw)
	if err != nil || major <= 0 {
		return CheckpointFormat{}, fmt.Errorf("invalid checkpoint major %q", majorRaw)
	}

	return CheckpointFormat{Family: CheckpointFamily(familyRaw), Major: major}, nil
}

func (f CheckpointFormat) String() string {
	if f.Family == "" || f.Major == 0 {
		return ""
	}
	return fmt.Sprintf("%s-v%d", f.Family, f.Major)
}

func Compare(a, b CheckpointFormat) int {
	aRank := familyRank(a.Family)
	bRank := familyRank(b.Family)
	if aRank != bRank {
		return cmp.Compare(aRank, bRank)
	}
	if a.Family != b.Family {
		return cmp.Compare(string(a.Family), string(b.Family))
	}
	return cmp.Compare(a.Major, b.Major)
}

func CanRead(format CheckpointFormat) bool {
	return readFormats[format]
}

func CanWrite(format CheckpointFormat) bool {
	return writeFormats[format]
}

func familyRank(family CheckpointFamily) int {
	if rank, ok := familyRanks[family]; ok {
		return rank
	}
	return len(familyRanks)
}

var familyRanks = map[CheckpointFamily]int{
	CheckpointFamilyBranch: 0,
	CheckpointFamilyRefs:   1,
}

var branchV1Format = CheckpointFormat{Family: CheckpointFamilyBranch, Major: 1}

var (
	readFormats = map[CheckpointFormat]bool{
		branchV1Format: true,
	}

	writeFormats = map[CheckpointFormat]bool{
		branchV1Format: true,
	}
)
