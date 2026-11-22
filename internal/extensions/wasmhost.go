package extensions

import (
	"context"
	"fmt"
	"os"

	"github.com/charmbracelet/log"
	"github.com/tetratelabs/wazero"
	api "github.com/tetratelabs/wazero/api"
)

// wasmHost runs a .wasm extension in a sandboxed wazero module (cgo-free;
// no file, network, or clock access beyond the kit imports). Contract:
//
//	exports: memory, kit_init, kit_alloc(i32) -> i32, kit_result_len() -> i32,
//	         kit_dispatch(id i64, ptr i32, len i32) -> i32
//	imports (module "kit"):
//	  on(name_ptr,name_len i32, id i64)
//	  register_tool(ptr,len i32, id i64)        meta JSON + callback id
//	  register_command(ptr,len i32, id i64)
//	  register_option(ptr,len i32)              data only
//	  register_shortcut(ptr,len i32, id i64)
//	  register_tool_renderer(ptr,len, header i64, body i64)
//	  register_message_renderer(ptr,len, render i64)
//	  log / print / print_info / print_error / print_block (ptr,len i32)
//	  ctx() -> (ptr i32, len i32)               in-flight Context, JSON
//
// Host->guest: ptr = kit_alloc(len); write payload; kit_dispatch(id,ptr,len)
// -> result ptr; result length via kit_result_len. Guest code must not
// re-enter host dispatch from a callback; imports during callbacks are fine.
type wasmHost struct {
	b    *nativeBridge
	rt   wazero.Runtime
	mod  api.Module
	env  *wasmEnv
	path string
	api  *API
}

type wasmEnv struct {
	memory    api.Memory
	alloc     api.Function
	resultLen api.Function
	dispatch  api.Function
}

// compileWasm instantiates the module; its registrations happen inside
// kit_init through the kit imports. Returned initFn runs kit_init against
// the recording API — the exact shape of the yaegi phase.
func compileWasm(path string) (initFn func(API), closeFn func(), err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) < 8 || string(raw[:4]) != "\x00asm" {
		return nil, nil, fmt.Errorf("%s: not a wasm binary", path)
	}

	h := &wasmHost{b: &nativeBridge{}, path: path}
	h.b.rt = h
	rt := wazero.NewRuntime(context.Background())
	defer func() {
		if err != nil {
			rt.Close(context.Background())
		}
	}()

	_, err = rt.NewHostModuleBuilder("kit").
		NewFunctionBuilder().WithFunc(h.impOn).Export("on").
		NewFunctionBuilder().WithFunc(h.impRegisterTool).Export("register_tool").
		NewFunctionBuilder().WithFunc(h.impRegisterCommand).Export("register_command").
		NewFunctionBuilder().WithFunc(h.impRegisterOption).Export("register_option").
		NewFunctionBuilder().WithFunc(h.impRegisterShortcut).Export("register_shortcut").
		NewFunctionBuilder().WithFunc(h.impRegisterToolRenderer).Export("register_tool_renderer").
		NewFunctionBuilder().WithFunc(h.impRegisterMessageRenderer).Export("register_message_renderer").
		NewFunctionBuilder().WithFunc(h.impLog).Export("log").
		NewFunctionBuilder().WithFunc(h.impPrint).Export("print").
		NewFunctionBuilder().WithFunc(h.impPrintInfo).Export("print_info").
		NewFunctionBuilder().WithFunc(h.impPrintError).Export("print_error").
		NewFunctionBuilder().WithFunc(h.impPrintBlock).Export("print_block").
		NewFunctionBuilder().WithFunc(h.impCtx).Export("ctx").
		Instantiate(context.Background())
	if err != nil {
		return nil, nil, fmt.Errorf("kit host module: %w", err)
	}

	mod, err := rt.InstantiateWithConfig(context.Background(), raw, wazero.NewModuleConfig().WithName(path))
	if err != nil {
		return nil, nil, fmt.Errorf("instantiating: %w", err)
	}
	h.mod = mod
	alloc := mod.ExportedFunction("kit_alloc")
	resultLen := mod.ExportedFunction("kit_result_len")
	dispatch := mod.ExportedFunction("kit_dispatch")
	init := mod.ExportedFunction("kit_init")
	for n, fn := range map[string]api.Function{
		"kit_alloc": alloc, "kit_result_len": resultLen, "kit_dispatch": dispatch, "kit_init": init,
	} {
		if fn == nil {
			return nil, nil, fmt.Errorf("missing %s export", n)
		}
	}
	if mod.Memory() == nil {
		return nil, nil, fmt.Errorf("missing memory export")
	}
	h.env = &wasmEnv{memory: mod.Memory(), alloc: alloc, resultLen: resultLen, dispatch: dispatch}

	initFn = func(a API) {
		h.api = &a
		if _, err := init.Call(context.Background()); err != nil {
			log.Warn("native extension init failed", "ext", h.path, "err", err)
		}
	}
	// ponytail: no explicit close hook on LoadedExtension yet; wazero
	// reclaims compiled state with GC. closeFn stays for a future teardown.
	return initFn, func() { h.close() }, nil
}

// -- guest registration imports ----------------------------------------------

func (h *wasmHost) withAPI(fn func(*API)) {
	if h.api != nil {
		fn(h.api)
	}
}

func (h *wasmHost) impOn(namePtr, nameLen uint32, id uint64) {
	name := h.readStr(namePtr, nameLen)
	h.withAPI(func(a *API) {
		if err := h.b.on(a, name, int64(id)); err != nil {
			log.Warn("native extension register failed", "event", name, "err", err)
		}
	})
}

func (h *wasmHost) impRegisterTool(ptr, len uint32, id uint64) {
	h.withAPI(func(a *API) {
		if err := h.b.registerTool(a, h.readStr(ptr, len), int64(id)); err != nil {
			log.Warn("native extension register failed", "op", "tool", "err", err)
		}
	})
}

func (h *wasmHost) impRegisterCommand(ptr, len uint32, id uint64) {
	h.withAPI(func(a *API) {
		if err := h.b.registerCommand(a, h.readStr(ptr, len), int64(id)); err != nil {
			log.Warn("native extension register failed", "op", "command", "err", err)
		}
	})
}

func (h *wasmHost) impRegisterOption(ptr, len uint32) {
	h.withAPI(func(a *API) {
		if err := h.b.registerOption(a, h.readStr(ptr, len)); err != nil {
			log.Warn("native extension register failed", "op", "option", "err", err)
		}
	})
}

func (h *wasmHost) impRegisterShortcut(ptr, len uint32, id uint64) {
	h.withAPI(func(a *API) {
		if err := h.b.registerShortcut(a, h.readStr(ptr, len), int64(id)); err != nil {
			log.Warn("native extension register failed", "op", "shortcut", "err", err)
		}
	})
}

func (h *wasmHost) impRegisterToolRenderer(ptr, len uint32, header, body uint64) {
	h.withAPI(func(a *API) {
		ids := map[string]int64{"render_header": int64(header), "render_body": int64(body)}
		if err := h.b.registerToolRenderer(a, h.readStr(ptr, len), ids); err != nil {
			log.Warn("native extension register failed", "op", "tool_renderer", "err", err)
		}
	})
}

func (h *wasmHost) impRegisterMessageRenderer(ptr, len uint32, render uint64) {
	h.withAPI(func(a *API) {
		if err := h.b.registerMessageRenderer(a, h.readStr(ptr, len), map[string]int64{"render": int64(render)}); err != nil {
			log.Warn("native extension register failed", "op", "message_renderer", "err", err)
		}
	})
}

func (h *wasmHost) impLog(ptr, len uint32) {
	// ponytail: debug-level only; route into the UI print surface when a
	// native log bridge exists.
	log.Debug(h.readStr(ptr, len), "ext", h.path)
}

func (h *wasmHost) printVia(f func(*Context, string), ptr, len uint32) {
	h.b.mu.Lock()
	ctx := h.b.ctx
	h.b.mu.Unlock()
	if ctx != nil {
		f(ctx, h.readStr(ptr, len))
	}
}

func (h *wasmHost) impPrint(ptr, len uint32) {
	h.printVia(func(c *Context, s string) { c.Print(s) }, ptr, len)
}
func (h *wasmHost) impPrintInfo(ptr, len uint32) {
	h.printVia(func(c *Context, s string) { c.PrintInfo(s) }, ptr, len)
}
func (h *wasmHost) impPrintError(ptr, len uint32) {
	h.printVia(func(c *Context, s string) { c.PrintError(s) }, ptr, len)
}
func (h *wasmHost) impPrintBlock(ptr, len uint32) {
	h.printVia(func(c *Context, s string) { c.PrintBlock(PrintBlockOpts{Text: s}) }, ptr, len)
}

// impCtx writes the in-flight Context JSON into guest memory.
func (h *wasmHost) impCtx() (uint32, uint32) {
	return h.writeGuest(h.b.env())
}

// -- host -> guest ------------------------------------------------------------

// close implements nativeHost.
func (h *wasmHost) close() {
	if h.mod != nil {
		_ = h.mod.Close(context.Background())
	}
	if h.rt != nil {
		_ = h.rt.Close(context.Background())
	}
}

// invoke implements nativeHost.
func (h *wasmHost) invoke(id int64, payload string) (string, error) {
	ptr, err := h.writeAndDispatch(uint64(id), payload)
	if err != nil {
		return "", err
	}
	if ptr == 0 {
		return "", nil
	}
	n, err := first(h.env.resultLen.Call(context.Background()))
	if err != nil {
		return "", fmt.Errorf("kit_result_len: %w", err)
	}
	return h.readStr(uint32(ptr), uint32(n)), nil
}

// writeAndDispatch: alloc in guest, copy payload, call the callback.
func (h *wasmHost) writeAndDispatch(id uint64, payload string) (uint64, error) {
	ptr, err := first(h.env.alloc.Call(context.Background(), uint64(len(payload))))
	if err != nil {
		return 0, fmt.Errorf("kit_alloc: %w", err)
	}
	if len(payload) > 0 && !h.env.memory.Write(uint32(ptr), []byte(payload)) {
		return 0, fmt.Errorf("guest memory write failed at %d", ptr)
	}
	ret, err := first(h.env.dispatch.Call(context.Background(), id, ptr, uint64(len(payload))))
	if err != nil {
		return 0, fmt.Errorf("kit_dispatch: %w", err)
	}
	return ret, nil
}

func (h *wasmHost) readStr(ptr, len uint32) string {
	return readGuestString(h.env.memory, ptr, len)
}

func readGuestString(m api.Memory, ptr, n uint32) string {
	if n == 0 || m == nil {
		return ""
	}
	b, ok := m.Read(ptr, n)
	if !ok {
		return ""
	}
	return string(b)
}

func first(vs []uint64, err error) (uint64, error) {
	if err != nil {
		return 0, err
	}
	if len(vs) == 0 {
		return 0, fmt.Errorf("no result")
	}
	return vs[0], nil
}

// writeGuest copies s into guest memory; returns ptr, len. The guest reads
// the length from the import's second result.
func (h *wasmHost) writeGuest(s string) (uint32, uint32) {
	if h.env == nil {
		return 0, 0
	}
	ptr, err := first(h.env.alloc.Call(context.Background(), uint64(len(s))))
	if err != nil {
		return 0, 0
	}
	if len(s) > 0 && !h.env.memory.Write(uint32(ptr), []byte(s)) {
		return 0, 0
	}
	return uint32(ptr), uint32(len(s))
}
