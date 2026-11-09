package benchutil

import (
	"fmt"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

func BenchmarkNewBenchRepo(b *testing.B) {
	for b.Loop() {
		NewBenchRepo(b, RepoOpts{})
	}
}

func BenchmarkNewBenchRepo_Large(b *testing.B) {
	for b.Loop() {
		NewBenchRepo(b, RepoOpts{
			FileCount:     50,
			FileSizeLines: 500,
		})
	}
}

func BenchmarkSeedMetadataBranch(b *testing.B) {
	for _, count := range []int{1, 5, 10} {
		b.Run(fmt.Sprintf("%dCheckpoints", count), func(b *testing.B) {
			for b.Loop() {
				repo := NewBenchRepo(b, RepoOpts{FileCount: 10})
				repo.SeedMetadataBranch(b, count)
			}
		})
	}
}

func BenchmarkGenerateTranscript(b *testing.B) {
	b.Run("Small_20msg", func(b *testing.B) {
		for b.Loop() {
			GenerateTranscript(TranscriptOpts{
				MessageCount:    20,
				AvgMessageBytes: 500,
			})
		}
	})

	b.Run("Medium_200msg", func(b *testing.B) {
		for b.Loop() {
			GenerateTranscript(TranscriptOpts{
				MessageCount:    200,
				AvgMessageBytes: 500,
			})
		}
	})

	b.Run("Large_2000msg", func(b *testing.B) {
		for b.Loop() {
			GenerateTranscript(TranscriptOpts{
				MessageCount:    2000,
				AvgMessageBytes: 500,
			})
		}
	})

	b.Run("WithToolUse", func(b *testing.B) {
		files := []string{"src/main.go", "src/util.go", "src/handler.go"}
		for b.Loop() {
			GenerateTranscript(TranscriptOpts{
				MessageCount:    200,
				AvgMessageBytes: 500,
				IncludeToolUse:  true,
				FilesTouched:    files,
			})
		}
	})
}

func BenchmarkCreateSessionState(b *testing.B) {
	repo := NewBenchRepo(b, RepoOpts{FileCount: 10})

	b.ResetTimer()
	for b.Loop() {
		repo.CreateSessionState(b, SessionOpts{
			Phase:        session.PhaseActive,
			StepCount:    5,
			FilesTouched: []string{"src/file_000.go", "src/file_001.go", "src/file_002.go"},
		})
	}
}
