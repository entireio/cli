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
	"github.com/entireio/cli/internal/coreapi"
)

const apiMaxResponseBytes = 32 << 20 // 32 MiB cap on a printed response body.

type apiFlags struct {
	to           string
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
		Long: "Make an authenticated HTTP request to an Entire API and print the JSON response.\n\n" +
			"The CLI attaches the right bearer token and dials the right host for the\n" +
			"chosen backend, so you don't have to plumb auth yourself:\n\n" +
			"  --to core   the control plane (default): orgs, repos, mirrors, clusters, /me\n" +
			"  --to cell   your home entire-api cell: /me/* activity, repo aggregates\n\n" +
			"<path> is the full path on that host, e.g. /api/v1/clusters. These\n" +
			"placeholders are filled from the current repo's origin remote:\n" +
			"  {owner} {repo}   the GitHub owner / repo\n" +
			"  {repo_id}        the repo's Entire ULID (from its mirror) — cells key on this\n\n" +
			"The method is GET unless a field/body is given (then POST); override with -X.",
		Example: "  entire api /api/v1/clusters\n" +
			"  entire api --to cell /api/v1/me/activity\n" +
			"  entire api --to cell \"/api/v1/me/recap?repo={repo_id}\"\n" +
			"  entire api -X POST /api/v1/projects -f name=demo",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAPI(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], f)
		},
	}
	cmd.Flags().StringVar(&f.to, "to", "core", "which backend to call: core or cell")
	cmd.Flags().StringVarP(&f.method, "method", "X", "", "HTTP method (default GET, or POST when a field/body is given)")
	cmd.Flags().StringArrayVarP(&f.rawFields, "raw-field", "f", nil, "add a string parameter in key=value format (repeatable)")
	cmd.Flags().StringArrayVarP(&f.typedFields, "field", "F", nil, "add a typed parameter in key=value format; true/false/null/numbers are converted (repeatable)")
	cmd.Flags().StringArrayVarP(&f.headers, "header", "H", nil, "add a request header in key:value format (repeatable)")
	cmd.Flags().StringVar(&f.input, "input", "", "file with the request body (\"-\" for stdin)")
	cmd.Flags().BoolVarP(&f.include, "include", "i", false, "print the response status line and headers too")
	addInsecureHTTPAuthFlag(cmd, &f.insecureHTTP)
	return cmd
}

func runAPI(ctx context.Context, w, errW io.Writer, rawPath string, f *apiFlags) error {
	insecure := applyInsecureHTTPAuth(f.insecureHTTP)

	client, err := resolveAPIClient(ctx, f.to, insecure)
	if err != nil {
		return err
	}

	path, err := expandAPIPlaceholders(ctx, rawPath)
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

// resolveAPIClient builds an authenticated client for the chosen backend. Both
// return an *api.Client whose base URL is the backend origin, so <path> is the
// full path (e.g. /api/v1/…) against that host.
func resolveAPIClient(ctx context.Context, to string, insecure bool) (*api.Client, error) {
	switch strings.ToLower(strings.TrimSpace(to)) {
	case "", "core":
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
		return api.NewClientWithBaseURL(target.token, target.coreURL), nil
	case "cell":
		client, err := auth.NewEntireAPICellClient(ctx, insecure, nil)
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

// expandAPIPlaceholders fills {owner}/{repo}/{repo_id} from the current repo.
// Resolution is lazy: {owner}/{repo} need only the git remote; {repo_id} costs
// a control-plane mirror lookup, so it's only done when actually referenced.
func expandAPIPlaceholders(ctx context.Context, s string) (string, error) {
	needOwnerRepo := strings.Contains(s, "{owner}") || strings.Contains(s, "{repo}")
	needRepoID := strings.Contains(s, "{repo_id}")
	if !needOwnerRepo && !needRepoID {
		return s, nil
	}

	// Resolve the origin remote once, even when both {owner}/{repo} and
	// {repo_id} are present, and thread it into the mirror lookup.
	forge, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return "", fmt.Errorf("resolve current repo from origin remote: %w", err)
	}
	if needOwnerRepo {
		s = strings.ReplaceAll(s, "{owner}", owner)
		s = strings.ReplaceAll(s, "{repo}", repo)
	}
	if needRepoID {
		id, err := resolveCurrentRepoID(ctx, forge, owner, repo)
		if err != nil {
			return "", err
		}
		s = strings.ReplaceAll(s, "{repo_id}", id)
	}
	return s, nil
}

// resolveCurrentRepoID resolves a repo's Entire ULID from its mirror (the mirror
// id, which entire-api uses as the repo_id), given its already-resolved origin
// coordinates. Picks the first active placement.
func resolveCurrentRepoID(ctx context.Context, forge, owner, repo string) (string, error) {
	if f := strings.ToLower(strings.TrimSpace(forge)); f != "gh" && f != mirrorCloneProviderGitHub {
		return "", fmt.Errorf("{repo_id} needs a GitHub repo; origin forge is %q", forge)
	}
	c, err := coreapi.New()
	if err != nil {
		return "", fmt.Errorf("resolve {repo_id} needs the control plane: %w", err)
	}
	mirrors, err := listMirrorsForRepo(ctx, c, mirrorCloneProviderGitHub, strings.ToLower(owner), strings.ToLower(repo))
	if err != nil {
		return "", fmt.Errorf("list mirrors for %s/%s: %w", owner, repo, err)
	}
	for i := range mirrors {
		if !isActiveMirror(mirrors[i]) {
			continue
		}
		if id := strings.TrimSpace(mirrors[i].MirrorId); id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("no active mirror for %s/%s to resolve {repo_id}", owner, repo)
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
