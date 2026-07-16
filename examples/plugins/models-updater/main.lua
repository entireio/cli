-- models-updater: a real pricing-model updater plugin.
--
-- Embedded model-pricing tables rot silently: rates change upstream and the
-- copy shipped in a repo drifts out of date without anyone noticing. This
-- plugin keeps `.entire/models.json` fresh and makes drift visible.
--
--   * `entire models-update`          fetch upstream, diff against the cached
--                                     copy, report per-model rate drift, write
--                                     the merged result, remember the refresh.
--   * `entire models-update --check`  fetch + report drift only; exit non-zero
--                                     when anything drifted (CI-friendly).
--   * `entire models-update <url>`    override the upstream source URL.
--   * a `session_start` hook          gently nudges (once per session, only when
--                                     stale) so the refresh is never forgotten.
--
-- It demonstrates the whole plugin surface working together: a command
-- (entire.command) + an observer hook (entire.on) + the http and fs
-- capabilities + durable kv state + capability-gating (an ungranted call fails
-- loudly).
--
-- Enable it (installing alone does NOT run it):
--   * user-installed  -> .entire/settings.json or .entire/settings.local.json
--   * repo-local       -> .entire/settings.local.json only (never committed)
--   "plugins": { "models-updater": { "enabled": true, "capabilities": ["http", "fs"] } }

local DEFAULT_URL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
local DEST = ".entire/models.json"

-- Staleness is measured in *sessions since the last refresh*, not days: the
-- plugin sandbox opens only base/string/table/math, so there is no wall clock
-- (no `os` library, and event payloads carry no timestamp). A durable logical
-- session counter is the honest, achievable staleness signal with only kv.
-- Nudge once this many sessions have elapsed without a refresh.
local STALE_AFTER_SESSIONS = 25

-- Durable kv keys (survive across sessions; stored under the plugin data dir).
local KV_SESSION_COUNT = "session_count" -- logical clock, bumped each session_start
local KV_LAST_UPDATED = "last_updated_session" -- session_count value at the last write

-- The LiteLLM file carries a non-model "sample_spec" schema entry; skip it so
-- counts and drift stay about real models.
local NOT_A_MODEL = { sample_spec = true }

-- ---------------------------------------------------------------------------
-- Tiny JSON reader.
--
-- The sandbox exposes no JSON library, so the plugin ships just enough to walk
-- the LiteLLM shape: a flat object whose values are per-model objects. We keep
-- each model's value as its verbatim JSON text so the merged file we write back
-- preserves upstream bytes exactly (a minimal diff), and pull the two headline
-- rate fields out with a pattern. This is not a general JSON parser; it assumes
-- the well-known object-of-objects shape.
-- ---------------------------------------------------------------------------

-- split_top_level returns an ordered array of { key = <name>, entry = <verbatim
-- "name": value text> } for each member of the JSON object in `text`. It is
-- string-aware: braces, brackets and commas inside string values (and escaped
-- quotes) never confuse the member boundaries.
local function split_top_level(text)
  local raw_entries = {}
  local n = #text
  local i = 1
  while i <= n and text:sub(i, i) ~= "{" do
    i = i + 1
  end
  i = i + 1 -- step past the root "{"

  local depth = 0 -- nesting depth *inside* the root object
  local in_str, esc = false, false
  local start = nil -- index of the first char of the current member

  local function flush(stop)
    if start then
      raw_entries[#raw_entries + 1] = text:sub(start, stop)
      start = nil
    end
  end

  while i <= n do
    local c = text:sub(i, i)
    if in_str then
      if esc then
        esc = false
      elseif c == "\\" then
        esc = true
      elseif c == '"' then
        in_str = false
      end
    elseif c == '"' then
      in_str = true
      if not start then
        start = i
      end
    elseif c == "{" or c == "[" then
      if not start then
        start = i
      end
      depth = depth + 1
    elseif c == "}" or c == "]" then
      if depth == 0 then
        flush(i - 1) -- closing the root object ends the final member
        break
      end
      depth = depth - 1
    elseif c == "," and depth == 0 then
      flush(i - 1)
    elseif not start and c ~= " " and c ~= "\t" and c ~= "\n" and c ~= "\r" then
      start = i
    end
    i = i + 1
  end

  local out = {}
  for _, raw in ipairs(raw_entries) do
    local entry = raw:gsub("%s+$", "")
    local key = entry:match('^%s*"(.-)"%s*:') -- first quoted token, before its colon
    if key then
      out[#out + 1] = { key = key, entry = entry }
    end
  end
  return out
end

-- parse_number turns a JSON numeric literal into a Lua number. The sandbox's
-- tonumber accepts plain decimals/integers but rejects exponent notation
-- ("3e-06" -> nil), which the LiteLLM file uses, so parse the mantissa and
-- exponent by hand and recombine.
local function parse_number(s)
  if not s then
    return nil
  end
  local n = tonumber(s)
  if n then
    return n -- plain decimal or integer
  end
  local mant, esign, edigits = s:match("^(%-?[%d%.]+)[eE]([%-%+]?)(%d+)$")
  if not mant then
    return nil
  end
  local m, e = tonumber(mant), tonumber(edigits)
  if not m or not e then
    return nil
  end
  if esign == "-" then
    e = -e
  end
  return m * 10 ^ e
end

-- number_field pulls a numeric field out of a model's raw JSON text (decimal or
-- scientific notation). Returns nil when the field is absent.
local function number_field(entry, field)
  return parse_number(entry:match('"' .. field .. '"%s*:%s*([%-%+%d%.eE]+)'))
end

-- rates_differ compares two rates with a small relative tolerance, so that the
-- same value written in different notations (0.000003 vs 3e-06) never registers
-- as drift, while genuine rate changes still do.
local function rates_differ(a, b)
  if a == nil or b == nil then
    return a ~= b
  end
  local diff = math.abs(a - b)
  local scale = math.max(math.abs(a), math.abs(b))
  return diff > scale * 1e-9
end

-- parse_models turns a models.json body into a name -> {entry, input, output}
-- map plus the ordered list of model names (upstream order, sans sample_spec).
local function parse_models(body)
  local by_name, order = {}, {}
  for _, e in ipairs(split_top_level(body)) do
    if not NOT_A_MODEL[e.key] then
      by_name[e.key] = {
        entry = e.entry,
        input = number_field(e.entry, "input_cost_per_token"),
        output = number_field(e.entry, "output_cost_per_token"),
      }
      order[#order + 1] = e.key
    end
  end
  return by_name, order
end

-- ---------------------------------------------------------------------------
-- Diff / merge helpers.
-- ---------------------------------------------------------------------------

local function fmt_rate(v)
  if v == nil then
    return "(none)"
  end
  return string.format("%.10g", v)
end

-- rate_drift lists the per-model input/output rate changes between the cached
-- and freshly fetched tables, for models present in both.
local function rate_drift(old, new)
  local lines = {}
  for name, nv in pairs(new) do
    local ov = old[name]
    if ov then
      if rates_differ(ov.input, nv.input) then
        lines[#lines + 1] = string.format("%s  input %s -> %s", name, fmt_rate(ov.input), fmt_rate(nv.input))
      end
      if rates_differ(ov.output, nv.output) then
        lines[#lines + 1] = string.format("%s  output %s -> %s", name, fmt_rate(ov.output), fmt_rate(nv.output))
      end
    end
  end
  table.sort(lines)
  return lines
end

-- local_only_models lists model ids present in the cached copy but absent
-- upstream: our own newer/internal ids that must be preserved, never erased.
local function local_only_models(old, new)
  local names = {}
  for name in pairs(old) do
    if not new[name] then
      names[#names + 1] = name
    end
  end
  table.sort(names)
  return names
end

-- splice_local_only reinserts the preserved (local-only) models verbatim just
-- before the closing brace of the freshly fetched upstream body, so upstream
-- bytes are otherwise untouched and the local ids survive the refresh.
local function splice_local_only(upstream, old, keep_names)
  local close = upstream:find("}%s*$")
  if not close then
    return upstream -- not a shape we recognize; leave it alone
  end
  local head = upstream:sub(1, close - 1):gsub("%s+$", "")
  local tail = upstream:sub(close)
  local blocks = {}
  for _, name in ipairs(keep_names) do
    blocks[#blocks + 1] = old[name].entry
  end
  local sep = head:match("{%s*$") and "\n  " or ",\n  " -- no comma if upstream was empty
  return head .. sep .. table.concat(blocks, ",\n  ") .. "\n" .. tail
end

-- read_file returns the file contents or nil when it does not exist.
-- entire.fs.read raises on a missing file, so guard it with pcall.
local function read_file(path)
  local ok, data = pcall(function()
    return entire.fs.read(path) -- requires the "fs" capability
  end)
  if ok then
    return data
  end
  return nil
end

local function report_diff(drifts, kept)
  if #drifts == 0 then
    entire.print("No rate drift versus the cached copy.")
  else
    entire.print(string.format("Rate drift detected (%d change(s)):", #drifts))
    for _, line in ipairs(drifts) do
      entire.print("  " .. line)
    end
  end
  if #kept > 0 then
    entire.print(string.format("Keeping %d local-only model id(s) (manually maintained, absent upstream):", #kept))
    for _, name in ipairs(kept) do
      entire.print("  - " .. name)
    end
  end
end

-- ---------------------------------------------------------------------------
-- Command: entire models-update [--check] [--write] [url]
-- ---------------------------------------------------------------------------

local function run(args)
  local url = DEFAULT_URL
  local check = false
  for _, a in ipairs(args) do
    if a == "--check" then
      check = true
    elseif a == "--write" then
      check = false -- explicit default; accepted for symmetry with --check
    elseif a:sub(1, 2) == "--" then
      entire.print("models-update: unknown flag " .. a)
      return 2
    else
      url = a
    end
  end

  entire.print("Fetching " .. url .. " ...")
  local resp = entire.http.get(url) -- requires the "http" capability
  if resp.status ~= 200 then
    entire.print(string.format("error: server returned HTTP %d", resp.status))
    return 1
  end

  local new_by_name, new_order = parse_models(resp.body)
  local total = #new_order

  local existing = read_file(DEST)
  local drifts, kept, old_by_name = {}, {}, nil
  if existing then
    old_by_name = parse_models(existing)
    drifts = rate_drift(old_by_name, new_by_name)
    kept = local_only_models(old_by_name, new_by_name)
    report_diff(drifts, kept)
  else
    entire.print("No cached " .. DEST .. " yet — this will be the first copy.")
  end

  -- --check: report only, never write; non-zero exit signals drift to CI.
  if check then
    entire.print(string.format("Checked %d upstream models; %d rate change(s). (--check: nothing written)", total, #drifts))
    if #drifts > 0 then
      return 1
    end
    return 0
  end

  local merged = resp.body
  if old_by_name and #kept > 0 then
    merged = splice_local_only(resp.body, old_by_name, kept)
  end
  entire.fs.write(DEST, merged) -- requires the "fs" capability; confined to the repo

  -- Remember when we refreshed, on the logical session clock (see the note by
  -- STALE_AFTER_SESSIONS for why this is not a wall-clock timestamp).
  local marker = entire.kv.get(KV_SESSION_COUNT) or "0"
  entire.kv.set(KV_LAST_UPDATED, marker)

  entire.log.info(string.format("models.json refreshed from %s (%d models, %d drifted, %d preserved)", url, total, #drifts, #kept))
  entire.print(string.format(
    "Wrote %s — %d models, %d rate change(s), %d local-only preserved (refreshed at session #%s).",
    DEST, total, #drifts, #kept, marker
  ))
  return 0
end

entire.command{
  name = "models-update",
  short = "refresh .entire/models.json and report pricing drift",
  run = run,
}

-- ---------------------------------------------------------------------------
-- Hook: session_start — the gentle "don't let it rot" nudge.
--
-- Fires once per session. We bump the logical session clock silently and only
-- emit a single log line when the cache looks stale, so the hook stays quiet on
-- the common (fresh) path. We log rather than print: hook stdout can be consumed
-- by the host agent, and printing there could corrupt it.
-- ---------------------------------------------------------------------------

entire.on("session_start", function()
  local count = (tonumber(entire.kv.get(KV_SESSION_COUNT) or "0") or 0) + 1
  entire.kv.set(KV_SESSION_COUNT, tostring(count))

  local updated = entire.kv.get(KV_LAST_UPDATED)
  if updated == nil then
    entire.log.warn("Entire pricing data has never been fetched here — run `entire models-update`")
    return
  end

  local age = count - (tonumber(updated) or 0)
  if age >= STALE_AFTER_SESSIONS then
    entire.log.warn(string.format(
      "Entire pricing data was last refreshed %d sessions ago — run `entire models-update`", age))
  end
end)
