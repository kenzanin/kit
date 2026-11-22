package extensions

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/charmbracelet/log"
	lua "github.com/yuin/gopher-lua"
)

// luaHost runs a .lua extension in its own gopher-lua state (cgo-free,
// full stdlib — trust level matches yaegi's unrestricted set). The script
// must define the global function Init(api) and registers imperatively:
//
//	Init(function(api)
//	    api.on("agent_start", function(ev) ... end)
//	    api.register_tool{ name="x", description=..., parameters='{"type":"object"}',
//	                       execute=function(input) return "out" end }
//	    api.register_command{ name="x", description=..., execute=function(args) ... end }
//	    api.register_option{ name=..., description=..., default=... }
//	    api.register_shortcut({key="ctrl+p",description=...}, function() ... end)
//	    api.register_tool_renderer{ tool_name="x", render_header=fn, render_body=fn }
//	    api.register_message_renderer{ name="ext:render", render=fn }
//	end)
//
// Event payloads arrive as Lua tables (JSON-decoded; raw text stays a
// string); result events return a table or nil. Host env lives in kit:
// kit.log(...), kit.print/print_info/print_error/print_block(...), kit.ctx().
type luaHost struct {
	b   *nativeBridge
	L   *lua.LState
	reg *lua.LTable // registry table, for callback refs
	api *API
	ref int64 // next registry id; refs live for the extension's lifetime
}

// compileLua loads the script and returns its Init as a func(API) plus a
// close hook, without invoking it — the exact shape of the yaegi phase.
func compileLua(path string) (initFn func(API), closeFn func(), err error) {
	L := lua.NewState()
	defer func() {
		if err != nil {
			L.Close()
		}
	}()

	b := &nativeBridge{}
	h := &luaHost{b: b, L: L, reg: L.Get(lua.RegistryIndex).(*lua.LTable)}
	b.rt = h // bridge and host reference each other

	kit := L.NewTable()
	envPrint := func(f func(*Context, string)) lua.LValue {
		return L.NewFunction(func(L *lua.LState) int {
			if c := h.inFlight(); c != nil {
				f(c, L.CheckString(1))
			}
			return 0
		})
	}
	kitPrint := func(name string, f func(*Context, string)) { L.SetField(kit, name, envPrint(f)) }
	kitPrint("print", func(c *Context, s string) { c.Print(s) })
	kitPrint("print_info", func(c *Context, s string) { c.PrintInfo(s) })
	kitPrint("print_error", func(c *Context, s string) { c.PrintError(s) })
	kitPrint("print_block", func(c *Context, s string) {
		c.PrintBlock(PrintBlockOpts{Text: s})
	})
	L.SetField(kit, "log", L.NewFunction(func(L *lua.LState) int {
		// ponytail: debug-level only; route into the UI print surface when a
		// native log bridge exists.
		log.Debug(L.CheckString(1), "ext", filepath.Base(path))
		return 0
	}))
	L.SetField(kit, "ctx", L.NewFunction(func(L *lua.LState) int {
		L.Push(luaJSONDecode(L, h.b.env()))
		return 1
	}))
	L.SetGlobal("kit", kit)

	if err := L.DoFile(path); err != nil {
		return nil, nil, fmt.Errorf("running %s: %w", filepath.Base(path), err)
	}
	initVal := L.GetGlobal("Init")
	if _, ok := initVal.(*lua.LFunction); !ok {
		return nil, nil, fmt.Errorf("%s: no Init(api) function", filepath.Base(path))
	}

	initFn = func(api API) {
		h.api = &api
		if err := L.CallByParam(lua.P{Fn: initVal, NRet: 0, Protect: true}, h.adapter()); err != nil {
			log.Warn("native extension init failed", "ext", filepath.Base(path), "err", err)
		}
	}
	// ponytail: no explicit close hook exists on LoadedExtension yet. The
	// gopher-lua state is pure Go — dropping the extension lets GC reclaim
	// it; an explicit close belongs to the runner's future ext teardown.
	return initFn, func() { h.close() }, nil
}

func (h *luaHost) inFlight() *Context {
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	return h.b.ctx
}

// adapter builds the table handed to Init: one closure per extension-API
// operation, backed by the shared nativeBridge.
func (h *luaHost) adapter() *lua.LTable {
	L := h.L
	api := L.NewTable()
	reg := func(name string, fn func(L *lua.LState) int) {
		L.SetField(api, name, L.NewFunction(func(L *lua.LState) int { return fn(L) }))
	}
	reg("on", h.apiOn)
	reg("register_tool", h.apiRegisterTool)
	reg("register_command", h.apiRegisterCommand)
	reg("register_option", h.apiRegisterOption)
	reg("register_shortcut", h.apiRegisterShortcut)
	reg("register_tool_renderer", h.apiRegisterToolRenderer)
	reg("register_message_renderer", h.apiRegisterMessageRenderer)
	return api
}

// regRef stores a callable in the registry; ids are never reused and live
// as long as the extension.
func (h *luaHost) regRef(v lua.LValue) int64 {
	h.ref++
	h.reg.RawSetInt(int(h.ref), v)
	return h.ref
}

func (h *luaHost) refFn(L *lua.LState, idx int) int64 {
	return h.regRef(L.CheckAny(idx))
}

func (h *luaHost) apiOn(L *lua.LState) int {
	event, id := L.CheckString(1), h.refFn(L, 2)
	if err := h.b.on(h.api, event, id); err != nil {
		L.RaiseError("%s", err)
	}
	return 0
}

// metaFromTable pulls scalar fields into JSON and function fields into
// registry ids, in one pass.
func (h *luaHost) metaFromTable(L *lua.LState, fnKeys ...string) (string, map[string]int64) {
	tbl := L.CheckTable(1)
	meta, ids := map[string]any{}, map[string]int64{}
	for _, k := range fnKeys {
		v := L.GetField(tbl, k)
		if fn, ok := v.(*lua.LFunction); ok {
			ids[k] = h.regRef(fn)
			continue
		}
		if v != lua.LNil {
			meta[k] = luaToGo(v)
		}
	}
	return string(mustJSON(meta)), ids
}

func (h *luaHost) apiRegisterTool(L *lua.LState) int {
	meta, ids := h.metaFromTable(L, "name", "description", "parameters", "execute")
	if err := h.b.registerTool(h.api, meta, ids["execute"]); err != nil {
		L.RaiseError("%s", err)
	}
	return 0
}

func (h *luaHost) apiRegisterCommand(L *lua.LState) int {
	meta, ids := h.metaFromTable(L, "name", "description", "execute")
	if err := h.b.registerCommand(h.api, meta, ids["execute"]); err != nil {
		L.RaiseError("%s", err)
	}
	return 0
}

func (h *luaHost) apiRegisterOption(L *lua.LState) int {
	meta, _ := h.metaFromTable(L, "name", "description", "default")
	if err := h.b.registerOption(h.api, meta); err != nil {
		L.RaiseError("%s", err)
	}
	return 0
}

func (h *luaHost) apiRegisterShortcut(L *lua.LState) int {
	meta, ids := h.metaFromTable(L, "key", "description", "handler")
	if err := h.b.registerShortcut(h.api, meta, ids["handler"]); err != nil {
		L.RaiseError("%s", err)
	}
	return 0
}

func (h *luaHost) apiRegisterToolRenderer(L *lua.LState) int {
	meta, ids := h.metaFromTable(L, "tool_name", "render_header", "render_body")
	if err := h.b.registerToolRenderer(h.api, meta, ids); err != nil {
		L.RaiseError("%s", err)
	}
	return 0
}

func (h *luaHost) apiRegisterMessageRenderer(L *lua.LState) int {
	meta, ids := h.metaFromTable(L, "name", "render")
	if err := h.b.registerMessageRenderer(h.api, meta, ids); err != nil {
		L.RaiseError("%s", err)
	}
	return 0
}

func (h *luaHost) close() { h.L.Close() }

// invoke implements nativeHost: call registry fn id with the payload.
func (h *luaHost) invoke(id int64, payload string) (string, error) {
	fn := h.reg.RawGetInt(int(id))
	if _, ok := fn.(*lua.LFunction); !ok {
		return "", fmt.Errorf("callback %d is not a function", id)
	}
	arg := lua.LString(payload)
	asLua := lua.LValue(arg)
	if decoded := luaJSONDecode(h.L, payload); decoded != lua.LNil {
		asLua = decoded
	}
	if err := h.L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}, asLua); err != nil {
		return "", err
	}
	ret := h.L.Get(-1)
	h.L.Pop(1)
	return luaJSONEncode(ret), nil
}

// -- JSON <-> Lua value bridge ------------------------------------------------

// luaJSONDecode decodes JSON into Lua values; non-JSON strings pass through
// raw (commands deal in plain text).
func luaJSONDecode(L *lua.LState, s string) lua.LValue {
	if s == "" {
		return lua.LNil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return lua.LString(s)
	}
	return anyToLua(L, v)
}

func anyToLua(L *lua.LState, v any) lua.LValue {
	switch x := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(x)
	case float64:
		if i := int64(x); float64(i) == x {
			return lua.LNumber(i)
		}
		return lua.LNumber(x)
	case string:
		return lua.LString(x)
	case []any:
		t := L.NewTable()
		for i, e := range x {
			L.SetTable(t, lua.LNumber(i+1), anyToLua(L, e))
		}
		return t
	case map[string]any:
		t := L.NewTable()
		for k, e := range x {
			L.SetField(t, k, anyToLua(L, e))
		}
		return t
	default:
		b, _ := json.Marshal(v)
		return lua.LString(string(b))
	}
}

// luaJSONEncode encodes a returned Lua value as JSON; nil becomes "".
func luaJSONEncode(v lua.LValue) string {
	switch x := v.(type) {
	case nil, *lua.LNilType:
		return ""
	case lua.LString:
		return string(x)
	case lua.LNumber:
		return x.String()
	default:
		b, err := json.Marshal(luaToGo(x))
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func luaToGo(v lua.LValue) any {
	switch x := v.(type) {
	case *lua.LTable:
		if n := x.Len(); n > 0 {
			arr := make([]any, 0, n)
			dense := true
			for i := 1; i <= n; i++ {
				if x.RawGetInt(i) == lua.LNil {
					dense = false
					break
				}
				arr = append(arr, luaToGo(x.RawGetInt(i)))
			}
			if dense {
				return arr
			}
		}
		m := map[string]any{}
		x.ForEach(func(k, val lua.LValue) {
			if s, ok := k.(lua.LString); ok {
				m[string(s)] = luaToGo(val)
			}
		})
		return m
	case lua.LString:
		return string(x)
	case lua.LNumber:
		return float64(x)
	case lua.LBool:
		return bool(x)
	default:
		return nil
	}
}
