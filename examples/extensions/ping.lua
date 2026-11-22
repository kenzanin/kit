-- ping.lua — smallest native Kit extension: a /ping command and a log line
-- on agent start. Load with `kit -e ping.lua` or drop it in
-- ~/.config/kit/extensions/.
--
-- Native plugins register imperatively, exactly like Go ones: define
-- Init(api); see internal/extensions/luahost.go and docs.
function Init(api)
  api.on("agent_start", function(ev)
    kit.log("agent started")
  end)
  api.register_command{
    name = "ping", description = "reply pong",
    execute = function(args) return "pong " .. (args or "") end,
  }
end
