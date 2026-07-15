package plugins

import lua "github.com/yuin/gopher-lua"

// toLuaValue converts a Go value produced by the host into a Lua value inside
// L. Only the JSON-ish shapes the hook payloads use are supported; anything
// else becomes nil so a malformed payload can never inject an unexpected
// userdata or function into plugin scope.
//
// Numeric types are all mapped to lua.LNumber (Lua has a single number type).
func toLuaValue(ls *lua.LState, v any) lua.LValue {
	switch val := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(val)
	case string:
		return lua.LString(val)
	case int:
		return lua.LNumber(val)
	case int64:
		return lua.LNumber(val)
	case float64:
		return lua.LNumber(val)
	case map[string]any:
		return toLuaTable(ls, val)
	case []string:
		arr := ls.NewTable()
		for _, s := range val {
			arr.Append(lua.LString(s))
		}
		return arr
	case []any:
		arr := ls.NewTable()
		for _, item := range val {
			arr.Append(toLuaValue(ls, item))
		}
		return arr
	default:
		return lua.LNil
	}
}

// toLuaTable converts a string-keyed Go map into a Lua table inside ls.
func toLuaTable(ls *lua.LState, m map[string]any) *lua.LTable {
	tbl := ls.NewTable()
	for k, v := range m {
		ls.SetField(tbl, k, toLuaValue(ls, v))
	}
	return tbl
}
