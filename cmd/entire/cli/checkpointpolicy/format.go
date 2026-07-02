package checkpointpolicy

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

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

var (
	branchV1Format = CheckpointFormat{Family: CheckpointFamilyBranch, Major: 1}
	refsV1Format   = CheckpointFormat{Family: CheckpointFamilyRefs, Major: 1}
)

var (
	readFormats = map[CheckpointFormat]bool{
		branchV1Format: true,
		refsV1Format:   true,
	}

	writeFormats = map[CheckpointFormat]bool{
		branchV1Format: true,
		refsV1Format:   true,
	}
)
