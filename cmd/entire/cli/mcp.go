package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

// `entire mcp` runs a Model Context Protocol (MCP) server over stdio so that
// "MCP-host" agents — agents with no entire hook or context-injection channel —
// can reach entire's machine-readable surface as MCP tools. It is the active
// counterpart to the passive `entire status` / `entire help` discovery path: the
// host launches `entire mcp` as a stdio server and calls the agent_help and
// entire_status tools. The server is read-only and reuses the same live
// agent-help / status rendering the CLI uses, so it always matches the installed
// binary. Transport is newline-delimited JSON-RPC 2.0 (the MCP stdio framing).

// mcpProtocolVersion is the MCP revision we advertise when a client doesn't
// request one. We echo the client's requested version when present.
const mcpProtocolVersion = "2025-06-18"

// maxMCPMessageBytes bounds a single newline-delimited JSON-RPC message so a
// malformed or abusive line can't exhaust memory. 1 MiB is far above any real
// agent_help / status request.
const maxMCPMessageBytes = 1 << 20

// mcpServerName is the MCP serverInfo name advertised at initialize.
const mcpServerName = "entire"

func newMCPCmd(rootCmd *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run a Model Context Protocol server for MCP-host agents",
		Long: `Runs a Model Context Protocol (MCP) server over stdio. Configure an MCP host to
launch "entire mcp" as a stdio server; it exposes entire's agent-help and status
as MCP tools so agents without a hook or context-injection channel can discover
and use entire. Read-only; speaks newline-delimited JSON-RPC 2.0.`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runMCPServer(c.Context(), rootCmd, c.InOrStdin(), c.OutOrStdout())
		},
	}
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// runMCPServer reads newline-delimited JSON-RPC 2.0 messages from in (one per
// line — the MCP stdio framing), bounding each line to maxMCPMessageBytes, and
// writes responses to out until EOF. Notifications (messages with no id) get no
// response, per JSON-RPC. An unparseable line — including a JSON-RPC batch array,
// which this server does not support — yields a parse error and the server
// recovers to the next line rather than terminating.
func runMCPServer(ctx context.Context, rootCmd *cobra.Command, in io.Reader, out io.Writer) error {
	enc := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), maxMCPMessageBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}

		var req mcpRequest
		if err := json.Unmarshal(line, &req); err != nil {
			if encErr := enc.Encode(mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &mcpError{Code: -32700, Message: "parse error"}}); encErr != nil {
				return fmt.Errorf("write mcp parse-error response: %w", encErr)
			}
			continue
		}

		// Reject a parseable-but-invalid request (missing/incorrect jsonrpc version
		// or empty method) with -32600 before dispatch, per JSON-RPC, rather than
		// treating it as method-not-found.
		if req.JSONRPC != "2.0" || req.Method == "" {
			id := req.ID
			if len(id) == 0 {
				id = json.RawMessage("null")
			}
			if encErr := enc.Encode(mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: -32600, Message: "invalid request"}}); encErr != nil {
				return fmt.Errorf("write mcp invalid-request response: %w", encErr)
			}
			continue
		}

		result, rpcErr := dispatchMCP(ctx, rootCmd, req.Method, req.Params)

		// A request without an id is a notification: never responded to.
		if len(req.ID) == 0 {
			continue
		}

		resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("write mcp response: %w", err)
		}
	}
	if err := sc.Err(); err != nil {
		// A single line exceeded maxMCPMessageBytes (the scanner can't resynchronize
		// past an over-long token) or the read failed; report once and stop.
		if errors.Is(err, bufio.ErrTooLong) {
			if encErr := enc.Encode(mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &mcpError{Code: -32600, Message: "request too large"}}); encErr != nil {
				return fmt.Errorf("write mcp oversize response: %w", encErr)
			}
			return nil
		}
		return fmt.Errorf("read mcp request: %w", err)
	}
	return nil
}

func dispatchMCP(ctx context.Context, rootCmd *cobra.Command, method string, params json.RawMessage) (any, *mcpError) {
	switch method {
	case "initialize":
		return mcpInitializeResult(params), nil
	case "notifications/initialized":
		return nil, nil // notification; result is ignored by the caller
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpToolDefs()}, nil
	case "tools/call":
		return handleMCPToolCall(ctx, rootCmd, params)
	default:
		return nil, &mcpError{Code: -32601, Message: "method not found: " + method}
	}
}

func mcpInitializeResult(params json.RawMessage) map[string]any {
	protocol := mcpProtocolVersion
	if len(params) > 0 {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
			protocol = p.ProtocolVersion
		}
	}
	return map[string]any{
		"protocolVersion": protocol,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": mcpServerName, "version": versioninfo.Version},
	}
}

func mcpToolDefs() []map[string]any {
	objSchema := func(props map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": props}
	}
	return []map[string]any{
		{
			"name":        "agent_help",
			"description": "Machine-readable usage for the entire CLI, generated live from the installed binary. Omit command for a top-level map of when to use entire and which subcommand; pass a command path to drill into that command's exact, current flags.",
			"inputSchema": objSchema(map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Optional subcommand path, space-separated (e.g. \"checkpoint\" or \"doctor trace\"). Empty for the top-level overview.",
				},
			}),
		},
		{
			"name":        "entire_status",
			"description": "Current entire status for this repo as JSON (enabled, agents, active sessions, agent_help pointer).",
			"inputSchema": objSchema(map[string]any{}),
		},
	}
}

func handleMCPToolCall(ctx context.Context, rootCmd *cobra.Command, params json.RawMessage) (any, *mcpError) {
	var call struct {
		Name      string `json:"name"`
		Arguments struct {
			Command string `json:"command"`
		} `json:"arguments"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &call); err != nil {
			return nil, &mcpError{Code: -32602, Message: "invalid tool call params"}
		}
	}

	if call.Name == "" {
		return nil, &mcpError{Code: -32602, Message: "invalid params: tool name required"}
	}
	switch call.Name {
	case "agent_help":
		args := strings.Fields(call.Arguments.Command)
		// Resolve origin once (mirrors `entire agent-help`): derive both the repo
		// line and the trails-enablement check from a single scope.
		repoLine, trailsEnabled := agentHelpRepoContext(ctx)
		text, err := runAgentHelp(rootCmd, args, repoLine, true, trailsEnabled)
		if err != nil {
			// A bad command path is a tool-level error the agent can recover from,
			// not a protocol error — return it as an isError result.
			return mcpToolErrorResult(err.Error()), nil
		}
		return mcpToolTextResult(text), nil
	case "entire_status":
		var buf bytes.Buffer
		if err := runStatusJSON(ctx, &buf); err != nil {
			return mcpToolErrorResult(err.Error()), nil
		}
		return mcpToolTextResult(buf.String()), nil
	default:
		return nil, &mcpError{Code: -32602, Message: "unknown tool: " + call.Name}
	}
}

func mcpToolTextResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

func mcpToolErrorResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": true,
	}
}
