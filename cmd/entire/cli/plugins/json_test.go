package plugins

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	lua "github.com/yuin/gopher-lua"
)

// loadJSONProbe loads a plugin whose entry script exercises entire.json at load
// time and stores results in _G globals for inspection. It grants NO
// capabilities, so a successful load also proves entire.json is always available
// (never capability-gated). A raised Lua error during load fails the test.
func loadJSONProbe(t *testing.T, script string) *LoadedPlugin {
	t.Helper()
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())
	dir := writePluginDir(t, `{"name":"jsonprobe"}`, script)
	p, err := LoadPlugin(context.Background(), dir, SourceUser, "", settings.PluginSettings{Enabled: true})
	if err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// TestJSON_DecodeEncodeRoundTrip drives entire.json.decode / encode across the
// JSON-ish shapes plugins actually see: objects, arrays, nesting, numbers
// (including scientific notation), booleans, null and strings, plus an
// encode→decode round trip. entire.json is granted no capability here.
func TestJSON_DecodeEncodeRoundTrip(t *testing.T) {
	const probe = `
-- decode: object with number / string / bool / null members
local obj = entire.json.decode('{"a": 1, "b": "two", "c": true, "d": null}')
_G.obj_a = obj.a
_G.obj_b = obj.b
_G.obj_c = obj.c
_G.obj_d_is_nil = (obj.d == nil)

-- decode: array -> 1-based sequence
local arr = entire.json.decode('[10, 20, 30]')
_G.arr_len = #arr
_G.arr_1 = arr[1]
_G.arr_3 = arr[3]

-- decode: nested object + scientific-notation number + nested array
local nested = entire.json.decode('{"model": {"input_cost_per_token": 3e-06, "tags": ["x", "y"]}}')
_G.nested_input = nested.model.input_cost_per_token
_G.nested_tag2 = nested.model.tags[2]

-- decode: bare scalars
_G.scalar_num = entire.json.decode('42')
_G.scalar_true = entire.json.decode('true')
_G.scalar_str = entire.json.decode('"hi"')
_G.scalar_null_is_nil = (entire.json.decode('null') == nil)

-- encode: object, then decode it back (round trip)
local enc = entire.json.encode({ name = "gpt", rate = 3e-06, on = true })
_G.enc_str = enc
local back = entire.json.decode(enc)
_G.rt_name = back.name
_G.rt_rate = back.rate
_G.rt_on = back.on

-- encode: dense sequence -> JSON array
_G.enc_arr = entire.json.encode({ 1, 2, 3 })

-- encode: empty table -> JSON object
_G.enc_empty = entire.json.encode({})

-- encode: nested, round-tripped
local back2 = entire.json.decode(entire.json.encode({ outer = { inner = { 7, 8 } } }))
_G.rt_nested = back2.outer.inner[2]
`
	p := loadJSONProbe(t, probe)
	L := p.L

	// --- decode object ---
	if got := lua.LVAsNumber(L.GetGlobal("obj_a")); got != 1 {
		t.Errorf("obj.a = %v, want 1", got)
	}
	if got := L.GetGlobal("obj_b").String(); got != "two" {
		t.Errorf("obj.b = %q, want two", got)
	}
	if !lua.LVAsBool(L.GetGlobal("obj_c")) {
		t.Error("obj.c should decode to true")
	}
	if !lua.LVAsBool(L.GetGlobal("obj_d_is_nil")) {
		t.Error("obj.d should decode to nil (JSON null), got not-nil")
	}

	// --- decode array ---
	if got := lua.LVAsNumber(L.GetGlobal("arr_len")); got != 3 {
		t.Errorf("#arr = %v, want 3", got)
	}
	if got := lua.LVAsNumber(L.GetGlobal("arr_1")); got != 10 {
		t.Errorf("arr[1] = %v, want 10", got)
	}
	if got := lua.LVAsNumber(L.GetGlobal("arr_3")); got != 30 {
		t.Errorf("arr[3] = %v, want 30", got)
	}

	// --- decode nested + scientific notation ---
	if got := float64(lua.LVAsNumber(L.GetGlobal("nested_input"))); math.Abs(got-3e-06) > 1e-18 {
		t.Errorf("nested scientific-notation number = %v, want 3e-06", got)
	}
	if got := L.GetGlobal("nested_tag2").String(); got != "y" {
		t.Errorf("nested.model.tags[2] = %q, want y", got)
	}

	// --- decode scalars ---
	if got := lua.LVAsNumber(L.GetGlobal("scalar_num")); got != 42 {
		t.Errorf("decode('42') = %v, want 42", got)
	}
	if !lua.LVAsBool(L.GetGlobal("scalar_true")) {
		t.Error("decode('true') should be boolean true")
	}
	if got := L.GetGlobal("scalar_str").String(); got != "hi" {
		t.Errorf("decode('\"hi\"') = %q, want hi", got)
	}
	if !lua.LVAsBool(L.GetGlobal("scalar_null_is_nil")) {
		t.Error("decode('null') should be nil")
	}

	// --- encode round trip ---
	if got := L.GetGlobal("rt_name").String(); got != "gpt" {
		t.Errorf("round-tripped name = %q, want gpt", got)
	}
	if got := float64(lua.LVAsNumber(L.GetGlobal("rt_rate"))); math.Abs(got-3e-06) > 1e-18 {
		t.Errorf("round-tripped rate = %v, want 3e-06", got)
	}
	if !lua.LVAsBool(L.GetGlobal("rt_on")) {
		t.Error("round-tripped bool should be true")
	}
	if got := L.GetGlobal("enc_arr").String(); got != "[1,2,3]" {
		t.Errorf("encode({1,2,3}) = %q, want [1,2,3]", got)
	}
	if got := L.GetGlobal("enc_empty").String(); got != "{}" {
		t.Errorf("encode({}) = %q, want {} (empty table encodes as object)", got)
	}
	if got := lua.LVAsNumber(L.GetGlobal("rt_nested")); got != 8 {
		t.Errorf("round-tripped nested value = %v, want 8", got)
	}
	// The encoded object must itself be valid JSON with the expected members.
	if enc := L.GetGlobal("enc_str").String(); !strings.Contains(enc, `"name":"gpt"`) {
		t.Errorf("encoded object %q missing name member", enc)
	}
}

// TestJSON_DecodeInvalidRaises confirms malformed JSON raises a Lua error (not a
// silent nil), so a plugin fails loud on garbage input.
func TestJSON_DecodeInvalidRaises(t *testing.T) {
	const probe = `
_G.ok, _G.err = pcall(function() return entire.json.decode('{not valid') end)
`
	p := loadJSONProbe(t, probe)
	if lua.LVAsBool(p.L.GetGlobal("ok")) {
		t.Error("decode of invalid JSON should raise (pcall ok = true)")
	}
	if errMsg := p.L.GetGlobal("err").String(); !strings.Contains(errMsg, "entire.json.decode") {
		t.Errorf("expected decode error to name entire.json.decode, got %q", errMsg)
	}
}

// TestJSON_EncodeRejectsUnsupported confirms a value with no JSON form (a
// function) raises rather than producing bogus output.
func TestJSON_EncodeRejectsUnsupported(t *testing.T) {
	const probe = `
_G.ok, _G.err = pcall(function() return entire.json.encode(function() end) end)
`
	p := loadJSONProbe(t, probe)
	if lua.LVAsBool(p.L.GetGlobal("ok")) {
		t.Error("encode of a function should raise (pcall ok = true)")
	}
	if errMsg := p.L.GetGlobal("err").String(); !strings.Contains(errMsg, "entire.json.encode") {
		t.Errorf("expected encode error to name entire.json.encode, got %q", errMsg)
	}
}
