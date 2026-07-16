package plugins

import (
	"encoding/json"

	lua "github.com/yuin/gopher-lua"
)

// entire.json is a pure, deterministic JSON codec for plugin authors. Unlike
// http/exec/fs it reaches nothing outside the Lua state, so it is always
// available — never capability-gated. It is registered on the entire module in
// installAPI alongside entire.log / entire.kv / entire.print.
//
// The conversion in both directions reuses the shared value marshalers in
// marshal.go (toLuaValue / fromLuaValue), so decode and encode agree on how the
// JSON-ish shapes map to Lua: objects↔tables, arrays↔sequences, numbers↔Lua
// numbers, and true/false/null↔boolean/nil.

// luaJSONDecode implements entire.json.decode(str) -> value. It parses str as
// JSON and returns the equivalent Lua value: a JSON object becomes a table
// keyed by its member names, an array becomes a 1..n sequence, numbers (plain,
// fractional, or scientific-notation such as 3e-06) become Lua numbers, and
// true/false/null become boolean/boolean/nil. Invalid JSON raises a Lua error.
func luaJSONDecode(ls *lua.LState) int {
	s := ls.CheckString(1)
	// Unmarshal into interface{}: encoding/json yields map[string]any, []any,
	// float64, string, bool and nil — exactly the shapes toLuaValue converts.
	// Numbers arrive as float64, which is Lua's only number type, so fractional
	// and exponent values are preserved (3e-06 -> 0.000003) for free.
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		ls.RaiseError("entire.json.decode: %v", err)
		return 0
	}
	ls.Push(toLuaValue(ls, v))
	return 1
}

// luaJSONEncode implements entire.json.encode(value) -> string. It converts the
// Lua value to its Go equivalent (via fromLuaValue) and marshals it to a JSON
// string. A table is encoded as an array when it is a dense 1..n sequence and as
// an object otherwise (an empty table encodes as {}). Values with no JSON form
// (functions, userdata) and cyclic/over-deep tables raise a Lua error.
func luaJSONEncode(ls *lua.LState) int {
	v := ls.CheckAny(1)
	gov, err := fromLuaValue(v, 0)
	if err != nil {
		ls.RaiseError("entire.json.encode: %v", err)
		return 0
	}
	out, err := json.Marshal(gov)
	if err != nil {
		ls.RaiseError("entire.json.encode: %v", err)
		return 0
	}
	ls.Push(lua.LString(out))
	return 1
}
