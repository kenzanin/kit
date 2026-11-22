;; echo.wat — minimal Kit native wasm extension (ABI: see wasmhost.go).
;; Rebuild: wat2wasm echo.wat -o echo.wasm
;; kit_init registers a tool_call handler (id 1) and /ping (id 2) via imports;
;; kit_dispatch answers by id with static JSON/text.
(module
  (import "kit" "on" (func $on (param i32 i32 i64)))
  (import "kit" "register_command" (func $regcmd (param i32 i32 i64)))
  (import "kit" "log" (func $log (param i32 i32)))
  (memory (export "memory") 1)
  (global $p (mut i32) (i32.const 8192))
  (global $rlen (mut i32) (i32.const 0))

  (data (i32.const 256) "{\"name\":\"ping\",\"description\":\"wasm ping\"}")
  (data (i32.const 384) "tool_call")
  (data (i32.const 512) "{\"Block\":true,\"Reason\":\"blocked by wasm\"}")
  (data (i32.const 768) "pong")
  (data (i32.const 896) "agent_start")
  (data (i32.const 960) "ext tick")

  (func $ret (param $ptr i32) (param $n i32) (result i32)
    local.get $n
    global.set $rlen
    local.get $ptr)

  (func (export "kit_alloc") (param $n i32) (result i32)
    (local $r i32)
    global.get $p
    local.set $r
    global.get $p
    local.get $n
    i32.add
    global.set $p
    local.get $r)

  (func (export "kit_result_len") (result i32)
    global.get $rlen)

  (func (export "kit_init")
    (call $on (i32.const 384) (i32.const 9) (i64.const 1))
    (call $regcmd (i32.const 256) (i32.const 41) (i64.const 2))
    (call $on (i32.const 896) (i32.const 11) (i64.const 3)))

  (func (export "kit_dispatch") (param $id i64) (param $ptr i32) (param $n i32) (result i32)
    (if (i64.eq (local.get $id) (i64.const 1))
      (then
        (call $log (i32.const 960) (i32.const 8))
        (return (call $ret (i32.const 512) (i32.const 41)))))
    (if (i64.eq (local.get $id) (i64.const 2))
      (then (return (call $ret (i32.const 768) (i32.const 4)))))
    (i32.const 0))
)
