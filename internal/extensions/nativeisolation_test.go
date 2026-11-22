package extensions

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestNativeExtension_LuaCallbackError: a handler that errors is logged and
// surfaces as nil/no-op, never a panic or pipeline abort.
func TestNativeExtension_LuaCallbackError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boom.lua")
	src := `
function Init(api)
  api.on("tool_call", function(ev) error("boom") end)
  api.on("agent_start", function(ev) error("boom") end)
end
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ext, err := loadSingleExtension(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if res := ext.Handlers[ToolCall][0](ToolCallEvent{}, Context{}); res != nil {
		t.Fatalf("tool_call result = %#v, want nil after error", res)
	}
	ext.Handlers[AgentStart][0](AgentStartEvent{}, Context{}) // must not panic
}

// TestNativeExtension_ConcurrentInvoke: the bridge serializes guest access;
// parallel dispatch of the same handler is safe (wasm reentrancy guard), and
// the guest may call host imports (kit.log) mid-callback.
func TestNativeExtension_ConcurrentInvoke(t *testing.T) {
	path := "testdata/echo.wasm"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	ext, err := loadSingleExtension(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	hf := ext.Handlers[ToolCall][0]
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr, ok := hf(ToolCallEvent{}, Context{}).(ToolCallResult)
			if !ok || !tr.Block {
				t.Errorf("concurrent result = %#v, want blocked", tr)
			}
		}()
	}
	wg.Wait()
}

// TestNativeExtension_ReloadReplacesHandlers: the watcher reload path loads
// the new version fresh; the old extension keeps its registered handlers and
// the new one is independent (ponytail: VM teardown is GC'd — pure-Go
// runtimes, no explicit close hook on LoadedExtension yet).
func TestNativeExtension_ReloadReplacesHandlers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reloader.lua")
	write := func(handlers int) {
		src := "function Init(api)\n"
		for i := 0; i < handlers; i++ {
			src += `  api.on("agent_start", function(ev) end)` + "\n"
		}
		src += "end\n"
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(1)
	v1, err := loadSingleExtension(path)
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	write(3)
	v2, err := loadSingleExtension(path)
	if err != nil {
		t.Fatalf("load v2: %v", err)
	}
	if got := len(v1.Handlers[AgentStart]); got != 1 {
		t.Fatalf("v1 agent_start handlers = %d, want 1", got)
	}
	if got := len(v2.Handlers[AgentStart]); got != 3 {
		t.Fatalf("v2 agent_start handlers = %d, want 3", got)
	}
}
