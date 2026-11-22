package extensions

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestNativeExtension_Lua loads a Lua extension and checks the whole pipe:
// Init(api) -> registered handlers -> result JSON round trip -> command.
func TestNativeExtension_Lua(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guard.lua")
	src := `
function Init(api)
  api.on("agent_start", function(ev) end)
  api.on("tool_call", function(ev)
    if ev.ToolName == "shell" then
      return { Block = true, Reason = "no shell for you" }
    end
    return nil
  end)
  api.register_tool{
    name = "greeter", description = "greets",
    execute = function(input) return "tool hi" end,
  }
  api.register_command{
    name = "hi", description = "greets",
    execute = function(args) return "hi " .. args end,
  }
end
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	ext, err := loadSingleExtension(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := len(ext.Handlers[AgentStart]); got != 1 {
		t.Fatalf("agent_start handlers = %d, want 1", got)
	}

	// Result round trip: the Lua table answer becomes a typed ToolCallResult.
	hf := ext.Handlers[ToolCall][0]
	res := hf(ToolCallEvent{ToolName: "shell"}, Context{})
	tr, ok := res.(ToolCallResult)
	if !ok || !tr.Block || tr.Reason != "no shell for you" {
		t.Fatalf("tool_call result = %#v, want blocked", res)
	}
	if res := hf(ToolCallEvent{ToolName: "read"}, Context{}); res != nil {
		t.Fatalf("tool_call(read) result = %#v, want nil", res)
	}

	// Tools and commands land in the same registries as Go extensions.
	if len(ext.Tools) != 1 || ext.Tools[0].Name != "greeter" {
		t.Fatalf("tools = %#v, want [greeter]", ext.Tools)
	}
	if out, err := ext.Tools[0].Execute("{}"); err != nil || out != "tool hi" {
		t.Fatalf("tool greeter = %q, %v", out, err)
	}
	if len(ext.Commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(ext.Commands))
	}
	cmd, err := ext.Commands[0].Execute("ken", Context{})
	if err != nil || cmd != "hi ken" {
		t.Fatalf("command hi = %q, %v; want %q, nil", cmd, err, "hi ken")
	}
}

// TestNativeExtension_Wasm does the same for the checked-in wazero fixture
// (testdata/echo.wasm, built from echo.wat).
func TestNativeExtension_Wasm(t *testing.T) {
	path := "testdata/echo.wasm"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture missing: %v", err)
	}

	ext, err := loadSingleExtension(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := len(ext.Handlers[AgentStart]); got != 1 {
		t.Fatalf("agent_start handlers = %d, want 1", got)
	}
	if got := len(ext.Handlers[ToolCall]); got != 1 {
		t.Fatalf("tool_call handlers = %d, want 1", got)
	}
	hf := ext.Handlers[ToolCall][0]
	tr, ok := hf(ToolCallEvent{}, Context{}).(ToolCallResult)
	if !ok || !tr.Block || tr.Reason != "blocked by wasm" {
		t.Fatalf("tool_call result = %#v, want blocked", tr)
	}
	if len(ext.Commands) != 1 || ext.Commands[0].Name != "ping" {
		t.Fatalf("commands = %#v, want [ping]", ext.Commands)
	}
	out, err := ext.Commands[0].Execute("x", Context{})
	if err != nil || out != "pong" {
		t.Fatalf("ping = %q, %v; want pong, nil", out, err)
	}
}

// TestNativeExtension_RtkTool: the examples rtk.lua wrapper — full pipe
// Lua tool -> io.popen -> rtk binary. Skipped where rtk is not installed.
func TestNativeExtension_RtkTool(t *testing.T) {
	if _, err := exec.LookPath("rtk"); err != nil {
		t.Skip("rtk not installed")
	}
	ext, err := loadSingleExtension("../../examples/extensions/rtk.lua")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(ext.Tools) != 1 || ext.Tools[0].Name != "rtk" {
		t.Fatalf("tools = %#v", ext.Tools)
	}
	out, err := ext.Tools[0].Execute(`{"args":"git status -s` + `"}`)
	if err != nil {
		t.Fatalf("rtk execute: %v", err)
	}
	if out == "" {
		t.Fatal("rtk returned empty output")
	}
}

// TestNativeExtension_MissingEntry checks the load-failure path.
func TestNativeExtension_MissingEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.lua")
	if err := os.WriteFile(path, []byte(`x = 1`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSingleExtension(path); err == nil {
		t.Fatal("want error for script without Init(api)")
	}
}
