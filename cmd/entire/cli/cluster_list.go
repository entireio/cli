package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// clusterColumns is the human table view of a cluster. Every column is a value
// some other command takes, which is what the table is for: REGION is the
// jurisdiction slug `org create` and `project create` name with --region,
// CLUSTER is the placement slug `repo mirror list --cluster` filters on, and
// HOST is what every targeting --cluster takes (`repo mirror add`, `repo mirror
// remove`, `repo clone`, `repo remote use`) as well as the
// host in an entire:// clone URL. The catalog's apiUrl is --json only: the CLI
// dials the API URL itself.
var clusterColumns = []string{colHeaderRegion, colHeaderCluster, "HOST"}

func clusterRow(cl coreapi.Cluster) []string {
	host, err := hostFromPublicURL(cl.PublicUrl)
	if err != nil {
		host = "-" // unsafe/malformed publicUrl: dashed, never a spoofable host (see clusterHostBySlug)
	}
	return []string{cl.Jurisdiction, cl.Slug, host}
}

// clusterTable shapes the catalog's table. A DEFAULT column is added only when
// some cluster is not its region's default: that is the one catalog in which a
// reader needs telling where a region falls back to when a command names the
// region alone (`repo create`, whose home cluster is its project's region), and
// the only one in which the column would not read yes on every row. Every
// consumer of isDefault picks the default within one jurisdiction, so the
// column is read per region.
func clusterTable(clusters []coreapi.Cluster) ([]string, func(coreapi.Cluster) []string) {
	if !slices.ContainsFunc(clusters, func(cl coreapi.Cluster) bool { return !cl.IsDefault }) {
		return clusterColumns, clusterRow
	}
	headers := append(slices.Clone(clusterColumns), "DEFAULT")
	return headers, func(cl coreapi.Cluster) []string {
		mark := "-"
		if cl.IsDefault {
			mark = "yes"
		}
		return append(clusterRow(cl), mark)
	}
}

// clusterJSON is the --json view of the catalog: the wire model with a
// synthesized `host` merged into each cluster — the same validated bare host
// the table's HOST column shows and that every targeting `--cluster` takes —
// so a script reads the safe value instead of re-implementing
// hostFromPublicURL over publicUrl. Where
// publicUrl fails validation the field is absent, not dashed: publicUrl stays
// for the consumer that wants the raw value, and an absent host says
// "unsafe" more honestly than a placeholder does.
func clusterJSON(clusters []coreapi.Cluster) (any, error) {
	out := make([]map[string]json.RawMessage, 0, len(clusters))
	for i := range clusters {
		cl := &clusters[i]
		obj, err := mergeSynthesizedField(cl, "host", func() string {
			host, err := hostFromPublicURL(cl.PublicUrl)
			if err != nil {
				return ""
			}
			return host
		})
		if err != nil {
			return nil, err
		}
		out = append(out, obj)
	}
	return out, nil
}

// sortClusters orders the catalog for reading — by region, then by slug. The
// server returns registry order.
func sortClusters(clusters []coreapi.Cluster) {
	slices.SortFunc(clusters, func(a, b coreapi.Cluster) int {
		return cmp.Or(
			cmp.Compare(a.Jurisdiction, b.Jurisdiction),
			cmp.Compare(a.Slug, b.Slug),
		)
	})
}

func newClusterListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   cmdList,
		Short: "List the clusters Entire has available",
		Example: "  entire cluster list\n" +
			"  entire cluster list --json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			view := listView[coreapi.Cluster]{table: clusterTable, toJSON: clusterJSON}
			return runCoreListShaped(cmd, "No clusters found.", view, func(ctx context.Context, c *coreapi.Client) ([]coreapi.Cluster, error) {
				out, err := c.ListClusters(ctx)
				if err != nil {
					return nil, err
				}
				sortClusters(out.Clusters)
				return out.Clusters, nil
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}
