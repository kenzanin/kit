-- ping.lua — smallest native Kit extension: a /ping command and a log line
-- on agent start. Load with `kit -e ping.lua` or drop it in
-- ~/.config/kit/extensions/.
--
-- The contract is one function: kit_handle(event, payload). "describe" is
-- probed once at load; result-events answer with JSON; commands answer with
-- plain text. See internal/extensions/native.go.
function kit_handle(event, payload)
  if event == "describe" then
    return '{"commands":[{"name":"ping","description":"reply pong"}],"events":["agent_start"]}'
  end
  if event == "command:ping" then
    return "pong " .. payload
  end
  if event == "agent_start" then
    kit.log("agent started")
  end
  return ""
end
