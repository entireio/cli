package cli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

func placementIn(host, jurisdiction string) coreapi.ResolvedPlacement {
	return coreapi.ResolvedPlacement{ClusterHost: host, Jurisdiction: coreapi.NewOptString(jurisdiction)}
}

func TestPreferHomeJurisdiction(t *testing.T) {
	t.Parallel()

	us1 := placementIn("aws-us-east-2.entire.io", "us")
	us2 := placementIn("aws-us-west-1.entire.io", "us")
	eu1 := placementIn("aws-eu-central-1.entire.io", "eu")

	t.Run("narrows to the home jurisdiction", func(t *testing.T) {
		t.Parallel()
		got := preferHomeJurisdiction([]coreapi.ResolvedPlacement{us1, eu1, us2}, "eu")
		require.Equal(t, []coreapi.ResolvedPlacement{eu1}, got)
	})

	t.Run("matches case-insensitively", func(t *testing.T) {
		t.Parallel()
		got := preferHomeJurisdiction([]coreapi.ResolvedPlacement{us1, eu1}, "EU")
		require.Equal(t, []coreapi.ResolvedPlacement{eu1}, got)
	})

	t.Run("keeps every home-region placement", func(t *testing.T) {
		t.Parallel()
		got := preferHomeJurisdiction([]coreapi.ResolvedPlacement{us1, placementIn("aws-eu-west-1.entire.io", "eu"), eu1}, "eu")
		require.Len(t, got, 2)
	})

	t.Run("unknown home leaves the list unchanged", func(t *testing.T) {
		t.Parallel()
		in := []coreapi.ResolvedPlacement{us1, eu1}
		require.Equal(t, in, preferHomeJurisdiction(in, ""))
	})

	t.Run("no home-region match leaves the list unchanged", func(t *testing.T) {
		t.Parallel()
		in := []coreapi.ResolvedPlacement{us1, us2}
		require.Equal(t, in, preferHomeJurisdiction(in, "eu"))
	})

	t.Run("single placement is untouched", func(t *testing.T) {
		t.Parallel()
		in := []coreapi.ResolvedPlacement{us1}
		require.Equal(t, in, preferHomeJurisdiction(in, "eu"))
	})
}

func swapCloneHomeJurisdiction(t *testing.T, fn func(context.Context) string) {
	t.Helper()
	orig := cloneHomeJurisdiction
	cloneHomeJurisdiction = fn
	t.Cleanup(func() { cloneHomeJurisdiction = orig })
}

func TestSelectCloneTarget_HomeJurisdictionDefault(t *testing.T) {
	us := placementIn("aws-us-east-2.entire.io", "us")
	eu := placementIn("aws-eu-central-1.entire.io", "eu")
	usWest := placementIn("aws-us-west-1.entire.io", "us")

	t.Run("no --cluster auto-selects the home-region placement", func(t *testing.T) {
		swapCloneHomeJurisdiction(t, func(context.Context) string { return "eu" })
		// Three clusters across two regions; home filter leaves one, so the picker
		// is skipped (works even non-interactively) and eu is chosen.
		got, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{us, eu, usWest}, "")
		require.NoError(t, err)
		require.Equal(t, "aws-eu-central-1.entire.io", got.ClusterHost)
	})

	t.Run("--cluster bypasses the home filter", func(t *testing.T) {
		swapCloneHomeJurisdiction(t, func(context.Context) string {
			t.Fatal("home jurisdiction must not be consulted when --cluster is set")
			return ""
		})
		got, err := selectCloneTarget(newCloneTestCmd(), []coreapi.ResolvedPlacement{us, eu, usWest}, "aws-us-west-1.entire.io")
		require.NoError(t, err)
		require.Equal(t, "aws-us-west-1.entire.io", got.ClusterHost)
	})
}
