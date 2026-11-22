package extensions

import (
	"encoding/json"
	"fmt"

	"github.com/charmbracelet/log"
)

// Native extensions are non-Go plugins (.lua, .wasm) that plug into the same
// event system as Yaegi extensions. Both runtimes speak one contract: the
// plugin exposes a single handler
//
//	handle(event string, payload string) string
//
// and the host drives it:
//
//	"describe"          -> JSON {"commands":[{"name","description"},...],
//	                            "events":["tool_call",...]}
//	<EventType>         -> event struct as JSON; result-events answer with
//	                       the result struct as JSON (empty string = nil)
//	"command:<name>"    -> command arguments as plain text; plain text out
//
// The full event list is in events.go. Events the plugin does not list in
// "events" are never delivered. Plugins log via kit.log (Lua module function,
// wasm host import "kit/log").
type nativeHost interface {
	// call invokes the plugin handler and returns its string result.
	call(event, payload string) (string, error)
	// close releases the runtime.
	close()
}

// nativeEvent is one host-delivered event: the EventType name and how to
// register a handler for it on the API. result events unmarshal the plugin's
// answer into the typed result; notification events ignore it.
type nativeEvent struct {
	event string
	apply func(API, nativeHost)
}

// nativeEvents is the set of events native plugins can receive. ponytail:
// add a row (3 lines) per event when a plugin needs more; the plumbing is
// already generic.
var nativeEvents = []nativeEvent{
	{string(ToolCall), func(a API, h nativeHost) {
		a.OnToolCall(func(e ToolCallEvent, _ Context) *ToolCallResult {
			return nativeResult[ToolCallResult](h, ToolCall, e)
		})
	}},
	{string(ToolResult), func(a API, h nativeHost) {
		a.OnToolResult(func(e ToolResultEvent, _ Context) *ToolResultResult {
			return nativeResult[ToolResultResult](h, ToolResult, e)
		})
	}},
	{string(AgentStart), func(a API, h nativeHost) {
		a.OnAgentStart(func(e AgentStartEvent, _ Context) { nativeNotify(h, AgentStart, e) })
	}},
	{string(AgentEnd), func(a API, h nativeHost) {
		a.OnAgentEnd(func(e AgentEndEvent, _ Context) { nativeNotify(h, AgentEnd, e) })
	}},
	{string(SessionStart), func(a API, h nativeHost) {
		a.OnSessionStart(func(e SessionStartEvent, _ Context) { nativeNotify(h, SessionStart, e) })
	}},
	{string(SessionShutdown), func(a API, h nativeHost) {
		a.OnSessionShutdown(func(e SessionShutdownEvent, _ Context) { nativeNotify(h, SessionShutdown, e) })
	}},
}

// nativeMeta is the parsed "describe" answer.
type nativeMeta struct {
	Commands []struct {
		Name        string
		Description string
	}
	Events []string
}

// compileNative opens the plugin runtime, probes it with "describe" and
// returns an init func wiring the announced events and commands into the
// standard extension API, so runners and reload treat native plugins like
// any other.
func compileNative(path string, open func(string) (nativeHost, error)) (func(API), error) {
	h, err := open(path)
	if err != nil {
		return nil, err
	}

	out, err := h.call("describe", "")
	if err != nil {
		h.close()
		return nil, fmt.Errorf("describe probe failed: %w", err)
	}
	var meta nativeMeta
	if out != "" {
		if err := json.Unmarshal([]byte(out), &meta); err != nil {
			h.close()
			return nil, fmt.Errorf("bad describe JSON: %w", err)
		}
	}

	want := make(map[string]bool, len(meta.Events))
	for _, e := range meta.Events {
		want[e] = true
	}

	return func(api API) {
		for _, ev := range nativeEvents {
			if want[ev.event] {
				ev.apply(api, h)
			}
		}
		for _, c := range meta.Commands {
			name := c.Name
			api.RegisterCommand(CommandDef{
				Name:        name,
				Description: c.Description,
				Execute: func(args string, _ Context) (string, error) {
					return h.call("command:"+name, args)
				},
			})
		}
	}, nil
}

// nativeNotify delivers a notification event; errors are logged, never fatal.
func nativeNotify(h nativeHost, event EventType, e any) {
	if _, err := h.call(string(event), mustJSON(e)); err != nil {
		log.Warn("native extension error", "event", event, "err", err)
	}
}

// nativeResult delivers a result event and decodes the typed answer.
func nativeResult[R Result](h nativeHost, event EventType, e any) *R {
	out, err := h.call(string(event), mustJSON(e))
	if err != nil || out == "" {
		if err != nil {
			log.Warn("native extension error", "event", event, "err", err)
		}
		return nil
	}
	var r R
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		log.Warn("native extension bad result JSON", "event", event, "err", err)
		return nil
	}
	return &r
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
