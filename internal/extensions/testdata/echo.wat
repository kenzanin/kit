;; echo.wat — minimal Kit native extension used by nativeext_test.go and
;; examples/extensions/ping.wasm. Rebuild with: wat2wasm echo.wasm -o echo.wasm
;;
;; ABI (see ext_wasm.go): kit_handle(evt,en,pay,payn) -> ptr, kit_result_len.
;; Responses are static, selected by event-name length:
;;   describe(8) -> manifest, tool_call(9) -> block result, command:ping(12) -> pong
(module
  (memory (export "memory") 1)
  (global $p (mut i32) (i32.const 4096))
  (global $rlen (mut i32) (i32.const 0))
  (data (i32.const 256) "{\"commands\":[{\"name\":\"ping\",\"description\":\"wasm ping\"}],\"events\":[\"agent_start\",\"tool_call\"]}")
  (data (i32.const 512) "{\"Block\":true,\"Reason\":\"blocked by wasm\"}")
  (data (i32.const 768) "pong")

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

  (func (export "kit_handle") (param $e i32) (param $en i32) (param $q i32) (param $qn i32) (result i32)
    local.get $en i32.const 8 i32.eq
    if (result i32)
      i32.const 256 i32.const 93 call $ret
    else
      local.get $en i32.const 9 i32.eq
      if (result i32)
        i32.const 512 i32.const 41 call $ret
      else
        local.get $en i32.const 12 i32.eq
        if (result i32)
          i32.const 768 i32.const 4 call $ret
        else
          i32.const 0 i32.const 0 call $ret
        end
      end
    end))
