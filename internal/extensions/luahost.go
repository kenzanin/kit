package extensions

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/charmbracelet/log"
	"modernc.org/lua"
)

// luaHost runs a .lua extension in its own Lua 5.5 VM (pure Go, cgo-free,
// full stdlib — trust level matches yaegi's unrestricted set). The script
// must define the global function Init(api) and registers imperatively:
//
//	Init(function(api)
//	    api.on("agent_start", function(ev) ... end)
//	    api.register_tool{ name="x", description=..., parameters='{"type":"object"}',
//	                       execute=function(input) return "out" end }
//	    api.register_command{ name="x", description=..., execute=function(args) ... end }
//	    api.register_option{ name="x", description=..., default=... }
//	    api.register_shortcut({key="ctrl+p",description=...}, function() ... end)
//	    api.register_tool_renderer{ tool_name="x", render_header=fn, render_body=fn }
//	    api.register_message_renderer{ name="ext:render", render=fn }
//	end)
//
// Event payloads arrive as Lua tables (JSON-decoded; raw text stays a
// string); result events return a table or nil. Host env lives in kit:
// kit.log(...), kit.print/print_info/print_error/print_block(...), kit.ctx(),
// kit.json(v).
//
// Lua 5.5 numbers are integers or floats: int64 ids, bitwise operators and
// math.type/math.maxinteger all behave as in PUC Lua.
type luaHost struct {
	b    *nativeBridge
	vm   *lua.VM
	refs map[int64]*lua.Value // callbacks, live for the extension's lifetime
	next int64
	keep []*lua.Value // refs owned by the host: encoder, Init, api table
	enc  *lua.Value   // Lua JSON encoder, used for table results
	api  *API
}

// compileLua loads the script and returns its Init as a func(API) plus a
// close hook, without invoking it — the exact shape of the yaegi phase.
func compileLua(path string) (initFn func(API), closeFn func(), err error) {
	vm, err := lua.NewVM()
	if err != nil {
		return nil, nil, fmt.Errorf("lua state: %w", err)
	}
	defer func() {
		if err != nil {
			vm.Close()
		}
	}()

	b := &nativeBridge{}
	h := &luaHost{b: b, vm: vm, refs: map[int64]*lua.Value{}}
	b.rt = h // bridge and host reference each other

	kit, err := vm.NewTable()
	if err != nil {
		return nil, nil, err
	}
	var setErr error
	set := func(k string, v any) {
		if setErr == nil {
			setErr = kit.Set(k, v)
		}
	}
	printers := map[string]func(*Context, string){
		"print":       func(c *Context, s string) { c.Print(s) },
		"print_info":  func(c *Context, s string) { c.PrintInfo(s) },
		"print_error": func(c *Context, s string) { c.PrintError(s) },
		"print_block": func(c *Context, s string) { c.PrintBlock(PrintBlockOpts{Text: s}) },
	}
	for name, f := range printers {
		set(name, func(s string) {
			if c := h.inFlight(); c != nil {
				f(c, s)
			}
		})
	}
	set("log", func(s string) {
		// ponytail: debug-level only; route into the UI print surface when a
		// native log bridge exists.
		log.Debug(s, "ext", filepath.Base(path))
	})
	set("ctx", func() any {
		v, _ := decodeJSON(h.b.env())
		return v
	})
	set("json_decode", func(s string) any {
		if v, ok := decodeJSON(s); ok {
			return v
		}
		return s
	})
	if setErr != nil {
		return nil, nil, setErr
	}
	if err = vm.SetGlobal("kit", kit); err != nil {
		return nil, nil, err
	}

	// The JSON encoder has to walk Lua tables, which the Go API cannot
	// enumerate; keep it in Lua and call it from Go for table results.
	res, err := vm.Eval(luaJSONCodec)
	if err != nil {
		return nil, nil, fmt.Errorf("lua json prelude: %w", err)
	}
	if len(res) != 1 {
		return nil, nil, fmt.Errorf("lua json prelude: no encoder")
	}
	enc, ok := res[0].(*lua.Value)
	if !ok {
		return nil, nil, fmt.Errorf("lua json prelude: encoder is %T", res[0])
	}
	h.enc = enc
	h.keep = append(h.keep, enc)

	apiTbl, err := vm.NewTable()
	if err != nil {
		return nil, nil, err
	}
	var apiErr error
	apiSet := func(k string, f any) {
		if apiErr == nil {
			apiErr = apiTbl.Set(k, f)
		}
	}
	apiSet("on", func(event string, fn *lua.Value) error {
		id, err := h.regRef(fn)
		if err != nil {
			return err
		}
		return h.b.on(h.api, event, id)
	})
	apiSet("register_tool", func(meta *lua.Value) error {
		m, ids, err := h.metaFromTable(meta, "name", "description", "parameters", "execute")
		if err != nil {
			return err
		}
		return h.b.registerTool(h.api, m, ids["execute"])
	})
	apiSet("register_command", func(meta *lua.Value) error {
		m, ids, err := h.metaFromTable(meta, "name", "description", "execute")
		if err != nil {
			return err
		}
		return h.b.registerCommand(h.api, m, ids["execute"])
	})
	apiSet("register_option", func(meta *lua.Value) error {
		m, _, err := h.metaFromTable(meta, "name", "description", "default")
		if err != nil {
			return err
		}
		return h.b.registerOption(h.api, m)
	})
	apiSet("register_shortcut", func(meta *lua.Value, handler *lua.Value) error {
		m, ids, err := h.metaFromTable(meta, "key", "description", "handler")
		if err != nil {
			return err
		}
		id := ids["handler"]
		if id == 0 && handler != nil { // documented form: (meta, fn)
			if id, err = h.regRef(handler); err != nil {
				return err
			}
		}
		return h.b.registerShortcut(h.api, m, id)
	})
	apiSet("register_tool_renderer", func(meta *lua.Value) error {
		m, ids, err := h.metaFromTable(meta, "tool_name", "render_header", "render_body")
		if err != nil {
			return err
		}
		return h.b.registerToolRenderer(h.api, m, ids)
	})
	apiSet("register_message_renderer", func(meta *lua.Value) error {
		m, ids, err := h.metaFromTable(meta, "name", "render")
		if err != nil {
			return err
		}
		return h.b.registerMessageRenderer(h.api, m, ids)
	})
	if apiErr != nil {
		return nil, nil, apiErr
	}
	if err = vm.SetGlobal("api", apiTbl); err != nil {
		return nil, nil, err
	}
	h.keep = append(h.keep, apiTbl)

	if _, err = vm.EvalFile(path); err != nil {
		return nil, nil, fmt.Errorf("running %s: %w", filepath.Base(path), err)
	}
	initVal, err := vm.Global("Init")
	if err != nil {
		return nil, nil, err
	}
	initLua, ok := initVal.(*lua.Value)
	if !ok || initLua.Type() != "function" {
		return nil, nil, fmt.Errorf("%s: no Init(api) function", filepath.Base(path))
	}
	h.keep = append(h.keep, initLua)

	// The loader drops closeFn today, and a Lua VM holds malloc'd memory
	// the GC cannot reclaim, so close on collection as a backstop.
	runtime.SetFinalizer(h, (*luaHost).close)

	initFn = func(api API) {
		h.api = &api
		if _, err := initLua.Call(apiTbl); err != nil {
			log.Warn("native extension init failed", "ext", filepath.Base(path), "err", err)
		}
	}
	return initFn, func() { h.close() }, nil
}

func (h *luaHost) inFlight() *Context {
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	return h.b.ctx
}

// regRef stores a callback; ids are never reused and live as long as the
// extension. The argument is owned and freed by the caller (the Go API frees
// *Value arguments after the call), so the reference is duplicated.
func (h *luaHost) regRef(v *lua.Value) (int64, error) {
	if v == nil || v.Type() != "function" {
		return 0, fmt.Errorf("callback is not a function")
	}
	d, err := v.Dup()
	if err != nil {
		return 0, err
	}
	h.next++
	h.refs[h.next] = d
	return h.next, nil
}

// metaFromTable pulls scalar fields into JSON and function fields into
// registry ids, in one pass. Nested tables are encoded as JSON strings.
func (h *luaHost) metaFromTable(tbl *lua.Value, fnKeys ...string) (string, map[string]int64, error) {
	if tbl == nil || tbl.Type() != "table" {
		return "", nil, fmt.Errorf("expected a table of fields")
	}
	meta, ids := map[string]any{}, map[string]int64{}
	for _, k := range fnKeys {
		v, err := tbl.Get(k)
		if err != nil {
			return "", nil, err
		}
		if v == nil {
			continue
		}
		if lv, ok := v.(*lua.Value); ok {
			if lv.Type() == "function" {
				id, err := h.regRef(lv)
				if err != nil {
					return "", nil, err
				}
				ids[k] = id
				continue
			}
			s, err := h.encodeAny(lv)
			lv.Free()
			if err != nil {
				return "", nil, err
			}
			meta[k] = s
			continue
		}
		meta[k] = v
	}
	return string(mustJSON(meta)), ids, nil
}

func (h *luaHost) close() {
	for _, v := range h.refs {
		v.Free()
	}
	h.refs = nil
	for _, v := range h.keep {
		v.Free()
	}
	h.keep = nil
	h.enc = nil
	h.vm.Close()
}

// invoke implements nativeHost: call registry fn id with the payload.
func (h *luaHost) invoke(id int64, payload string) (string, error) {
	fn := h.refs[id]
	if fn == nil || fn.Type() != "function" {
		return "", fmt.Errorf("callback %d is not a function", id)
	}
	arg := any(payload)
	if v, ok := decodeJSON(payload); ok && v != nil {
		arg = v
	}
	res, err := fn.Call(arg)
	if err != nil {
		return "", err
	}
	if len(res) == 0 {
		return "", nil
	}
	defer freeLuaValues(res)
	return h.encodeResult(res[0])
}

// encodeResult mirrors the bridge contract: a string result is the raw
// answer (commands deal in plain text), nil is "", tables become JSON.
func (h *luaHost) encodeResult(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", nil
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case *lua.Value:
		return h.encodeAny(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

// encodeAny runs the Lua JSON encoder on a value.
func (h *luaHost) encodeAny(v *lua.Value) (string, error) {
	res, err := h.enc.Call(v)
	if err != nil {
		return "", err
	}
	defer freeLuaValues(res)
	if len(res) == 0 {
		return "null", nil
	}
	s, ok := res[0].(string)
	if !ok {
		return "", fmt.Errorf("json encoder returned %T", res[0])
	}
	return s, nil
}

func freeLuaValues(vals []any) {
	for _, v := range vals {
		if lv, ok := v.(*lua.Value); ok {
			lv.Free()
		}
	}
}

// decodeJSON decodes JSON preserving integers, so 64-bit ids survive the
// trip into Lua. It reports false for text that is not JSON.
func decodeJSON(s string) (any, bool) {
	if s == "" {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return normJSON(v), true
}

func normJSON(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case []any:
		for i, e := range x {
			x[i] = normJSON(e)
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = normJSON(e)
		}
		return x
	}
	return v
}

// luaJSONCodec installs kit.json (encode anything, decode strings) and
// returns the encoder. It lives in Lua because only Lua can enumerate table
// keys.
const luaJSONCodec = `
local function esc(s)
  return '"' .. s:gsub('[%z\1-\31\\"]', function(c)
    if c == '"' then return '\\"' end
    if c == '\\' then return '\\\\' end
    if c == '\n' then return '\\n' end
    if c == '\r' then return '\\r' end
    if c == '\t' then return '\\t' end
    return string.format('\\u%04x', string.byte(c))
  end) .. '"'
end

local function enc(v)
  local t = type(v)
  if t == 'nil' then return 'null' end
  if t == 'boolean' then return tostring(v) end
  if t == 'number' then
    if math.type(v) == 'integer' then return tostring(v) end
    if v ~= v or v == math.huge or v == -math.huge then return 'null' end
    return string.format('%.17g', v)
  end
  if t == 'string' then return esc(v) end
  if t == 'table' then
    local n = #v
    local isarr = n > 0
    for k in pairs(v) do
      if math.type(k) ~= 'integer' or k < 1 or k > n then
        isarr = false
        break
      end
    end
    local out = {}
    if isarr then
      for i = 1, n do out[i] = enc(v[i]) end
      return '[' .. table.concat(out, ',') .. ']'
    end
    for k, val in pairs(v) do
      out[#out + 1] = esc(tostring(k)) .. ':' .. enc(val)
    end
    return '{' .. table.concat(out, ',') .. '}'
  end
  return 'null'
end

kit.json = function(v)
  if type(v) == 'string' then return kit.json_decode(v) end
  return enc(v)
end

return enc
`
