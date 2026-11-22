package extensions

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	"github.com/charmbracelet/log"
)

// Native extensions are non-Go plugins (.lua, .wasm) that inherit the full
// extension API. The host never speaks a side-protocol: the plugin's entry
// point receives an API adapter and registers imperatively, exactly like a
// Go extension's Init(api). Across the VM boundary every value is JSON; the
// per-runtime hosts (luahost.go, wasmhost.go) translate to native values.
//
// The only runtime contract is nativeHost: invoke a guest callback by id.
type nativeHost interface {
	// invoke calls guest callback id with a JSON payload; the reply is JSON
	// for result-shaped events ("" = nil) or the raw answer otherwise.
	invoke(id int64, payload string) (string, error)
	// close releases the runtime.
	close()
}

// nativeBridge adapts one native extension. Registration calls land here
// from the guest and are forwarded to the recording API; dispatches go the
// other way through nativeHost. Guest invocation is serialized: extensions
// are single-threaded, like the yaegi path.
type nativeBridge struct {
	rt nativeHost

	mu  sync.Mutex
	ctx *Context // context of the callback in flight, for print/ctx env
}

// nativeEvents maps the guest-visible event name to the API registration
// method that takes its handler. Result-shaped events are detected from the
// method signature; no per-event wrapper is hand written.
//
// ponytail: parity with api.go is enforced by TestNativeBindingParity; add
// a row when API grows, or the test fails.
var nativeEvents = map[string]string{
	"tool_call":             "OnToolCall",
	"tool_call_input_start": "OnToolCallInputStart",
	"tool_call_input_delta": "OnToolCallInputDelta",
	"tool_call_input_end":   "OnToolCallInputEnd",
	"tool_execution_start":  "OnToolExecutionStart",
	"tool_execution_end":    "OnToolExecutionEnd",
	"tool_output":           "OnToolOutput",
	"tool_result":           "OnToolResult",
	"input":                 "OnInput",
	"before_agent_start":    "OnBeforeAgentStart",
	"agent_start":           "OnAgentStart",
	"agent_end":             "OnAgentEnd",
	"message_start":         "OnMessageStart",
	"message_update":        "OnMessageUpdate",
	"message_end":           "OnMessageEnd",
	"session_start":         "OnSessionStart",
	"session_shutdown":      "OnSessionShutdown",
	"model_change":          "OnModelChange",
	"thinking_level_change": "OnThinkingLevelChange",
	"terminal_resize":       "OnTerminalResize",
	"turn_state_change":     "OnTurnStateChange",
	"mcp_tools_ready":       "OnMCPToolsReady",
	"context_prepare":       "OnContextPrepare",
	"before_fork":           "OnBeforeFork",
	"before_session_switch": "OnBeforeSessionSwitch",
	"before_compact":        "OnBeforeCompact",
	"subagent_start":        "OnSubagentStart",
	"subagent_chunk":        "OnSubagentChunk",
	"subagent_end":          "OnSubagentEnd",
	"step_start":            "OnStepStart",
	"step_finish":           "OnStepFinish",
	"reasoning_start":       "OnReasoningStart",
	"warnings":              "OnWarnings",
	"source":                "OnSource",
	"error":                 "OnError",
	"retry":                 "OnRetry",
	"step_resume":           "OnStepResume",
	"prepare_step":          "OnPrepareStep",
	"llm_usage":             "OnLLMUsage",
}

// on registers one guest callback for the named event. Names outside
// nativeEvents become custom events on the inter-extension bus.
func (b *nativeBridge) on(api *API, event string, id int64) error {
	methName, ok := nativeEvents[event]
	if !ok {
		api.OnCustomEvent(event, func(payload string) {
			if _, err := b.call(id, payload, nil); err != nil {
				log.Warn("native extension error", "event", event, "err", err)
			}
		})
		return nil
	}

	m := reflect.ValueOf(api).MethodByName(methName)
	if !m.IsValid() {
		return fmt.Errorf("event %q: no API method %s", event, methName)
	}
	h := m.Type().In(0) // func(Event, Context) [Result]

	fn := reflect.MakeFunc(h, func(args []reflect.Value) []reflect.Value {
		out, err := b.call(id, mustJSON(args[0].Interface()), ctxPtr(args[1]))
		if err != nil {
			log.Warn("native extension error", "event", event, "err", err)
		}
		if h.NumOut() == 0 {
			return nil
		}
		if err != nil || out == "" {
			return []reflect.Value{reflect.Zero(h.Out(0))}
		}
		res := reflect.New(h.Out(0).Elem())
		if err := json.Unmarshal([]byte(out), res.Interface()); err != nil {
			log.Warn("native extension bad result JSON", "event", event, "err", err)
			return []reflect.Value{reflect.Zero(h.Out(0))}
		}
		return []reflect.Value{res}
	})
	m.Call([]reflect.Value{fn})
	return nil
}

// registerTool: meta JSON {name,description,parameters?}; the callback gets
// the JSON tool input and replies with the text result.
func (b *nativeBridge) registerTool(api *API, meta string, id int64) error {
	var d struct{ Name, Description, Parameters string }
	if err := json.Unmarshal([]byte(meta), &d); err != nil {
		return fmt.Errorf("register_tool meta: %w", err)
	}
	if d.Name == "" {
		return fmt.Errorf("register_tool: name required")
	}
	api.RegisterTool(ToolDef{
		Name: d.Name, Description: d.Description, Parameters: d.Parameters,
		Execute: func(input string) (string, error) { return b.call(id, input, nil) },
	})
	return nil
}

// registerCommand: meta {name,description}; callback gets the args string.
func (b *nativeBridge) registerCommand(api *API, meta string, id int64) error {
	var d struct{ Name, Description string }
	if err := json.Unmarshal([]byte(meta), &d); err != nil {
		return fmt.Errorf("register_command meta: %w", err)
	}
	if d.Name == "" {
		return fmt.Errorf("register_command: name required")
	}
	api.RegisterCommand(CommandDef{
		Name: d.Name, Description: d.Description,
		Execute: func(args string, ctx Context) (string, error) {
			return b.call(id, args, &ctx)
		},
	})
	return nil
}

// registerOption: pure data, no callback.
func (b *nativeBridge) registerOption(api *API, meta string) error {
	var opt OptionDef
	if err := json.Unmarshal([]byte(meta), &opt); err != nil {
		return fmt.Errorf("register_option meta: %w", err)
	}
	api.RegisterOption(opt)
	return nil
}

// registerShortcut: meta {key,description}; callback fires with no payload.
func (b *nativeBridge) registerShortcut(api *API, meta string, id int64) error {
	var def ShortcutDef
	if err := json.Unmarshal([]byte(meta), &def); err != nil {
		return fmt.Errorf("register_shortcut meta: %w", err)
	}
	api.RegisterShortcut(def, func(ctx Context) {
		if _, err := b.call(id, "", &ctx); err != nil {
			log.Warn("native extension error", "shortcut", def.Key, "err", err)
		}
	})
	return nil
}

// registerToolRenderer: meta {tool_name}; callbacks for render_header and
// render arrive as separate ids in the "ids" map {render_header,render}.
func (b *nativeBridge) registerToolRenderer(api *API, meta string, ids map[string]int64) error {
	var cfg ToolRenderConfig
	if err := json.Unmarshal([]byte(meta), &cfg); err != nil {
		return fmt.Errorf("register_tool_renderer meta: %w", err)
	}
	if id, ok := ids["render_header"]; ok {
		cfg.RenderHeader = func(s string, width int) string {
			return b.renderCall(id, mustJSON(map[string]any{"text": s, "width": width}), false)
		}
	}
	if id, ok := ids["render_body"]; ok {
		cfg.RenderBody = func(s string, isErr bool, width int) string {
			return b.renderCall(id, mustJSON(map[string]any{"text": s, "is_error": isErr, "width": width}), true)
		}
	}
	api.RegisterToolRenderer(cfg)
	return nil
}

// registerMessageRenderer: meta {name}; callback id under "render".
func (b *nativeBridge) registerMessageRenderer(api *API, meta string, ids map[string]int64) error {
	var cfg MessageRendererConfig
	if err := json.Unmarshal([]byte(meta), &cfg); err != nil {
		return fmt.Errorf("register_message_renderer meta: %w", err)
	}
	id, ok := ids["render"]
	if !ok {
		return fmt.Errorf("register_message_renderer: render callback required")
	}
	cfg.Render = func(s string, width int) string {
		return b.renderCall(id, mustJSON(map[string]any{"text": s, "width": width}), false)
	}
	api.RegisterMessageRenderer(cfg)
	return nil
}

func (b *nativeBridge) renderCall(id int64, payload string, _ bool) string {
	out, err := b.call(id, payload, nil)
	if err != nil {
		log.Warn("native extension renderer error", "err", err)
		return ""
	}
	return out
}

// call invokes guest callback id, serializing access and publishing ctx so
// the runtime's print/ctx environment routes to the right place.
func (b *nativeBridge) call(id int64, payload string, ctx *Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	prev := b.ctx
	b.ctx = ctx
	defer func() { b.ctx = prev }()
	return b.rt.invoke(id, payload)
}

// env returns the context of the callback in flight (guest kit.ctx()).
func (b *nativeBridge) env() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx == nil {
		return "{}"
	}
	return ctxJSON(*b.ctx)
}

func ctxPtr(v reflect.Value) *Context {
	ctx, ok := v.Interface().(Context)
	if !ok {
		return nil
	}
	return &ctx
}

// ctxJSON marshals the scalar surface of a Context; func fields (Print...)
// are host-provided environment, not data.
func ctxJSON(c Context) string {
	return mustJSON(struct {
		SessionID   string
		CWD         string
		Model       string
		Interactive bool
	}{c.SessionID, c.CWD, c.Model, c.Interactive})
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
