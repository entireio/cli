package checkpointpolicy

import (
	"cmp"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

type CheckpointFamily string

const (
	CheckpointFamilyBranch CheckpointFamily = "branch"
	CheckpointFamilyRefs   CheckpointFamily = "refs"
)

type CheckpointFormat struct {
	Family  CheckpointFamily
	Version string
}

func ParseFormat(raw string) (CheckpointFormat, error) {
	familyRaw, versionRaw, ok := strings.Cut(raw, "-")
	if !ok || familyRaw == "" || versionRaw == "" {
		return CheckpointFormat{}, fmt.Errorf("invalid checkpoint format %q", raw)
	}

	if !semver.IsValid(versionRaw) {
		return CheckpointFormat{}, fmt.Errorf("invalid checkpoint version %q", versionRaw)
	}
	if semver.Major(versionRaw) == "v0" {
		return CheckpointFormat{}, fmt.Errorf("invalid checkpoint version %q", versionRaw)
	}
	if semver.Build(versionRaw) != "" {
		return CheckpointFormat{}, fmt.Errorf("checkpoint version %q must not include build metadata", versionRaw)
	}

	return CheckpointFormat{Family: CheckpointFamily(familyRaw), Version: semver.Canonical(versionRaw)}, nil
}

func (f CheckpointFormat) String() string {
	if f.Family == "" || f.Version == "" {
		return ""
	}
	return fmt.Sprintf("%s-%s", f.Family, f.Version)
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
	return semver.Compare(a.Version, b.Version)
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

var branchV1Format = CheckpointFormat{Family: CheckpointFamilyBranch, Version: semver.Canonical("v1")}

var (
	readFormats = map[CheckpointFormat]bool{
		branchV1Format: true,
	}

	writeFormats = map[CheckpointFormat]bool{
		branchV1Format: true,
	}
)
