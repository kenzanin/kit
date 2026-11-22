-- rtk.lua — expose rtk (Rust token killer, ~/.local/bin/rtk) as a Kit tool:
-- the agent calls tool `rtk` with {"args":"git status"} and gets the
-- token-optimized output. Subcommands: ls tree read git gh glab aws psql
-- err test json deps find diff log dotnet docker, `rtk <cmd>` proxying.
--
-- Install: cp to ~/.config/kit/extensions/ (or load with -e).
function Init(api)
  api.register_tool{
    name = "rtk",
    description = "Run a command through rtk for token-optimized output. "
      .. "args = subcommand plus arguments, e.g. 'git status', 'err make test', 'read path'.",
    parameters = '{"type":"object","properties":{"args":{"type":"string"}},"required":["args"]}',
    execute = function(input)
      local req = input
      if type(req) == "string" then req = kit.json(req) end
      local args = tostring(req.args or "")
      if args == "" then error("args required") end
      local pipe = io.popen("rtk " .. args .. " 2>&1")
      local out = pipe:read("*a") or ""
      local ok = pipe:close()
      if not ok and out == "" then error("rtk failed") end
      return out
    end,
  }
end
