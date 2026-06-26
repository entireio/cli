package checkpoint

import "testing"

func TestImportedFlagsOnSummaryAndInfo(t *testing.T) {
	t.Parallel()
	if !(CheckpointSummary{Imported: true}).Imported {
		t.Fatal("CheckpointSummary.Imported not settable")
	}
	if !(CheckpointInfo{Imported: true}).Imported {
		t.Fatal("CheckpointInfo.Imported not settable")
	}
}
