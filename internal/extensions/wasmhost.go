package extensions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/charmbracelet/log"
	wazero "github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// wasmHost runs a .wasm extension in wazero. Required guest exports:
//
//	memory                                        linear memory
//	kit_alloc(len) ptr                            growable scratch for host->guest bytes
//	kit_handle(evtPtr, evtLen, payPtr, payLen) ptr result pointer
//	kit_result_len() len                          length of the last result
//
// Result memory belongs to the guest allocator. WASI is instantiated so
// toolchain-built modules (TinyGo, Rust, ...) find their imports; it is not
// otherwise used by the ABI.
type wasmHost struct {
	mu     sync.Mutex
	ctx    context.Context
	r      wazero.Runtime
	mod    api.Closer
	alloc  api.Function
	handle api.Function
	rlen   api.Function
	mem    api.Memory
}

// newWasmHost compiles and instantiates the module, then resolves the ABI exports.
func newWasmHost(path string) (nativeHost, error) {
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)

	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	_, err := r.NewHostModuleBuilder("kit").
		NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, ptr, n uint32) {
		// ponytail: debug-level only; see luaHost log note.
		if b, ok := m.Memory().Read(ptr, n); ok {
			log.Debug(string(b), "ext", filepath.Base(path))
		}
	}).Export("log").
		Instantiate(ctx)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("host module: %w", err)
	}

	src, err := os.ReadFile(path)
	if err != nil {
		r.Close(ctx)
		return nil, err
	}
	mod, err := r.Instantiate(ctx, src)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("instantiating %s: %w", filepath.Base(path), err)
	}

	h := &wasmHost{ctx: ctx, r: r, mod: mod, mem: mod.Memory()}
	for _, e := range []struct {
		name string
		dst  *api.Function
	}{{"kit_alloc", &h.alloc}, {"kit_handle", &h.handle}, {"kit_result_len", &h.rlen}} {
		if *e.dst = mod.ExportedFunction(e.name); *e.dst == nil {
			mod.Close(ctx)
			r.Close(ctx)
			return nil, fmt.Errorf("%s: missing export %s", filepath.Base(path), e.name)
		}
	}
	if h.mem == nil {
		mod.Close(ctx)
		r.Close(ctx)
		return nil, fmt.Errorf("%s: no exported memory", filepath.Base(path))
	}
	return h, nil
}

func (h *wasmHost) call(event, payload string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	evtPtr, err := h.write(event)
	if err != nil {
		return "", err
	}
	payPtr, err := h.write(payload)
	if err != nil {
		return "", err
	}

	res, err := h.handle.Call(h.ctx, uint64(evtPtr), uint64(len(event)), uint64(payPtr), uint64(len(payload)))
	if err != nil {
		return "", fmt.Errorf("kit_handle: %w", err)
	}
	lens, err := h.rlen.Call(h.ctx)
	if err != nil {
		return "", fmt.Errorf("kit_result_len: %w", err)
	}
	n := uint32(lens[0])
	if n == 0 {
		return "", nil
	}
	out, ok := h.mem.Read(uint32(res[0]), n)
	if !ok {
		return "", fmt.Errorf("kit_handle: result outside memory")
	}
	return string(out), nil
}

// write copies s into guest scratch memory via kit_alloc.
func (h *wasmHost) write(s string) (uint32, error) {
	if len(s) == 0 {
		return 0, nil
	}
	res, err := h.alloc.Call(h.ctx, uint64(len(s)))
	if err != nil {
		return 0, fmt.Errorf("kit_alloc: %w", err)
	}
	ptr := uint32(res[0])
	if !h.mem.Write(ptr, []byte(s)) {
		return 0, fmt.Errorf("kit_alloc: returned range outside memory")
	}
	return ptr, nil
}

func (h *wasmHost) close() {
	h.mod.Close(h.ctx)
	h.r.Close(h.ctx)
}
