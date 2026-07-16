package plugins

import (
	"fmt"

	lua "github.com/yuin/gopher-lua"
)

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

// fromLuaMaxDepth bounds recursion when converting a Lua value back to Go (the
// inverse of toLuaValue). Lua tables can reference themselves, which would
// otherwise recurse until the Go stack overflows; a self-referential or
// pathologically nested table trips this limit and yields an error instead. It
// is far deeper than any real JSON payload a plugin would hand to entire.json.
const fromLuaMaxDepth = 200

// fromLuaValue converts a Lua value into the Go value that encoding/json can
// marshal. It is the inverse of toLuaValue: nil→nil, booleans, numbers→float64
// (Lua's only number type), strings, and tables→[]any (a 1..n sequence) or
// map[string]any (otherwise). Functions, userdata, threads and channels have no
// JSON form and produce an error. depth guards against cycles/runaway nesting.
func fromLuaValue(v lua.LValue, depth int) (any, error) {
	if depth > fromLuaMaxDepth {
		return nil, fmt.Errorf("value nested deeper than %d levels (cyclic table?)", fromLuaMaxDepth)
	}
	switch val := v.(type) {
	case *lua.LNilType:
		// JSON null: a Go nil interface value with no error is the intended
		// result (json.Marshal renders it as null).
		return nil, nil //nolint:nilnil // nil is the valid encoding of JSON null
	case lua.LBool:
		return bool(val), nil
	case lua.LNumber:
		return float64(val), nil
	case lua.LString:
		return string(val), nil
	case *lua.LTable:
		return fromLuaTable(val, depth)
	default:
		return nil, fmt.Errorf("cannot encode Lua %s", v.Type().String())
	}
}

// fromLuaTable converts a Lua table into a Go slice or map. A table that is a
// proper 1..n integer-keyed sequence becomes a []any (JSON array); anything else
// (including the empty table) becomes a map[string]any (JSON object), with
// integer keys coerced to their decimal string form. Non-string, non-number keys
// (booleans, nested tables) have no JSON object-key form and produce an error.
func fromLuaTable(t *lua.LTable, depth int) (any, error) {
	if n := t.Len(); n > 0 && isLuaSequence(t, n) {
		arr := make([]any, 0, n)
		for i := 1; i <= n; i++ {
			gv, err := fromLuaValue(t.RawGetInt(i), depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, gv)
		}
		return arr, nil
	}

	obj := make(map[string]any)
	var ferr error
	t.ForEach(func(k, v lua.LValue) {
		if ferr != nil {
			return
		}
		var key string
		switch kk := k.(type) {
		case lua.LString:
			key = string(kk)
		case lua.LNumber:
			key = kk.String()
		default:
			ferr = fmt.Errorf("cannot encode table with a %s key", k.Type().String())
			return
		}
		gv, err := fromLuaValue(v, depth+1)
		if err != nil {
			ferr = err
			return
		}
		obj[key] = gv
	})
	if ferr != nil {
		return nil, ferr
	}
	return obj, nil
}

// isLuaSequence reports whether t is a dense array: every key is an integer in
// [1, n] and there are exactly n of them (no gaps, no extra non-integer keys),
// where n is the table's border length. Such a table round-trips to a JSON
// array; any other shape is treated as an object.
func isLuaSequence(t *lua.LTable, n int) bool {
	count := 0
	dense := true
	t.ForEach(func(k, _ lua.LValue) {
		num, ok := k.(lua.LNumber)
		if !ok {
			dense = false
			return
		}
		i := int(num)
		if lua.LNumber(i) != num || i < 1 || i > n {
			dense = false
			return
		}
		count++
	})
	return dense && count == n
}
