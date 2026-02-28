//go:build growthmodel

package checkpoint

import (
	"fmt"
	"strings"
	"testing"
)

// Model parameters — adjust these based on observed data.
const (
	// Derived from entire/cli repo: 1100 checkpoints / 10 devs / ~44 working days ≈ 2.5
	checkpointsPerDevPerDay = 2.5

	// From real transcript analysis: avg 2821 KB across 1140 checkpoints.
	// User says ~2.1 MB; real data shows ~2.8 MB. Use observed value.
	avgCheckpointSizeBytes = 2.8 * 1024 * 1024 // 2.8 MB

	// From push profiling with real transcripts (2.5MB tier, 100-200 checkpoints):
	// pack/raw ratio was 9.6-11.6%. Use 11% as conservative estimate.
	gitPackRatio = 0.11

	// Working days per month.
	workingDaysPerMonth = 22

	// Average repos per company (active repos with Entire enabled).
	reposPerCompanySmall      = 3
	reposPerCompanyMedium     = 8
	reposPerCompanyLarge      = 20
	reposPerCompanyEnterprise = 60

	// Push timing constants from profiling (pack+send phase).
	// Effective throughput: ~2 MB/s for pack data over HTTPS.
	pushThroughputMBps = 2.0
	// Fixed cost per push: negotiate + remote + overhead ≈ 1.1s
	pushFixedCostSec = 1.1
)

// TestGrowthModel projects data volumes and push times as customer scale grows.
//
// Run with: go test -v -run TestGrowthModel -tags growthmodel ./cmd/entire/cli/checkpoint/
func TestGrowthModel(t *testing.T) {
	t.Parallel()

	teamSizes := []int{10, 50, 250, 1000}
	months := []int{1, 3, 6, 12}

	// --- Table 1: Per-Repo Growth ---
	t.Logf("\n%s", strings.Repeat("=", 100))
	t.Logf("TABLE 1: PER-REPO DATA GROWTH")
	t.Logf("Assumptions: %.1f checkpoints/dev/day, %.1f MB avg transcript, %d working days/month",
		checkpointsPerDevPerDay, avgCheckpointSizeBytes/(1024*1024), workingDaysPerMonth)
	t.Logf("%s", strings.Repeat("=", 100))

	t.Logf("\n%-12s | %-52s | %-30s", "Team Size",
		"Raw Data (cumulative)",
		"Git Pack (on disk / transfer)")
	t.Logf("%-12s | %12s %12s %12s %12s | %12s %12s %12s",
		"", "1 mo", "3 mo", "6 mo", "12 mo", "3 mo", "6 mo", "12 mo")
	t.Log(strings.Repeat("-", 110))

	type repoProjection struct {
		teamSize    int
		rawByMonth  [4]float64 // GB at 1,3,6,12 months
		packByMonth [4]float64 // GB at 1,3,6,12 months
		cpsByMonth  [4]int     // checkpoint count
	}

	var projections []repoProjection

	for _, devs := range teamSizes {
		var p repoProjection
		p.teamSize = devs

		for i, mo := range months {
			days := float64(mo) * workingDaysPerMonth
			totalCPs := checkpointsPerDevPerDay * float64(devs) * days
			rawGB := totalCPs * avgCheckpointSizeBytes / (1024 * 1024 * 1024)
			packGB := rawGB * gitPackRatio

			p.rawByMonth[i] = rawGB
			p.packByMonth[i] = packGB
			p.cpsByMonth[i] = int(totalCPs)
		}

		t.Logf("%-12s | %12s %12s %12s %12s | %12s %12s %12s",
			fmt.Sprintf("%d devs", devs),
			fmtSize(p.rawByMonth[0]), fmtSize(p.rawByMonth[1]),
			fmtSize(p.rawByMonth[2]), fmtSize(p.rawByMonth[3]),
			fmtSize(p.packByMonth[1]), fmtSize(p.packByMonth[2]),
			fmtSize(p.packByMonth[3]))

		projections = append(projections, p)
	}

	// Checkpoint counts.
	t.Logf("")
	t.Logf("%-12s | %12s %12s %12s %12s |", "Checkpoints", "1 mo", "3 mo", "6 mo", "12 mo")
	t.Log(strings.Repeat("-", 70))
	for _, p := range projections {
		t.Logf("%-12s | %12s %12s %12s %12s |",
			fmt.Sprintf("%d devs", p.teamSize),
			fmtCount(p.cpsByMonth[0]), fmtCount(p.cpsByMonth[1]),
			fmtCount(p.cpsByMonth[2]), fmtCount(p.cpsByMonth[3]))
	}

	// --- Table 2: Push Time Projections ---
	t.Logf("\n%s", strings.Repeat("=", 100))
	t.Logf("TABLE 2: PUSH TIME PROJECTIONS (first push of full branch)")
	t.Logf("Assumptions: %.1f MB/s pack throughput, %.1fs fixed cost (negotiate + remote + overhead)",
		pushThroughputMBps, pushFixedCostSec)
	t.Logf("%s", strings.Repeat("=", 100))

	t.Logf("\n%-12s | %12s %12s %12s %12s |", "Team Size", "1 mo", "3 mo", "6 mo", "12 mo")
	t.Log(strings.Repeat("-", 70))
	for _, p := range projections {
		pushTimes := [4]string{}
		for i := range months {
			packMB := p.packByMonth[i] * 1024 // GB to MB
			pushSec := packMB/pushThroughputMBps + pushFixedCostSec
			pushTimes[i] = fmtDuration(pushSec)
		}
		t.Logf("%-12s | %12s %12s %12s %12s |",
			fmt.Sprintf("%d devs", p.teamSize),
			pushTimes[0], pushTimes[1], pushTimes[2], pushTimes[3])
	}

	t.Logf("\nNote: Incremental pushes (1 new checkpoint) are ~1-1.5s regardless of repo size.")

	// --- Table 3: Platform-Level (Multi-Customer) ---
	t.Logf("\n%s", strings.Repeat("=", 100))
	t.Logf("TABLE 3: PLATFORM-LEVEL STORAGE (total across all customers)")
	t.Logf("Repos/company: small=%d, medium=%d, large=%d, enterprise=%d",
		reposPerCompanySmall, reposPerCompanyMedium, reposPerCompanyLarge, reposPerCompanyEnterprise)
	t.Logf("%s", strings.Repeat("=", 100))

	type customerMix struct {
		label      string
		small      int // 10-dev companies
		medium     int // 50-dev companies
		large      int // 250-dev companies
		enterprise int // 1000-dev companies
	}

	mixes := []customerMix{
		{"Early (10 customers)", 7, 2, 1, 0},
		{"Growth (50 customers)", 25, 15, 8, 2},
		{"Scale (200 customers)", 80, 70, 35, 15},
		{"Mature (1000 customers)", 400, 350, 175, 75},
	}

	// Per-repo raw data at each month for each team size (index: 0=10dev, 1=50dev, 2=250dev, 3=1000dev).
	reposForTier := []int{reposPerCompanySmall, reposPerCompanyMedium, reposPerCompanyLarge, reposPerCompanyEnterprise}

	t.Logf("\n%-28s | %15s %15s %15s %15s |", "Stage",
		"1 mo (raw)", "3 mo (raw)", "6 mo (raw)", "12 mo (raw)")
	t.Log(strings.Repeat("-", 105))

	for _, mix := range mixes {
		counts := []int{mix.small, mix.medium, mix.large, mix.enterprise}
		monthTotals := [4]float64{}

		for i := range months {
			for tier := range teamSizes {
				// raw GB per repo × repos per company × number of companies
				repoRaw := projections[tier].rawByMonth[i]
				monthTotals[i] += repoRaw * float64(reposForTier[tier]) * float64(counts[tier])
			}
		}

		t.Logf("%-28s | %15s %15s %15s %15s |",
			mix.label,
			fmtSizeLarge(monthTotals[0]),
			fmtSizeLarge(monthTotals[1]),
			fmtSizeLarge(monthTotals[2]),
			fmtSizeLarge(monthTotals[3]))
	}

	// Git pack (compressed) version.
	t.Logf("")
	t.Logf("%-28s | %15s %15s %15s %15s |",
		fmt.Sprintf("Stage (git pack, %.0f%% of raw)", gitPackRatio*100),
		"1 mo (pack)", "3 mo (pack)", "6 mo (pack)", "12 mo (pack)")
	t.Log(strings.Repeat("-", 105))

	for _, mix := range mixes {
		counts := []int{mix.small, mix.medium, mix.large, mix.enterprise}
		monthTotals := [4]float64{}

		for i := range months {
			for tier := range teamSizes {
				repoPack := projections[tier].packByMonth[i]
				monthTotals[i] += repoPack * float64(reposForTier[tier]) * float64(counts[tier])
			}
		}

		t.Logf("%-28s | %15s %15s %15s %15s |",
			mix.label,
			fmtSizeLarge(monthTotals[0]),
			fmtSizeLarge(monthTotals[1]),
			fmtSizeLarge(monthTotals[2]),
			fmtSizeLarge(monthTotals[3]))
	}

	// --- Table 4: Per-Developer Unit Economics ---
	t.Logf("\n%s", strings.Repeat("=", 100))
	t.Logf("TABLE 4: PER-DEVELOPER UNIT ECONOMICS")
	t.Logf("%s", strings.Repeat("=", 100))

	rawPerDevPerMonth := checkpointsPerDevPerDay * workingDaysPerMonth * avgCheckpointSizeBytes / (1024 * 1024)
	packPerDevPerMonth := rawPerDevPerMonth * gitPackRatio
	cpsPerDevPerMonth := checkpointsPerDevPerDay * workingDaysPerMonth

	t.Logf("  Checkpoints/dev/month:     %.0f", cpsPerDevPerMonth)
	t.Logf("  Raw data/dev/month:        %.0f MB", rawPerDevPerMonth)
	t.Logf("  Git pack/dev/month:        %.1f MB", packPerDevPerMonth)
	t.Logf("  Raw data/dev/year:         %.1f GB", rawPerDevPerMonth*12/1024)
	t.Logf("  Git pack/dev/year:         %.0f MB", packPerDevPerMonth*12)
	t.Logf("")
	t.Logf("  At $0.023/GB/mo (S3):      $%.4f/dev/month (pack)", packPerDevPerMonth/1024*0.023)
	t.Logf("  At $0.023/GB/mo (S3):      $%.4f/dev/month (raw)", rawPerDevPerMonth/1024*0.023)
	t.Logf("")
	t.Logf("  GitHub storage (pack):     %.1f MB/dev/year", packPerDevPerMonth*12)
	t.Logf("  GitHub free tier:          1 GB per repo")
	t.Logf("  GitHub LFS:               $5/mo per 50 GB data pack")

	// --- Table 5: When Do Repos Hit Size Thresholds ---
	t.Logf("\n%s", strings.Repeat("=", 100))
	t.Logf("TABLE 5: TIME TO HIT GITHUB SIZE THRESHOLDS (git pack size)")
	t.Logf("GitHub warning at 1 GB, recommended limit 5 GB, hard limit ~100 GB")
	t.Logf("%s", strings.Repeat("=", 100))

	thresholds := []struct {
		name string
		gb   float64
	}{
		{"1 GB (warning)", 1},
		{"5 GB (recommended max)", 5},
		{"10 GB (push issues)", 10},
		{"100 GB (hard limit)", 100},
	}

	t.Logf("\n%-12s | %22s %22s %22s %22s |", "Team Size",
		thresholds[0].name, thresholds[1].name, thresholds[2].name, thresholds[3].name)
	t.Log(strings.Repeat("-", 105))

	for _, devs := range teamSizes {
		packPerMonth := checkpointsPerDevPerDay * float64(devs) * workingDaysPerMonth *
			avgCheckpointSizeBytes / (1024 * 1024 * 1024) * gitPackRatio

		results := [4]string{}
		for i, th := range thresholds {
			monthsToHit := th.gb / packPerMonth
			if monthsToHit < 1 {
				results[i] = fmt.Sprintf("%.0f days", monthsToHit*30)
			} else if monthsToHit > 120 {
				results[i] = "> 10 years"
			} else {
				results[i] = fmt.Sprintf("%.1f months", monthsToHit)
			}
		}

		t.Logf("%-12s | %22s %22s %22s %22s |",
			fmt.Sprintf("%d devs", devs),
			results[0], results[1], results[2], results[3])
	}
}

func fmtSize(gb float64) string {
	switch {
	case gb >= 1024:
		return fmt.Sprintf("%.1f TB", gb/1024)
	case gb >= 1:
		return fmt.Sprintf("%.1f GB", gb)
	default:
		return fmt.Sprintf("%.0f MB", gb*1024)
	}
}

func fmtSizeLarge(gb float64) string {
	switch {
	case gb >= 1024*1024:
		return fmt.Sprintf("%.1f PB", gb/(1024*1024))
	case gb >= 1024:
		return fmt.Sprintf("%.1f TB", gb/1024)
	case gb >= 1:
		return fmt.Sprintf("%.1f GB", gb)
	default:
		return fmt.Sprintf("%.0f MB", gb*1024)
	}
}

func fmtCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func fmtDuration(sec float64) string {
	switch {
	case sec >= 3600:
		return fmt.Sprintf("%.1f hr", sec/3600)
	case sec >= 60:
		return fmt.Sprintf("%.1f min", sec/60)
	default:
		return fmt.Sprintf("%.0fs", sec)
	}
}
