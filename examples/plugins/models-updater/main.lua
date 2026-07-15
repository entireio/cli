-- models-updater: an example command plugin that downloads a models.json
-- pricing file and caches it inside the repo.
--
-- It demonstrates the http + fs capabilities working together, and how a
-- capability-gated call fails loudly when the grant is missing.
--
-- Enable it in .entire/settings.json:
--   "plugins": { "models-updater": { "enabled": true, "capabilities": ["http", "fs"] } }
--
-- Usage:
--   entire models-update [url]

local DEFAULT_URL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
local DEST = ".entire/models.json"

entire.command{
  name = "models-update",
  short = "download the latest models.json into .entire/",
  run = function(args)
    local url = args[1] or DEFAULT_URL

    entire.print("Fetching " .. url .. " ...")
    local resp = entire.http.get(url) -- requires the "http" capability
    if resp.status ~= 200 then
      entire.print(string.format("error: server returned HTTP %d", resp.status))
      return 1
    end

    entire.fs.write(DEST, resp.body) -- requires the "fs" capability; confined to the repo
    entire.print(string.format("Wrote %d bytes to %s", #resp.body, DEST))
    entire.log.info("models.json updated from " .. url)
    return 0
  end,
}
