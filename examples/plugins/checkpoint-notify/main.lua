-- checkpoint-notify: a minimal observer plugin.
--
-- On each checkpoint it bumps a persistent counter (entire.kv) and logs a line.
-- On each commit it logs the commit. If the `exec` capability is granted AND a
-- notifier is available, it sends a desktop notification (best-effort).
--
-- Enable it in .entire/settings.json:
--   "plugins": { "checkpoint-notify": { "enabled": true, "capabilities": ["exec"] } }
-- The "exec" grant is optional; without it the desktop notification is skipped
-- and the plugin still logs and counts.

local function bump_count()
  local n = tonumber(entire.kv.get("checkpoints") or "0") + 1
  entire.kv.set("checkpoints", tostring(n))
  return n
end

-- notify tries a couple of common notifiers; failures are swallowed so a
-- missing notifier never disrupts the checkpoint.
local function notify(title, body)
  -- entire.exec is only present-and-usable with the "exec" capability. Guard
  -- with pcall so an ungranted call (which raises) is a no-op.
  pcall(function()
    entire.exec.run("osascript", "-e",
      string.format('display notification %q with title %q', body, title))
  end)
end

entire.on("checkpoint_saved", function(ev)
  local n = bump_count()
  local files = 0
  if ev.modified_files then files = #ev.modified_files end
  entire.log.info(string.format("checkpoint #%d saved (%d files)", n, files))
  notify("Entire checkpoint", string.format("Checkpoint #%d saved", n))
end)

entire.on("post_commit", function(ev)
  if ev.has_checkpoint then
    entire.log.info("commit " .. (ev.commit or "?") .. " linked to a checkpoint")
  end
end)

entire.command{
  name = "notify-stats",
  short = "show how many checkpoints this plugin has seen",
  run = function()
    local n = entire.kv.get("checkpoints") or "0"
    entire.print("checkpoints seen: " .. n)
    return 0
  end,
}
