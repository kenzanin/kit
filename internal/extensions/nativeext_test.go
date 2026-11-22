package extensions

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNativeExtension_Lua loads a Lua extension and checks the whole pipe:
// describe probe -> registered handlers -> result JSON round trip -> command.
func TestNativeExtension_Lua(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guard.lua")
	src := `function kit_handle(event, payload)
  if event == "describe" then
    return '{"commands":[{"name":"hi","description":"greets"}],"events":["agent_start","tool_call"]}'
  end
  if event == "command:hi" then
    return "hi " .. payload
  end
  if event == "tool_call" and payload:find("shell") then
    return '{"Block":true,"Reason":"no shell for you"}'
  end
  kit.log("event " .. event)
  return ""
end`
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

	// Result round trip: the JSON answer becomes a typed ToolCallResult.
	hf := ext.Handlers[ToolCall][0]
	res := hf(ToolCallEvent{ToolName: "shell"}, Context{})
	tr, ok := res.(ToolCallResult)
	if !ok || !tr.Block || tr.Reason != "no shell for you" {
		t.Fatalf("tool_call result = %#v, want blocked", res)
	}
	if res := hf(ToolCallEvent{ToolName: "read"}, Context{}); res != nil {
		t.Fatalf("tool_call(read) result = %#v, want nil", res)
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
	if ext.Commands[0].Name != "ping" {
		t.Fatalf("command = %q, want ping", ext.Commands[0].Name)
	}
	out, err := ext.Commands[0].Execute("x", Context{})
	if err != nil || out != "pong" {
		t.Fatalf("ping = %q, %v; want pong, nil", out, err)
	}
}

// TestNativeExtension_MissingHandler checks the load-failure path.
func TestNativeExtension_MissingHandler(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.lua")
	if err := os.WriteFile(path, []byte(`x = 1`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSingleExtension(path); err == nil {
		t.Fatal("want error for script without kit_handle")
	}
}
