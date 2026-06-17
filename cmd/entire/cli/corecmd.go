package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
)

// addControlPlaneFlags registers the persistent flags shared by every
// control-plane command group. Persistent so they're inherited by nested
// subcommands (e.g. `entire repo mirror list`):
//   - --json: emit the raw wire JSON instead of the default human table.
//   - --insecure-http-auth: permit the token exchange over plain http://
//     (local/dev deployments where the core isn't behind TLS). Hidden, as
//     elsewhere in the CLI.
func addControlPlaneFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().Bool("json", false, "output raw JSON instead of a table")
	cmd.PersistentFlags().Bool("insecure-http-auth", false, "Allow authentication over plain HTTP (insecure, for local development only)")
	if err := cmd.PersistentFlags().MarkHidden("insecure-http-auth"); err != nil {
		panic(fmt.Sprintf("hide insecure-http-auth flag: %v", err))
	}
}

// jsonRequested reports whether --json was set on cmd or an ancestor. A
// lookup error means the flag isn't defined on this command tree, which is
// treated as "not requested".
func jsonRequested(cmd *cobra.Command) bool {
	v, err := cmd.Flags().GetBool("json")
	return err == nil && v
}

// insecureHTTPRequested reports whether --insecure-http-auth was set on cmd
// or an ancestor.
func insecureHTTPRequested(cmd *cobra.Command) bool {
	v, err := cmd.Flags().GetBool("insecure-http-auth")
	return err == nil && v
}

// runCoreList fetches a slice via fn and renders it as an aligned table
// (default) or the raw wire JSON (--json). headers names the columns; row
// maps one item to its cells in the same order. The human view keeps the
// output actionable — only the columns a person acts on — while --json
// preserves the full model for scripting.
func runCoreList[T any](cmd *cobra.Command, headers []string, row func(T) []string, fn func(ctx context.Context, c *coreapi.Client) ([]T, error)) error {
	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		items, err := fn(ctx, c)
		if err != nil {
			return err
		}
		if jsonRequested(cmd) {
			return printJSON(cmd.OutOrStdout(), items)
		}
		if len(items) == 0 {
			fmt.Fprintln(cmd.ErrOrStderr(), "(none)")
			return nil
		}
		return printTable(cmd.OutOrStdout(), headers, items, row)
	})
}

// runCoreObject fetches a single value via fn and renders it as a vertical
// field/value list (default) or raw JSON (--json), reusing the same column
// definition as the matching list view.
func runCoreObject[T any](cmd *cobra.Command, headers []string, row func(T) []string, fn func(ctx context.Context, c *coreapi.Client) (*T, error)) error {
	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		item, err := fn(ctx, c)
		if err != nil {
			return err
		}
		if jsonRequested(cmd) {
			return printJSON(cmd.OutOrStdout(), item)
		}
		return printFields(cmd.OutOrStdout(), headers, row(*item))
	})
}

// tableStyles holds the foreground styles for the human table/field views,
// matching the activity/session palette: gray ("8") for headers and
// secondary cells, white ("7") for the primary (first-column) value. When
// color is disabled (non-TTY, NO_COLOR — e.g. piped output and tests) the
// styles are no-ops and output is plain.
type tableStyles struct {
	enabled bool
	header  lipgloss.Style
	primary lipgloss.Style
	cell    lipgloss.Style
}

func newTableStyles(w io.Writer) tableStyles {
	if !shouldUseColor(w) {
		return tableStyles{}
	}
	return tableStyles{
		enabled: true,
		header:  lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true),
		primary: lipgloss.NewStyle().Foreground(lipgloss.Color("7")),
		cell:    lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
	}
}

// style applies s to text only when color is enabled; otherwise it returns
// text unchanged so padding math and plain output stay correct.
func (t tableStyles) style(s lipgloss.Style, text string) string {
	if !t.enabled {
		return text
	}
	return s.Render(text)
}

// columnStyle picks the foreground for a data cell: the first column is the
// primary identifier (white), the rest are secondary (gray).
func (t tableStyles) columnStyle(col int) lipgloss.Style {
	if col == 0 {
		return t.primary
	}
	return t.cell
}

// printTable writes headers plus one row per item, columns aligned on the
// plain-text widths so ANSI color codes don't throw off the layout (they're
// applied after padding). Callers handle the empty case.
func printTable[T any](w io.Writer, headers []string, items []T, row func(T) []string) error {
	st := newTableStyles(w)
	rows := make([][]string, len(items))
	for i, it := range items {
		rows[i] = row(it)
	}
	widths := columnWidths(headers, rows)

	var b strings.Builder
	writeTableRow(&b, headers, widths, func(int) lipgloss.Style { return st.header }, st)
	for _, r := range rows {
		writeTableRow(&b, r, widths, st.columnStyle, st)
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("render table: %w", err)
	}
	return nil
}

// printFields writes a single record as aligned "FIELD  value" lines: the
// label in header gray, the value in the same primary/secondary color the
// list view would give that column.
func printFields(w io.Writer, headers, values []string) error {
	st := newTableStyles(w)
	labelWidth := 0
	for _, h := range headers {
		if n := lipgloss.Width(h); n > labelWidth {
			labelWidth = n
		}
	}
	var b strings.Builder
	for i, h := range headers {
		var v string
		if i < len(values) {
			v = values[i]
		}
		label := st.style(st.header, h+strings.Repeat(" ", labelWidth-lipgloss.Width(h)))
		b.WriteString(label)
		b.WriteString("  ")
		b.WriteString(st.style(st.columnStyle(i), v))
		b.WriteByte('\n')
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("render fields: %w", err)
	}
	return nil
}

// columnWidths returns the max plain-text width of each column across the
// headers and all rows.
func columnWidths(headers []string, rows [][]string) []int {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = lipgloss.Width(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) && lipgloss.Width(c) > widths[i] {
				widths[i] = lipgloss.Width(c)
			}
		}
	}
	return widths
}

// writeTableRow pads each cell to its column width on the plain text, then
// styles it — so alignment is computed before any ANSI codes are added.
// The final column isn't padded, avoiding trailing whitespace.
func writeTableRow(b *strings.Builder, cells []string, widths []int, styleFor func(col int) lipgloss.Style, st tableStyles) {
	for i, c := range cells {
		last := i == len(cells)-1
		padded := c
		if !last && i < len(widths) {
			padded = c + strings.Repeat(" ", widths[i]-lipgloss.Width(c))
		}
		b.WriteString(st.style(styleFor(i), padded))
		if !last {
			b.WriteString("  ")
		}
	}
	b.WriteByte('\n')
}

// runCoreJSON runs fn against an authenticated control-plane client and
// prints its result as indented JSON. It owns the preamble every
// control-plane command shares: silence usage so input errors don't spam
// the usage block, build the client, and map an API error to a
// problem-detail SilentError. Commands supply only the call + the value to
// render.
func runCoreJSON(cmd *cobra.Command, fn func(ctx context.Context, c *coreapi.Client) (any, error)) error {
	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		out, err := fn(ctx, c)
		if err != nil {
			return err
		}
		return printJSON(cmd.OutOrStdout(), out)
	})
}

// runCore is the variant for commands that don't render JSON (delete,
// revoke, remove): it runs the same preamble — silence usage, build
// client, map API errors — and leaves any success output to fn.
func runCore(cmd *cobra.Command, fn func(ctx context.Context, c *coreapi.Client) error) error {
	cmd.SilenceUsage = true
	// Opt into plain-HTTP token exchange before the client (and its lazily
	// built token manager) is constructed — the manager freezes the
	// setting on first use.
	if insecureHTTPRequested(cmd) {
		auth.EnableInsecureHTTP()
	}
	client, err := coreapi.New()
	if err != nil {
		return fmt.Errorf("connect to Entire control plane: %w", err)
	}
	if err := fn(cmd.Context(), client); err != nil {
		return renderCoreError(err)
	}
	return nil
}

// markRequired marks one or more flags required, panicking if a name
// doesn't exist — that can only happen from a typo at wiring time, never
// at runtime, so a panic surfaces the bug immediately rather than letting
// a "required" flag silently not be enforced.
func markRequired(cmd *cobra.Command, names ...string) {
	for _, name := range names {
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(fmt.Sprintf("mark flag %q required: %v", name, err))
		}
	}
}

// renderCoreError converts a Core API error into the server's
// problem-detail message (so users see "organization name already taken"
// rather than ogen's decode-wrapped string), falling back to the raw error
// for transport/local failures. It returns a plain error, not a
// SilentError: main.go prints plain errors, and runCore has already set
// SilenceUsage, so the message reaches the user without a usage dump. (A
// SilentError here would be swallowed — main.go skips printing those —
// leaving e.g. a 409 conflict with no output.)
func renderCoreError(err error) error {
	if err == nil {
		return nil
	}
	if msg := coreapi.APIError(err); msg != "" {
		return errors.New(msg)
	}
	return err
}

// printJSON writes v as indented JSON to w — the --json view for list/get
// and the default for create commands that echo the new object.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	return nil
}
