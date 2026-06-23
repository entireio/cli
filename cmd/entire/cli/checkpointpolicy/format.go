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

	family := CheckpointFamily(familyRaw)
	if _, ok := familyRanks[family]; !ok {
		return CheckpointFormat{}, fmt.Errorf("unknown checkpoint family %q", familyRaw)
	}

	major, err := strconv.Atoi(majorRaw)
	if err != nil || major <= 0 {
		return CheckpointFormat{}, fmt.Errorf("invalid checkpoint major %q", majorRaw)
	}

	return CheckpointFormat{Family: family, Major: major}, nil
}

func (f CheckpointFormat) String() string {
	if f.Family == "" || f.Major == 0 {
		return ""
	}
	return fmt.Sprintf("%s-v%d", f.Family, f.Major)
}

func Compare(a, b CheckpointFormat) int {
	aRank := familyRanks[a.Family]
	bRank := familyRanks[b.Family]
	if aRank != bRank {
		return cmp.Compare(aRank, bRank)
	}
	return cmp.Compare(a.Major, b.Major)
}

func CanRead(format CheckpointFormat) bool {
	return readFormats[format]
}

func CanWrite(format CheckpointFormat) bool {
	return writeFormats[format]
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
