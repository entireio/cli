package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type mcpTestResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// driveMCP runs the server against a sequence of newline-delimited requests and
// returns the decoded responses (notifications produce none).
func driveMCP(t *testing.T, requests ...string) []mcpTestResp {
	t.Helper()
	root := NewRootCmd()
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	var out bytes.Buffer
	if err := runMCPServer(context.Background(), root, in, &out); err != nil {
		t.Fatalf("runMCPServer: %v", err)
	}
	var resps []mcpTestResp
	dec := json.NewDecoder(&out)
	for dec.More() {
		var r mcpTestResp
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode response: %v (raw: %s)", err, out.String())
		}
		resps = append(resps, r)
	}
	return resps
}

func mcpResultText(t *testing.T, result json.RawMessage) string {
	t.Helper()
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &res); err != nil {
		t.Fatalf("unmarshal tool result content: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("tool result had no content: %s", result)
	}
	return res.Content[0].Text
}

// The MCP handshake: initialize echoes the client's protocolVersion and returns
// serverInfo + the tools capability; the `notifications/initialized` notification
// gets no response.
func TestMCPServer_InitializeHandshake(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response (the notification is silent), got %d", len(resps))
	}
	var res struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resps[0].Result, &res); err != nil {
		t.Fatalf("unmarshal initialize result: %v", err)
	}
	if res.ServerInfo.Name != mcpServerName {
		t.Errorf("serverInfo.name = %q, want %q", res.ServerInfo.Name, mcpServerName)
	}
	if res.ProtocolVersion != "2024-11-05" {
		t.Errorf("initialize should echo the client's protocolVersion, got %q", res.ProtocolVersion)
	}
	if _, ok := res.Capabilities["tools"]; !ok {
		t.Errorf("initialize result should advertise the tools capability, got %v", res.Capabilities)
	}
}

// When the client omits protocolVersion, the server advertises its own default.
func TestMCPServer_InitializeDefaultProtocol(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(resps[0].Result, &res); err != nil {
		t.Fatalf("unmarshal initialize result: %v", err)
	}
	if res.ProtocolVersion != mcpProtocolVersion {
		t.Errorf("default protocolVersion = %q, want %q", res.ProtocolVersion, mcpProtocolVersion)
	}
}

// A non-string protocolVersion is ignored gracefully; the server falls back to
// its default instead of erroring.
func TestMCPServer_InitializeBadProtocolType(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":123}}`)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(resps[0].Result, &res); err != nil {
		t.Fatalf("unmarshal initialize result: %v", err)
	}
	if res.ProtocolVersion != mcpProtocolVersion {
		t.Errorf("bad protocolVersion type should fall back to default %q, got %q", mcpProtocolVersion, res.ProtocolVersion)
	}
}

func TestMCPServer_Ping(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t, `{"jsonrpc":"2.0","id":8,"method":"ping"}`)
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("ping should return exactly one non-error response, got %+v", resps)
	}
}

func TestMCPServer_ToolsList(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	var res struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resps[0].Result, &res); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range res.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"agent_help", "entire_status"} {
		if !names[want] {
			t.Errorf("tools/list missing %q; got %v", want, names)
		}
	}
}

// The agent_help tool returns the live agent-help JSON document (asJSON=true),
// reusing the same renderer the CLI uses.
func TestMCPServer_AgentHelpToolCall(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"agent_help","arguments":{"command":""}}}`)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	var doc struct {
		Command     string `json:"command"`
		Subcommands []struct {
			Name string `json:"name"`
		} `json:"subcommands"`
	}
	if err := json.Unmarshal([]byte(mcpResultText(t, resps[0].Result)), &doc); err != nil {
		t.Fatalf("agent_help tool should return the agent-help JSON document: %v", err)
	}
	if doc.Command != NewRootCmd().Name() {
		t.Errorf("top-level agent_help should be the root command document, got command=%q", doc.Command)
	}
	if len(doc.Subcommands) == 0 {
		t.Error("top-level agent_help should list subcommands, got none")
	}
}

// Drilling into a command path returns that command's document.
func TestMCPServer_AgentHelpToolCall_Subcommand(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"agent_help","arguments":{"command":"status"}}}`)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	var doc struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(mcpResultText(t, resps[0].Result)), &doc); err != nil {
		t.Fatalf("agent_help subcommand should return JSON: %v", err)
	}
	if doc.Command != "entire status" {
		t.Errorf("agent_help command=status should drill into `entire status`, got %q", doc.Command)
	}
}

// A bad command path is surfaced as a tool error (isError), not a protocol error,
// so the agent can recover.
func TestMCPServer_AgentHelpUnknownCommandIsToolError(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"agent_help","arguments":{"command":"definitely-not-a-command"}}}`)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	var res struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resps[0].Result, &res); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	if !res.IsError {
		t.Errorf("unknown command should be a tool error (isError=true); result: %s", resps[0].Result)
	}
}

// The entire_status tool must stay byte-identical to the passive `status --json`
// surface (runStatusJSON) — it is the same data over a different transport.
func TestMCPServer_EntireStatusMatchesPassive(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t, `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"entire_status"}}`)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	text := mcpResultText(t, resps[0].Result)
	var direct bytes.Buffer
	if err := runStatusJSON(context.Background(), &direct); err != nil {
		t.Fatalf("runStatusJSON: %v", err)
	}
	if strings.TrimSpace(text) != strings.TrimSpace(direct.String()) {
		t.Errorf("entire_status MCP tool must match runStatusJSON\n tool:   %s\n direct: %s", text, direct.String())
	}
}

// entire_status is a shipped path: it must return parseable status JSON.
func TestMCPServer_EntireStatusToolCall(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"entire_status","arguments":{}}}`)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	text := mcpResultText(t, resps[0].Result)
	var status struct {
		Enabled        *bool    `json:"enabled"`
		Agents         []string `json:"agents"`
		ActiveSessions []any    `json:"active_sessions"`
		AgentHelp      string   `json:"agent_help"`
		Error          string   `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &status); err != nil {
		t.Fatalf("entire_status tool should return status JSON, got %q (err %v)", text, err)
	}
}

func TestMCPServer_EmptyToolName(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":""}}`)
	if len(resps) != 1 || resps[0].Error == nil || resps[0].Error.Code != -32602 {
		t.Fatalf("empty tool name should return invalid-params (-32602), got %+v", resps)
	}
}

func TestMCPServer_UnknownToolIsProtocolError(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"no-such-tool"}}`)
	if len(resps) != 1 || resps[0].Error == nil {
		t.Fatalf("expected one error response for an unknown tool, got %+v", resps)
	}
}

func TestMCPServer_UnknownMethodReturnsError(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t, `{"jsonrpc":"2.0","id":6,"method":"bogus/method"}`)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	if resps[0].Error == nil || resps[0].Error.Code != -32601 {
		t.Errorf("expected method-not-found (-32601), got: %+v", resps[0].Error)
	}
}

// A parseable-but-invalid request (missing method or wrong jsonrpc version) is
// rejected with -32600 before dispatch, not treated as method-not-found.
func TestMCPServer_InvalidRequest(t *testing.T) {
	t.Parallel()
	for _, req := range []string{
		`{"jsonrpc":"2.0","id":1}`,                 // missing method
		`{"jsonrpc":"1.0","id":2,"method":"ping"}`, // wrong jsonrpc version
	} {
		resps := driveMCP(t, req)
		if len(resps) != 1 || resps[0].Error == nil || resps[0].Error.Code != -32600 {
			t.Errorf("request %s should be rejected with -32600 (invalid request), got %+v", req, resps)
		}
	}
}

// runMCPServer must echo the request id verbatim (numeric or string); a
// dropped/swapped id silently breaks multi-call MCP sessions.
func TestMCPServer_EchoesRequestID(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t,
		`{"jsonrpc":"2.0","id":42,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"abc","method":"ping"}`,
	)
	if len(resps) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(resps))
	}
	if string(resps[0].ID) != "42" {
		t.Errorf("numeric id should round-trip verbatim, got %s", resps[0].ID)
	}
	if string(resps[1].ID) != `"abc"` {
		t.Errorf("string id should round-trip verbatim, got %s", resps[1].ID)
	}
}

// A single line larger than maxMCPMessageBytes is rejected without consuming
// unbounded memory, and the server stops cleanly.
func TestMCPServer_OversizedMessageRejected(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", maxMCPMessageBytes+1024)
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_help","arguments":{"command":"` + big + `"}}}` + "\n"
	var out bytes.Buffer
	if err := runMCPServer(context.Background(), NewRootCmd(), strings.NewReader(line), &out); err != nil {
		t.Fatalf("runMCPServer: %v", err)
	}
	var resp mcpTestResp
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("expected an oversize error response, got %q (err %v)", out.String(), err)
	}
	if resp.Error == nil || resp.Error.Code != -32600 {
		t.Errorf("expected request-too-large (-32600), got %+v", resp.Error)
	}
}

// A JSON-RPC batch array is unsupported: it yields a parse error, and the server
// recovers to the next line instead of terminating.
func TestMCPServer_BatchArrayRejectedThenRecovers(t *testing.T) {
	t.Parallel()
	resps := driveMCP(t,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	)
	if len(resps) != 2 {
		t.Fatalf("expected 2 responses (parse error, then the recovered ping), got %d: %+v", len(resps), resps)
	}
	if resps[0].Error == nil || resps[0].Error.Code != -32700 {
		t.Errorf("batch array should yield a parse error (-32700), got %+v", resps[0].Error)
	}
	if resps[1].Error != nil {
		t.Errorf("server should recover and serve the next message, got error %+v", resps[1].Error)
	}
}

// Malformed JSON in the stream is reported as a JSON-RPC parse error (-32700).
func TestMCPServer_ParseError(t *testing.T) {
	t.Parallel()
	root := NewRootCmd()
	var out bytes.Buffer
	if err := runMCPServer(context.Background(), root, strings.NewReader("{not valid json\n"), &out); err != nil {
		t.Fatalf("runMCPServer: %v", err)
	}
	var resp mcpTestResp
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("expected a JSON parse-error response, got %q (err %v)", out.String(), err)
	}
	if resp.Error == nil || resp.Error.Code != -32700 {
		t.Errorf("expected parse error -32700, got %+v", resp.Error)
	}
}
