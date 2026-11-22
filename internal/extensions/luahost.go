package extensions

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/charmbracelet/log"
	lua "github.com/yuin/gopher-lua"
)

// luaHost runs a .lua extension in its own gopher-lua state. The script must
// define the global function kit_handle(event, payload); it may call kit.log.
type luaHost struct {
	mu sync.Mutex
	L  *lua.LState
	fn lua.LValue
}

// newLuaHost loads and runs the script, then resolves kit_handle.
func newLuaHost(path string) (nativeHost, error) {
	L := lua.NewState()

	kit := L.NewTable()
	L.SetField(kit, "log", L.NewFunction(func(L *lua.LState) int {
		// ponytail: debug-level only; route into the UI print surface when a
		// native Context bridge exists.
		log.Debug(L.CheckString(1), "ext", filepath.Base(path))
		return 0
	}))
	L.SetGlobal("kit", kit)

	if err := L.DoFile(path); err != nil {
		L.Close()
		return nil, fmt.Errorf("running %s: %w", filepath.Base(path), err)
	}

	fn := L.GetGlobal("kit_handle")
	if fn == lua.LNil {
		L.Close()
		return nil, fmt.Errorf("%s: no kit_handle(event, payload) function", filepath.Base(path))
	}
	return &luaHost{L: L, fn: fn}, nil
}

func (h *luaHost) call(event, payload string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.L.CallByParam(lua.P{Fn: h.fn, NRet: 1}, lua.LString(event), lua.LString(payload)); err != nil {
		return "", err
	}
	ret := h.L.Get(-1)
	h.L.Pop(1)
	if s, ok := ret.(lua.LString); ok {
		return string(s), nil
	}
	return "", nil
}

func (h *luaHost) close() { h.L.Close() }
