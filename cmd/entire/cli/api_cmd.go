package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

const apiMaxResponseBytes = 32 << 20 // 32 MiB cap on a printed response body.

// The two --to backends: the control plane (core) and a per-jurisdiction
// entire-api cell.
const (
	apiTargetCore = "core"
	apiTargetCell = "cell"
)

type apiFlags struct {
	to           string
	jurisdiction string
	method       string
	rawFields    []string
	typedFields  []string
	headers      []string
	input        string
	include      bool
	insecureHTTP bool
}

func newAPICmd() *cobra.Command {
	f := &apiFlags{}
	cmd := &cobra.Command{
		Use:   "api <path>",
		Short: "Make an authenticated request to an Entire API and print the response",
		// Human-facing help stays a reference, not a lecture: someone who typed
		// this command chose it deliberately. The one caveat kept here is a real
		// compatibility fact rather than advice — raw endpoints are internal. The
		// "prefer a first-class command" steer is agent-facing guidance and lives
		// in agentHelpGuidance, which only agent-help renders.
		Long: "Make an authenticated HTTP request to an Entire API and print the JSON response.\n\n" +
			"These endpoints are internal and can change shape without notice; where a\n" +
			"purpose-built command exists, its output is the stable interface.\n\n" +
			"The CLI attaches the right bearer token and dials the right host for the\n" +
			"chosen backend, so you don't have to plumb auth yourself:\n\n" +
			"  --to core   the control plane (default): orgs, repos, mirrors, clusters, /me\n" +
			"  --to cell   your home entire-api cell: /me/* activity, repo aggregates\n\n" +
			"Use --jurisdiction <slug> (e.g. us, eu) to reach a specific jurisdiction's\n" +
			"entire-api cell instead of your home one; it implies --to cell.\n\n" +
			"Both backends act as ENTIRE_TOKEN when set, else the selected login\n" +
			"(--context, else $ENTIRE_CONTEXT, else the active context), so a staging\n" +
			"login reaches staging's control plane and cells. The cell host comes from\n" +
			"environment's cluster listing; an explicit ENTIRE_API_BASE_URL names the\n" +
			"data host directly instead.\n\n" +
			"<path> is the full path on that host, e.g. /api/v1/clusters. These\n" +
			"placeholders are filled from the current repo's origin remote:\n" +
			"  {owner} {repo}   the GitHub owner / repo\n" +
			"  {repo_id}        the repo's Entire ULID (its processing placement) — cells key on this\n\n" +
			"With --to cell, a request whose path names {repo_id} goes to the cell that\n" +
			"hosts the repo instead of your home cell. /me/* paths and {repo_id} in the\n" +
			"query string stay on the home cell; --jurisdiction, and an ENTIRE_API_BASE_URL\n" +
			"naming a direct cell or local host, always win.\n\n" +
			"The method is GET unless a field/body is given (then POST); override with -X.",
		Example: "  entire api /api/v1/clusters\n" +
			"  entire api --to cell /api/v1/me/activity\n" +
			"  entire api --jurisdiction eu /api/v1/me/activity\n" +
			"  entire api --to cell \"/api/v1/me/recap?repo={repo_id}\"\n" +
			"  entire api -X POST /api/v1/projects -f name=demo",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAPI(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], f, cmd.Flags().Changed("to"))
		},
	}
	cmd.Flags().StringVar(&f.to, "to", apiTargetCore, "which backend to call: core or cell")
	cmd.Flags().StringVarP(&f.jurisdiction, "jurisdiction", "j", "", "target a specific jurisdiction's cell (e.g. us, eu) instead of your home cell; implies --to cell")
	cmd.Flags().StringVarP(&f.method, "method", "X", "", "HTTP method (default GET, or POST when a field/body is given)")
	cmd.Flags().StringArrayVarP(&f.rawFields, "raw-field", "f", nil, "add a string parameter in key=value format (repeatable)")
	cmd.Flags().StringArrayVarP(&f.typedFields, "field", "F", nil, "add a typed parameter in key=value format; true/false/null/numbers are converted (repeatable)")
	cmd.Flags().StringArrayVarP(&f.headers, "header", "H", nil, "add a request header in key:value format (repeatable)")
	cmd.Flags().StringVar(&f.input, "input", "", "file with the request body (\"-\" for stdin)")
	cmd.Flags().BoolVarP(&f.include, "include", "i", false, "print the response status line and headers too")
	addInsecureHTTPAuthFlag(cmd, &f.insecureHTTP)
	return cmd
}

func runAPI(ctx context.Context, w, errW io.Writer, rawPath string, f *apiFlags, toExplicit bool) error {
	insecure := applyInsecureHTTPAuth(f.insecureHTTP)

	to, jurisdiction, err := resolveAPITarget(f, toExplicit)
	if err != nil {
		return err
	}

	client, path, err := resolveAPIClientAndPath(ctx, to, jurisdiction, insecure, rawPath)
	if err != nil {
		return err
	}
	if err := validateAPIPath(path); err != nil {
		return err
	}

	fields, err := buildAPIFields(f.rawFields, f.typedFields)
	if err != nil {
		return err
	}

	req, err := buildAPIRequestBody(path, f, fields)
	if err != nil {
		return err
	}

	headers, err := parseAPIHeaders(f.headers)
	if err != nil {
		return err
	}

	resp, err := client.Request(ctx, req.method, req.path, headers, req.body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", req.method, req.path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	return writeAPIResponse(w, errW, resp, f.include)
}

// resolveAPITarget applies the --jurisdiction/--to interplay. --jurisdiction
// selects a specific jurisdiction's entire-api cell, so it implies --to cell;
// combining it with an explicit --to core is a contradiction and is rejected.
// The returned jurisdiction is normalized to a bare lowercase slug (e.g. "US"
// or " us " -> "us"); NewEntireAPICellClient validates it as a DNS label.
func resolveAPITarget(f *apiFlags, toExplicit bool) (to, jurisdiction string, err error) {
	to = f.to
	jurisdiction = strings.ToLower(strings.TrimSpace(f.jurisdiction))
	if jurisdiction == "" {
		return to, "", nil
	}
	switch {
	case !toExplicit:
		to = apiTargetCell // --jurisdiction targets a cell; imply it over the default core.
	case strings.ToLower(strings.TrimSpace(f.to)) != apiTargetCell:
		return "", "", fmt.Errorf("--jurisdiction targets a cell; use --to cell (not --to %s)", f.to)
	}
	return to, jurisdiction, nil
}

// newAPIBackendClient is a package seam so tests can observe which backend a
// request selects without dialing it.
var newAPIBackendClient = resolveAPIClient

// resolveAPIClient builds an authenticated client for the chosen backend. Both
// return an *api.Client whose base URL is the backend origin, so <path> is the
// full path (e.g. /api/v1/…) against that host. jurisdiction, when non-empty,
// pins the entire-api cell to that jurisdiction (cell path only; resolveAPITarget
// guarantees it is empty for core).
func resolveAPIClient(ctx context.Context, to, jurisdiction string, insecure bool) (*api.Client, error) {
	switch strings.ToLower(strings.TrimSpace(to)) {
	case "", apiTargetCore:
		target, err := resolveAuthStatusTarget(ctx, auth.Contexts, auth.RefreshedLoginToken)
		if err != nil {
			return nil, err
		}
		if target.token == "" {
			// Return a normal (non-silent) error so main.go prints the hint;
			// a SilentError would exit non-zero with no explanation.
			return nil, errors.New("not logged in: run 'entire login' to authenticate")
		}
		if !insecure && target.coreURL != "" {
			if err := api.RequireSecureURL(target.coreURL); err != nil {
				return nil, fmt.Errorf("control-plane URL check: %w", err)
			}
		}
		// Stderr, so the response body on stdout stays clean.
		auth.AnnounceContext(target.totalContexts, target.activeContext)
		return api.NewClientWithBaseURL(target.token, target.coreURL), nil
	case apiTargetCell:
		var target *auth.CellTarget
		if jurisdiction != "" {
			target = &auth.CellTarget{Jurisdiction: jurisdiction}
		}
		client, err := auth.NewEntireAPICellClient(ctx, insecure, target)
		if err != nil {
			return nil, err //nolint:wrapcheck // NewEntireAPICellClient already returns contextual auth errors
		}
		return client, nil
	default:
		return nil, fmt.Errorf("unknown --to %q (use core or cell)", to)
	}
}

// validateAPIPath rejects anything that isn't origin-relative. The path is
// resolved against the backend origin via url.ResolveReference, which lets an
// absolute ("https://evil/…") or scheme-relative ("//evil/…") value replace the
// host — and the bearer transport would then send the Entire token there. So a
// path carrying its own scheme or host is refused before any request is made.
func validateAPIPath(path string) error {
	u, err := url.Parse(path)
	if err != nil {
		return fmt.Errorf("invalid path %q: %w", path, err)
	}
	if u.Scheme != "" || u.Host != "" {
		return fmt.Errorf("path must be origin-relative (e.g. /api/v1/…), not a cross-host or absolute URL: %q", path)
	}
	return nil
}

// apiOrigin is the current repo's origin remote, resolved once per request for
// every placeholder that needs it.
type apiOrigin struct {
	forge, owner, repo string
}

// resolveAPIClientAndPath builds the request's client and fills the path's
// placeholders. They are resolved together because a repo-scoped cell request
// takes both from one lookup: {repo_id} is the repo's processing placement,
// and only the cell hosting that placement can answer for it. Any other cell
// either rejects the id or, for the processing id, answers with an empty
// result, so routing such a request to the home cell would look like a repo
// with no data.
func resolveAPIClientAndPath(ctx context.Context, to, jurisdiction string, insecure bool, rawPath string) (*api.Client, string, error) {
	needOwnerRepo := strings.Contains(rawPath, "{owner}") || strings.Contains(rawPath, "{repo}")
	needRepoID := strings.Contains(rawPath, "{repo_id}")

	var origin apiOrigin
	if needOwnerRepo || needRepoID {
		forge, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
		if err != nil {
			return nil, "", fmt.Errorf("resolve current repo from origin remote: %w", err)
		}
		origin = apiOrigin{forge: forge, owner: owner, repo: repo}
	}

	// An explicit --jurisdiction, or an ENTIRE_API_BASE_URL naming a direct
	// cell or loopback host, is the user's choice of cell and wins; the
	// placement then supplies only the id. Rerouting past it would send a
	// request meant for a local or direct cell, mutations included, to the
	// hosted one. An override naming a BFF/apex still routes by catalog, so it
	// does not pin a cell.
	if needRepoID && jurisdiction == "" && !auth.BaseURLOverridePinsCell() && isAPICellTarget(to) && isRepoScopedAPIPath(rawPath) {
		if err := checkAPIRepoIDForge(origin.forge); err != nil {
			return nil, "", err
		}
		client, repoID, err := newTrailAPIClient(ctx, insecure, origin.forge, origin.owner, origin.repo)
		if err != nil {
			return nil, "", fmt.Errorf("resolve {repo_id}: %w", err)
		}
		return client, expandAPIPlaceholders(rawPath, origin, repoID), nil
	}

	client, err := newAPIBackendClient(ctx, to, jurisdiction, insecure)
	if err != nil {
		return nil, "", err
	}
	var repoID string
	if needRepoID {
		if repoID, err = resolveAPIRepoID(ctx, origin); err != nil {
			return nil, "", err
		}
	}
	return client, expandAPIPlaceholders(rawPath, origin, repoID), nil
}

func isAPICellTarget(to string) bool {
	return strings.ToLower(strings.TrimSpace(to)) == apiTargetCell
}

// isRepoScopedAPIPath reports whether {repo_id} names the resource the request
// addresses, which lives on the repo's cell. A {repo_id} in the query string
// only filters, and /me/* is served by the caller's home cell, so neither
// changes the cell (e.g. /api/v1/me/recap?repo={repo_id}).
func isRepoScopedAPIPath(rawPath string) bool {
	p, _, _ := strings.Cut(rawPath, "?")
	if !strings.Contains(p, "{repo_id}") {
		return false
	}
	return p != "/api/v1/me" && !strings.HasPrefix(p, "/api/v1/me/")
}

// checkAPIRepoIDForge rejects an origin that is neither a GitHub nor an Entire
// repo before any control-plane lookup, with a message that names the cause.
func checkAPIRepoIDForge(forge string) error {
	if forge != mirrorCloneForge && forge != nativeCloneForge {
		return fmt.Errorf("{repo_id} needs a GitHub or Entire repo; origin forge is %q", forge)
	}
	return nil
}

// resolveAPIRepoID resolves the repo's processing placement id, the value
// entire-api uses as repo_id. A repo mirrored in several regions has a
// different id per placement, and only the processing one holds its data, so
// a lookup failure is an error rather than a fallback to another mirror.
func resolveAPIRepoID(ctx context.Context, origin apiOrigin) (string, error) {
	if err := checkAPIRepoIDForge(origin.forge); err != nil {
		return "", err
	}
	placement, err := resolveForgeRepoCellPlacement(ctx, origin.forge, origin.owner, origin.repo)
	if err != nil {
		return "", fmt.Errorf("resolve {repo_id}: %w", err)
	}
	return placement.RepoID, nil
}

// expandAPIPlaceholders fills {owner}/{repo}/{repo_id} from already-resolved
// values; resolveAPIClientAndPath only resolves what the path references.
func expandAPIPlaceholders(s string, origin apiOrigin, repoID string) string {
	return strings.NewReplacer(
		"{owner}", origin.owner,
		"{repo}", origin.repo,
		"{repo_id}", repoID,
	).Replace(s)
}

// buildAPIFields merges -f (always string) and -F (type-inferred) into one map.
// Returns an empty (non-nil) map when no fields are given.
func buildAPIFields(rawFields, typedFields []string) (map[string]any, error) {
	out := make(map[string]any, len(rawFields)+len(typedFields))
	for _, kv := range rawFields {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid -f %q (want key=value)", kv)
		}
		out[k] = v
	}
	for _, kv := range typedFields {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid -F %q (want key=value)", kv)
		}
		out[k] = inferFieldValue(v)
	}
	return out, nil
}

// inferFieldValue mirrors gh's -F conversion: true/false/null and numbers get
// their JSON type; everything else stays a string.
func inferFieldValue(v string) any {
	switch v {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	if fl, err := strconv.ParseFloat(v, 64); err == nil {
		return fl
	}
	return v
}

type apiRequest struct {
	method string
	path   string
	body   io.Reader
}

// buildAPIRequestBody resolves method + body from the flags: --input wins (raw
// body), else fields become a JSON body. The method defaults to GET, or POST
// when there's a body; an explicit -X wins. For a GET with fields, the fields
// go on the query string instead of a body.
func buildAPIRequestBody(path string, f *apiFlags, fields map[string]any) (apiRequest, error) {
	method := strings.ToUpper(strings.TrimSpace(f.method))
	if method == "" {
		if f.input != "" || len(fields) > 0 {
			method = http.MethodPost
		} else {
			method = http.MethodGet
		}
	}

	if f.input != "" {
		if len(fields) > 0 {
			return apiRequest{}, errors.New("use either --input or -f/-F, not both")
		}
		raw, err := readAPIInput(f.input)
		if err != nil {
			return apiRequest{}, err
		}
		return apiRequest{method: method, path: path, body: bytes.NewReader(raw)}, nil
	}

	if len(fields) == 0 {
		return apiRequest{method: method, path: path, body: nil}, nil
	}

	if method == http.MethodGet {
		q, err := fieldsToQuery(fields)
		if err != nil {
			return apiRequest{}, err
		}
		return apiRequest{method: method, path: appendQuery(path, q), body: nil}, nil
	}

	data, err := json.Marshal(fields)
	if err != nil {
		return apiRequest{}, fmt.Errorf("encode fields: %w", err)
	}
	return apiRequest{method: method, path: path, body: bytes.NewReader(data)}, nil
}

// fieldsToQuery stringifies fields for a GET query string. Non-string scalars
// are rendered with their JSON form (e.g. true, 42) so -F round-trips sensibly.
func fieldsToQuery(fields map[string]any) (url.Values, error) {
	q := make(url.Values, len(fields))
	for k, v := range fields {
		switch val := v.(type) {
		case string:
			q.Set(k, val)
		case nil:
			q.Set(k, "null")
		default:
			b, err := json.Marshal(val)
			if err != nil {
				return nil, fmt.Errorf("encode field %q: %w", k, err)
			}
			q.Set(k, string(b))
		}
	}
	return q, nil
}

// appendQuery adds encoded query params to a path that may already have some.
func appendQuery(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + q.Encode()
}

// readWithinLimit reads r up to limit bytes and errors if the source is larger,
// rather than silently truncating — a truncated JSON body would be malformed
// and misleading for an escape-hatch command.
func readWithinLimit(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("exceeds the %d-byte limit", limit)
	}
	return raw, nil
}

func readAPIInput(path string) ([]byte, error) {
	if path == "-" {
		raw, err := readWithinLimit(os.Stdin, apiMaxResponseBytes)
		if err != nil {
			return nil, fmt.Errorf("read body from stdin: %w", err)
		}
		return raw, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // a user-specified request-body file is the whole point
	if err != nil {
		return nil, fmt.Errorf("read body file: %w", err)
	}
	return raw, nil
}

func parseAPIHeaders(headers []string) (http.Header, error) {
	h := make(http.Header, len(headers))
	for _, kv := range headers {
		k, v, ok := strings.Cut(kv, ":")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("invalid -H %q (want key:value)", kv)
		}
		h.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	return h, nil
}

func writeAPIResponse(w, errW io.Writer, resp *http.Response, include bool) error {
	if include {
		fmt.Fprintf(errW, "%s %s\n", resp.Proto, resp.Status)
		names := make([]string, 0, len(resp.Header))
		for name := range resp.Header {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for _, v := range resp.Header[name] {
				fmt.Fprintf(errW, "%s: %s\n", name, v)
			}
		}
		fmt.Fprintln(errW)
	}

	raw, err := readWithinLimit(resp.Body, apiMaxResponseBytes)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	toWrite := raw
	if body := bytes.TrimSpace(raw); len(body) > 0 && json.Valid(body) {
		var pretty bytes.Buffer
		if json.Indent(&pretty, body, "", "  ") == nil {
			pretty.WriteByte('\n')
			toWrite = pretty.Bytes()
		}
	}
	if _, err := w.Write(toWrite); err != nil {
		return fmt.Errorf("write response: %w", err)
	}

	// Non-2xx: the body is already printed; return a silent error so the exit
	// code is non-zero for scripts without reprinting anything.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return NewSilentError(fmt.Errorf("HTTP %d", resp.StatusCode))
	}
	return nil
}
