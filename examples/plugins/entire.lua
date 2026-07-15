--- entire.lua — type-annotation stub for the Entire plugin API.
---
--- This file is NOT loaded at runtime. It exists so editors with an EmmyLua /
--- lua-language-server setup give you autocompletion and type hints while
--- authoring an Entire Lua plugin. Point your workspace library at the
--- directory containing this file, e.g. in .luarc.json:
---
---   { "workspace": { "library": ["path/to/examples/plugins"] } }
---
--- The real `entire` table is injected by the host at load time.

---@meta

---@class EntireEvent
---@field event string            # hook name, e.g. "turn_end"
---@field agent string|nil        # agent name
---@field session_id string|nil
---@field session_ref string|nil  # transcript path
---@field model string|nil
---@field modified_files string[]|nil
---@field new_files string[]|nil
---@field deleted_files string[]|nil
---@field subagent_type string|nil
---@field source string|nil       # prepare_commit_msg: "message" | "template" | ...
---@field commit string|nil       # post_commit: commit SHA
---@field checkpoint_id string|nil
---@field has_checkpoint boolean|nil
---@field remote string|nil       # pre_push
---@field push_target string|nil

---@class EntireHttpResponse
---@field status integer
---@field body string

---@class EntireExecResult
---@field stdout string
---@field stderr string
---@field code integer

---@class EntireCommandSpec
---@field name string
---@field short string|nil
---@field run fun(args: string[]): integer|nil

---@class EntireLog
---@field debug fun(msg: string)
---@field info fun(msg: string)
---@field warn fun(msg: string)
---@field error fun(msg: string)

---@class EntireKV
---@field get fun(key: string): string|nil
---@field set fun(key: string, value: string)
---@field delete fun(key: string)

---@class EntireHttp
---@field get fun(url: string): EntireHttpResponse
---@field post fun(url: string, body: string, content_type: string|nil): EntireHttpResponse

---@class EntireExec
---@field run fun(cmd: string, ...: string): EntireExecResult

---@class EntireFs
---@field read fun(path: string): string
---@field write fun(path: string, contents: string)

---@class Entire
---@field plugin_name string
---@field version string
---@field source string           # "user" | "repo"
---@field repo_root string
---@field data_dir string
---@field log EntireLog
---@field kv EntireKV
---@field http EntireHttp         # requires "http" capability
---@field exec EntireExec         # requires "exec" capability
---@field fs EntireFs             # requires "fs" capability
---@field on fun(hook: string, cb: fun(event: EntireEvent): any)
---@field command fun(spec: EntireCommandSpec)
---@field print fun(...: any)     # stdout (use in commands, not hooks)
---@field write fun(...: any)     # stdout, no newline
entire = {}

return entire
