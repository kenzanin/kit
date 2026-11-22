---
title: Native Extensions (Lua & Wasm)
description: Write Kit extensions in Lua or WebAssembly with the same API as Go extensions.
ogImage: /og-image.png
---

# Native Extensions (Lua & Wasm)

Extensions can be written in **Lua** (`.lua`) or **WebAssembly** (`.wasm`)
instead of Go. Native extensions are not a reduced side-protocol: they
receive the same extension API Go extensions do and register imperatively
from a single entry point.

| | Go | Lua | Wasm |
|---|---|---|---|
| Entry point | `func Init(api ext.API)` | `function Init(api)` | `kit_init` export |
| Events | `api.OnToolCall(...)` | `api.on("tool_call", fn)` | `kit.on` import |
| Tools | `api.RegisterTool(...)` | `api.register_tool{...}` | `kit.register_tool` import |
| Commands | `api.RegisterCommand(...)` | `api.register_command{...}` | `kit.register_command` |
| Options / shortcuts / renderers | yes | yes | yes |
| Runtime | yaegi (Go) | gopher-lua | wazero |
| cgo | none | none | none |

Every `On*` method on the extension API has a native binding; a parity test
fails the build if the API grows one that is missing. Values cross the
boundary as JSON: event structs become tables/objects, and a returned table
is decoded into the typed result (e.g. `{ Block = true, Reason = "..." }`).

## Lua

```lua
function Init(api)
  api.on("tool_call", function(ev)
    if ev.ToolName == "shell" then
      return { Block = true, Reason = "no shell" }
    end
    return nil
  end)

  api.register_tool{
    name = "greeter", description = "Greets someone",
    parameters = '{"type":"object","properties":{"name":{"type":"string"}}}',
    execute = function(input) return "hello " .. input end,
  }

  api.register_command{
    name = "ping", description = "reply pong",
    execute = function(args) return "pong " .. args end,
  }
end
```

Host environment (`kit` table):

| Function | Purpose |
|---|---|
| `kit.log(msg)` | debug log line (tagged with the extension name) |
| `kit.print(msg)` / `print_info` / `print_error` | user-facing output through the Context print surface |
| `kit.print_block(msg)` | styled block output |
| `kit.ctx()` | table of the in-flight Context scalars (session id, cwd, model, interactive) |

The full Lua standard library is available, matching the trust level of Go
extensions.

## Wasm

Guest exports:

| Export | Signature | Purpose |
|---|---|---|
| `memory` | — | linear memory |
| `kit_init` | `() -> ()` | register capabilities |
| `kit_alloc` | `(i32) -> i32` | allocate host-written payloads |
| `kit_result_len` | `() -> i32` | length of the last result |
| `kit_dispatch` | `(i64 id, i32 ptr, i32 len) -> i32` | callback entry |

Host imports (module `"kit"`): `on`, `register_tool`, `register_command`,
`register_option`, `register_shortcut`, `register_tool_renderer`,
`register_message_renderer`, `log`, `print`, `print_info`, `print_error`,
`print_block`, `ctx`. See `internal/extensions/testdata/echo.wat` for a
minimal complete module and `internal/extensions/wasmhost.go` for the ABI.

## Discovery and loading

`.lua` and `.wasm` files are discovered everywhere `.go` files are:
`/usr/share/kit/extensions/`, `~/.config/kit/extensions/`,
`.kit/extensions/`, and subdirectories with `main.lua` / `main.wasm`.
Explicit `-e path.lua` / `--extension path.wasm` works too. Check what
loaded:

```bash
kit extensions list -e examples/extensions/ping.lua
```

## Failure behavior

- A load failure (missing `Init`, missing wasm export, bad syntax) skips only
  that extension; the rest still load.
- A callback error or wasm trap is logged with the extension and event name;
  the event pipeline and session continue.
- Guest calls are serialized per extension: extensions are single-threaded,
  like the Go/yaegi path.

## Not yet exposed

Native plugins currently cannot call the `Context` send-message / widget
surfaces beyond the print functions; use Go extensions for those until the
bindings cover them (tracked in `openspec/changes/add-native-plugin-runtimes`).
