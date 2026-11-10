# MCP server

`jms mcp` runs a Model Context Protocol server over stdio, so an AI client can list assets, run commands and move files through the same pooled connections your shell uses.
The [README](../README.md) has the short version.
Siblings: [简体中文](mcp.zh-CN.md) · [Command reference](command-reference.md) · [Configuration](configuration.md) · [Addresses and proxies](networking.md) · [Troubleshooting](troubleshooting.md).

## What `jms mcp` is

One long-lived process, three jobs:

| Job | Detail |
|---|---|
| Serve MCP over stdio | stdout is the JSON-RPC channel. Every diagnostic goes to stderr. A log line on stdout would corrupt the stream. |
| Host the connection pool | Tool calls share one pool, so a second call for the same asset reuses the live session instead of logging in again. |
| Host the observation channel | `jms attach` clients watch the same event stream and can run commands on the same pooled connections. The IPC socket is embedded in this process. |

The process has **no per-call flags**. An MCP client starts it once and cannot pass arguments per call, so the endpoint, the transport backend and the exec ceiling are steered with environment variables instead.

```console
$ jms mcp --help
Usage:
  jms mcp [flags]

Flags:
  -h, --help           help for mcp
      --print-config   Print client configuration snippets (contains no credential)

Global Flags:
      --config string   Configuration file (overrides JMS_CONFIG; overridden by Deps.ConfigPath)
```

`jms mcp` takes no positional arguments (`cobra.NoArgs`).

### Exit codes

| Situation | Exit code |
|---|---|
| The client disconnects, or stdin reaches EOF | 0 |
| `--print-config` | 0 |
| Protocol failure, e.g. malformed JSON on stdin | 1, with `Error: <message>` on stderr |
| An unexpected argument, e.g. `jms mcp bogusarg` | 1, with `Error: unknown command "bogusarg" for "jms mcp"` |

Two warnings go to stderr and do **not** change the exit code: `Warning: audit log unavailable: …` (the audit directory cannot be written) and `Warning: observation channel unavailable: ipc: … is already in use` (another `jms mcp` or `jms attach` host owns the socket; the MCP server still serves).

## Setup

### The client entry

Every MCP client reads a server entry. `jms mcp --print-config` prints the plainest one to stdout and exits 0:

```json
{
  "mcpServers": {
    "jms": {
      "command": "jms",
      "args": ["mcp"]
    }
  }
}
```

It carries **no credential**: credentials live in the OS credential store, so the entry is safe to sync through a client manager.

To steer the process, add an `env` block, and use an absolute `command` path: a GUI client starts the server without your login shell, so `PATH` may not contain `~/.local/bin`. Check where yours lives with `command -v jms`.

```json
{
  "mcpServers": {
    "jms": {
      "command": "/home/ubuntu/.local/bin/jms",
      "args": ["mcp"],
      "env": {
        "JMS_MCP_ENDPOINT": "external",
        "JMS_MCP_BACKEND": "ws",
        "JMS_MCP_EXEC_TIMEOUT": "10m"
      }
    }
  }
}
```

### Claude Code

Claude Code stores MCP servers in `~/.claude.json` under `mcpServers`, and each entry accepts `command`, `args` and `env`. Its own CLI writes the same file:

```console
$ claude mcp add jms -- /home/ubuntu/.local/bin/jms mcp
$ claude mcp add --transport stdio jms -e JMS_MCP_BACKEND=ws -- /home/ubuntu/.local/bin/jms mcp
$ claude mcp add-json jms '{"command":"/home/ubuntu/.local/bin/jms","args":["mcp"]}'
$ claude mcp list
```

`claude mcp add` writes to the `local` scope by default; `--scope user` moves it to `~/.claude.json` for every project.

### Codex

Codex stores MCP servers in `~/.codex/config.toml` under `[mcp_servers.<name>]`:

```toml
[mcp_servers.jms]
command = "/home/ubuntu/.local/bin/jms"
args = ["mcp"]

[mcp_servers.jms.env]
JMS_MCP_BACKEND = "ws"
```

Its CLI writes the same file:

```console
$ codex mcp add jms --env JMS_MCP_BACKEND=ws -- /home/ubuntu/.local/bin/jms mcp
$ codex mcp list
```

### Claude Desktop, Cursor and other clients

Claude Desktop and Cursor both read the same `mcpServers` JSON block shown above, pasted into the client's own MCP server configuration. The key is `mcpServers` and the entry is `command` + `args`; the file each client reads is a client-side convention and differs between them. Keep the absolute command path here too, for the same `PATH` reason.

The protocol itself is standard: `initialize`, `tools/list`, `tools/call` over JSON-RPC on stdio. Any client that speaks stdio MCP can drive the server.

### The MCPB bundle

The release ships an MCPB bundle (`mcpb/manifest.json`). It is the same server in a portable package: its `mcp_config` points at a launcher instead of a bare binary, because one bundle has to cover six platforms.

```json
"mcp_config": {
  "command": "${__dirname}/launcher.sh",
  "args": ["mcp"],
  "env": {},
  "platform_overrides": {
    "win32": { "command": "${__dirname}/launcher.cmd", "args": ["mcp"] }
  }
}
```

`launcher.sh` detects the OS and CPU and `exec`s the matching binary, so the JSON-RPC channel is not broken by an intermediate shell.

## Steering the process

### Environment variables

| Variable | Accepted value | Effect | Default |
|---|---|---|---|
| `JMS_MCP_ENDPOINT` | `internal`, `external` | Forces the address kind for the whole process. | The configured preference plus the last-good address in `state.json` |
| `JMS_MCP_BACKEND` | `ssh`, `ws`, `auto` | Forces the transport backend for the whole process. | The backend remembered in `state.json` for the selected endpoint, else `ws` |
| `JMS_MCP_EXEC_TIMEOUT` | A Go duration (`90s`, `10m`, `1h30m`) or a positive number of seconds | The ceiling for a `jms_exec` call that omitted `timeout`. | `5m` |
| `JMS_CONFIG` | A file path | The configuration file the tools read. | `<UserConfigDir>/jms/config.toml` |
| `JMS_ACTOR` | Any non-blank string | The actor name stamped on audit events. | `mcp` |
| `JMS_PASSWORD_<ALIAS>` | The login secret | The credential for one server alias. Alias uppercased, every `-` becomes `_`: `JMS_PASSWORD_BASTION`. | unset → the OS credential store is read instead |
| `JMS_OTP_<ALIAS>` | The base32 TOTP secret | The MFA secret for one alias, same alias rule. | unset → the server rejects a login that needs MFA |

Values are trimmed and case-insensitive where the table says so. An invalid `JMS_MCP_ENDPOINT` or `JMS_MCP_EXEC_TIMEOUT` is **reported** as a tool error, not silently ignored:

```text
ERROR: JMS_MCP_ENDPOINT="bogus": must be internal or external
ERROR: JMS_MCP_EXEC_TIMEOUT="bogus": must be a duration ("90s", "10m") or a positive number of seconds
```

One exception: an invalid `JMS_MCP_BACKEND` is **silently ignored** and the remembered backend is used instead. The source comment claims the error is reported; the code discards it. Do not rely on a typo in that variable being caught.

### Which configuration file the tools read

| Source | Precedence |
|---|---|
| The tool's own `config_path` argument | highest |
| `JMS_CONFIG` | next |
| The platform default | lowest |

`jms mcp --config <path>` does **not** reach the MCP server. The flag is resolved on a separate path from the one the server is handed, so the server still loads the default configuration. Steer it with `JMS_CONFIG` or with the per-tool `config_path` argument.

## The tool surface

The tool list is decided when the server starts. The gates are read at registration time, so **restart the client** after changing one. Unsetting a variable restores the previous surface without touching anything on disk.

### Counts

| Gate combination | Registered tools | Write tools present |
|---|---|---|
| Defaults (no variable set) | **29** | none |
| `JMS_MCP_TOOLSET=core` | **15** | none |
| `JMS_MCP_WRITE=limited` | **31** | both |
| `JMS_MCP_WRITE=limited` + `JMS_MCP_WRITE_DOMAINS=node` | **30** | `jms_node_write` |
| `JMS_MCP_WRITE=limited` + `JMS_MCP_WRITE_DOMAINS=account` | **30** | `jms_account_secret` |
| `JMS_MCP_WRITE=limited` + `JMS_MCP_WRITE_DOMAINS=bogus` | **29** | none — an unknown domain matches nothing |
| `JMS_MCP_DEBUG=1` | **30** | none — adds `jms_pool_status` |
| `JMS_MCP_WRITE=limited` + `JMS_MCP_DEBUG=1` | **32** | both — the maximum surface |
| `JMS_MCP_TOOLSET=core` + `JMS_MCP_WRITE=limited` | **15** | none — **`core` wins over `limited`** |
| `JMS_MCP_WRITE=full` (or any value other than `limited`) | **29** | none — only the literal `limited` enables writes |

The arithmetic: 15 core + 14 management reads + 0/1/2 write tools + 0/1 debug tool.

### Gate reference

| Variable | Accepted value | Effect |
|---|---|---|
| `JMS_MCP_TOOLSET` | `core` (trimmed, case-insensitive) | Registers only the 15 core tools. Any other value, including empty, leaves the management block on. |
| `JMS_MCP_WRITE` | `limited` (trimmed, case-insensitive) | Registers the write tools at all. `full`, `true`, `1` do **not**. |
| `JMS_MCP_WRITE_DOMAINS` | Comma list of `node`, `account` | Narrows the write tools. Empty means both. An unknown value registers neither. Re-checked on **every call**, not only at startup. |
| `JMS_MCP_DEBUG` | `1`, `true`, `yes`, `on` (trimmed, case-insensitive) | Registers `jms_pool_status`. `0`, `off` and empty do not. |

Two rules worth remembering: the management reads are **on by default**, and the write tools are **off by default**. `core` beats `limited` — a `core` surface has no write tools even when `JMS_MCP_WRITE=limited` is also set.

## The 15 core tools

Every tool takes two optional arguments in addition to its own:

| Argument | Type | Meaning |
|---|---|---|
| `server` | string | Server alias. The default server when omitted. |
| `config_path` | string | Configuration file path override. |

| Tool | Required | Optional | What it does |
|---|---|---|---|
| `jms_ls` | — | `keyword` | Lists the assets you may connect to. One line per asset: `name  address  platform`. `No assets found.` when the list is empty. |
| `jms_resolve_asset` | `asset` | — | Resolves an asset to `name:`, `address:`, `account:`, `protocol:` — four lines. |
| `jms_exec` | `asset`, `command` | `timeout`, `account`, `protocol` | Runs a shell command on an asset and returns its output. |
| `jms_sftp_upload` | `src`, `asset`, `dst` | — | Uploads a local file. Result: `OK: uploaded <src> -> <asset>:<dst> (<bytes> bytes)`. |
| `jms_sftp_download` | `asset`, `src`, `dst` | — | Downloads a file. Result: `OK: downloaded <asset>:<src> -> <dst> (<bytes> bytes)`. |
| `jms_sftp_relay` | `src_spec`, `dst_spec` | — | Streams a file between two assets, no local disk. Result: `OK: relayed <src_spec> -> <dst_spec> (<bytes> bytes)`. |
| `jms_config_list` | — | — | Lists the configured servers, marking the default with `*`. Reads the file only and never touches the pool, so it works before any server is reachable. No credential appears in the output. |
| `jms_asset_list` | — | — | Lists **every** asset on the server, not only the ones you may connect to. |
| `jms_asset_show` | `target` | — | One asset in detail: `ID:`, `Name:`, `Address:`, `Platform:`, `Comment:`, `Nodes:`, `Accounts:`. Empty values render as `-`. |
| `jms_asset_create` | `name`, `address` | `account`, `username`, `password`, `comment`, `node_id`, `platform_id` | Creates a host asset. `platform_id` 0 means Linux. `username` defaults to `account`. Result: `Created <name> (id <id>, address <address>).` followed by `The asset is not connectable until it is granted to your account.` |
| `jms_asset_update` | `target` | `new_name`, `comment`, `address` | Updates the fields you set. Only non-empty fields are sent. Result: `Updated <name> (id <id>).` |
| `jms_asset_delete` | `target` | — | Deletes an asset. **Cannot be undone and has no `confirm` gate.** Result: `Deleted <name> (id <id>).` |
| `jms_asset_grant` | `target` | `name`, `users`, `user_groups`, `accounts`, `protocols`, `actions`, `comment` | Creates an `AssetPermission`. Defaults: `name` = `grant-<asset>`, `accounts` = `["@ALL"]`, `protocols` = `["all"]`, `actions` = `["connect"]`. With neither `users` nor `user_groups`, the authenticated user is the principal. Result: `Granted <asset> to <n> user(s) and <m> user group(s) via permission <name> (id <id>).` |
| `jms_asset_permissions` | — | `target` | Lists permissions, optionally for one asset. `No permissions found.` when there are none. |
| `jms_asset_revoke` | `target` | — | Removes a permission; the asset stays. `target` matches a permission **name or id**. Result: `Revoked <name> (id <id>).` |

Notes that change what you do:

- A **newly created asset is not reachable** until it is granted. `jms_asset_list` shows it, `jms_ls` does not.
- `jms_sftp_relay` takes `<asset>[@<server>]:<path>` specs, not separate arguments. The **first** colon splits host from path, so a colon inside the path survives; the **last** `@` inside the host splits the server alias.
- `jms_exec`'s `timeout` is in **seconds**. Omitted or `0` means `JMS_MCP_EXEC_TIMEOUT`, else 5 minutes.
- `jms_asset_revoke` fails with `ERROR: no permission matches "<target>"` when nothing matches exactly.

## The 14 management read tools

All of them take `server` and `config_path` as well, and all of them return the same JSON page shape.

| Tool | Required | Optional | What it reads |
|---|---|---|---|
| `jms_audit_list` | — | `kind`, `since`, `until`, `user`, `asset`, `action`, `limit`, `offset` | JumpServer audit logs. `kind` is `login` (default), `operate`, `password`, `ftp` or `job`. `since`/`until` are applied by the server; `user`/`asset`/`action` are matched on the fetched page. |
| `jms_session_list` | — | `since`, `until`, `user`, `asset`, `active_only`, `limit`, `offset` | Terminal sessions: who connected to which asset, when, and whether it is still open. |
| `jms_session_show` | `session_id` | — | One session: participants, timing, replay availability, whether it can be terminated. |
| `jms_command_list` | — | `since`, `until`, `user`, `asset`, `input`, `limit`, `offset` | Recorded commands from the session command logs. |
| `jms_node_tree` | — | — | The asset node tree. No `offset`/`limit`. |
| `jms_platform_list` | — | `keyword`, `limit` | Platform templates, and the `platform_id` used when creating an asset. No `offset`. |
| `jms_account_list` | — | `asset`, `username`, `privileged`, `limit`, `offset` | Asset accounts, **never their secrets**. The note always says `secret values are never returned`. |
| `jms_identity_list` | — | `kind`, `org`, `limit`, `offset` | `kind` is `users` (default), `groups`, `roles`, `orgs` or `org_members`. `org` narrows users and org members to one organization. |
| `jms_identity_show` | `id_or_name` | `kind` | One user, group, role or organization, by id or exact name. `kind` is `users`, `groups`, `roles` or `orgs`. |
| `jms_job_list` | — | `keyword`, `since`, `until`, `limit`, `offset` | Job definitions. A 403 usually means job execution is off on that instance, not that the path is wrong — `jms_audit_list` with `kind=job` still answers. |
| `jms_job_execution_list` | — | `keyword`, `since`, `until`, `limit`, `offset` | Job executions: what ran, when, whether it succeeded. |
| `jms_ticket_list` | — | `status`, `type`, `applicant`, `limit`, `offset` | Work orders (tickets) with status and applicant. |
| `jms_ticket_show` | `ticket` | — | One ticket by id. |
| `jms_local_audit` | — | `since`, `kind`, `asset`, `failed`, `limit` | The **local** audit stream on this machine — the same records `jms log show` prints. No server permission needed. `since` is a Go duration (`24h`, `30m`); `kind` is an event kind such as `exec.end`, `api.read`, `error`. |

Two of them fail with a short message rather than a page: `ERROR: session_id is required`, `ERROR: ticket is required`, `ERROR: id_or_name is required`.

`jms_local_audit` behaves differently from the others in two ways: its `limit` defaults to **50** and is capped at **500**, and an over-large limit falls back to 50 rather than to 500. Its `note` is `newest last` — the newest events are at the **end**.

## The 2 write tools

Off by default. Registered only when `JMS_MCP_WRITE=limited` and the domain is allowed.

| Tool | Domain | Required | Optional | What it does |
|---|---|---|---|---|
| `jms_node_write` | `node` | `op` | `name`, `parent_id`, `node`, `confirm` | `op` is `create`, `update` (alias `rename`) or `delete`. `create` needs `name`; `parent_id` empty means the root. `update` needs `name`. `delete` needs `node` and a `confirm` that repeats the node's current name or id. |
| `jms_account_secret` | `account` | `username`, `confirm` | `asset` | Rotates one account's secret. `confirm` must equal exactly `"<asset>/<username>"`. The new secret is generated server-side, never passed in and never returned. |

Both answer with a message line plus the pretty-printed JSON body:

```text
renamed node "old-name" to "new-name"
{
  "id": "…"
}
```

Both are refused **before** any request is made when a gate fails, and the refusal is recorded as `api.denied`:

```text
ERROR: jms_node_write: write domain "node" is not enabled
ERROR: jms_node_write: confirm does not match: confirm must repeat the node name "web-01"
ERROR: jms_account_secret: confirm must be "web-01/ops"
ERROR: jms_node_write: unknown op "bogus": use create | update | delete
```

Three separate gates protect a write: registration (`JMS_MCP_WRITE=limited`), the domain list (`JMS_MCP_WRITE_DOMAINS`), and the `confirm` argument. The domain list is re-checked on every call, so a host that started with a wider list still refuses a call a narrower configuration excludes.

## `jms_pool_status` (debug only)

Registered only when `JMS_MCP_DEBUG` is `1`, `true`, `yes` or `on`. It takes no arguments.

```text
terminals: 2
hits: 7
cold: 1
```

`hits` and `cold` are cumulative for the pool's lifetime.

## Result semantics

### Success and failure

| Case | Shape |
|---|---|
| Success | One text content block: `{"content":[{"type":"text","text":"…"}]}` |
| Tool failure | The same shape with `isError: true` and the text `ERROR: <message>`. The protocol-level error is **not** raised. |
| Unknown tool name | A JSON-RPC protocol error: `-32602 unknown tool "jms_x"`. Not a tool result. |
| Malformed arguments | `ERROR: invalid arguments: <json error>` |
| Missing required argument | Rejected by the SDK before the handler runs, because the schema declares `required`. |

The `ERROR: ` prefix is deliberate and matches the earlier Python implementation, so existing prompts keep working.

### The 32 KiB result cap

`maxResultBytes = 32 * 1024` = **32768 bytes**. It applies to the **14 management read tools and `jms_local_audit`**, because only those render through the page renderer. The 15 core tools return plain text and are not capped by it.

A truncated answer ends with exactly:

```text
… truncated at 32768 bytes; narrow the filters or lower limit
```

The cut is made on the serialized JSON, so a truncated body is **not** valid JSON. Narrow the filters or lower `limit` instead of parsing it.

### The page shape

```json
{
  "count": 30,
  "returned": 1,
  "items": [ … ],
  "note": "…"
}
```

`count` is the server's total for the query, `returned` is how many records are in this answer, `items` is the page, and `note` is optional — it explains a narrowed answer and is omitted when empty.

The notes that occur:

| Tool | Note |
|---|---|
| `jms_audit_list`, `jms_session_list`, `jms_command_list`, `jms_account_list`, `jms_ticket_list`, `jms_platform_list` | `filter applied to the 100 most recent records only, not to the whole collection` |
| `jms_command_list` (when the server reports nothing) | also `the server reports no command records; command logging may be off on this instance` |
| `jms_account_list` | also `secret values are never returned` |
| `jms_local_audit` | `newest last` |

That window note exists because `user`, `asset`, `action` and `input` are matched **locally on the fetched page**, not on the server. When such a filter is present the tool widens the request to 100 records and then filters them, so a filtered answer is a filtered *window*, not a filtered collection.

### Paging

The default page size is 20 and the maximum is 100. A `limit` of 0 or less falls back to 20; a `limit` over 100 is capped at 100. `jms_local_audit` defaults to **50** and caps at **500** — but an over-large limit falls back to 50, not 500. Use `offset` to page; `jms_node_tree`, `jms_platform_list` and `jms_local_audit` have no `offset`.

### The exit code of a remote command is not surfaced

`jms_exec` returns the command's **output text only**. The remote exit code is not part of the tool result; it is recorded in the audit event and nothing else. A non-zero exit code on the remote host is not an error at the transport level either.

If you need the exit code, ask for it in the command itself:

```sh
command; echo "exit=$?"
```

### One reconnect attempt, never a replay

Two independent layers enforce the rule that a command is not sent twice.

| Layer | Rule |
|---|---|
| MCP | `jms_exec` may run **twice** in total: the first attempt plus **one** recovery attempt through a fresh login. The retry happens only when the first attempt failed, the failure is recoverable, the first attempt produced **no output**, and the re-selected session or backend is actually **different** from the failed one. |
| Pool | The terminal pool evicts and rebuilds a terminal and replays a command **once**, and only when the failure happened before any output arrived. |

Not retried, because the command already reached the remote host and `rm` or `reboot` are not idempotent: a client cancellation, a context deadline, an unsupported backend, a command timeout, and any authentication rejection (HTTP 400, 401, 403, 422). A *connect* timeout stays retryable — it never delivered anything.

File transfers follow the same rule: the retry happens only when **no bytes moved** and the verification did not fail. A relay refreshes both sides, because the caller cannot tell which side was stale.

## Worked examples

The examples below are the JSON-RPC that crosses stdio. `tools/call` takes the tool name and its arguments.

### List the assets you may reach

```json
{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"jms_ls","arguments":{}}}
```

```text
web-01  10.20.0.44  Linux
web-02  10.20.0.45  Linux
```

Add a `keyword` to search instead of listing: `{"name":"jms_ls","arguments":{"keyword":"web"}}`.

### Run a command on an asset

```json
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"jms_exec","arguments":{"asset":"web-01","command":"uptime"}}}
```

```text
 14:22:31 up 12 days,  3:07,  2 users,  load average: 0.21, 0.18, 0.15
```

Target another server by embedding the alias, and bound the call with `timeout` (seconds): `{"asset":"web-01@bastion","command":"df -h /","timeout":30}`.

### Upload a file

```json
{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"jms_sftp_upload","arguments":{"src":"/home/ubuntu/app.log","asset":"web-01","dst":"/var/log/app.log"}}}
```

```text
OK: uploaded /home/ubuntu/app.log -> web-01:/var/log/app.log (2048 bytes)
```

The md5 comparison is always on for the upload and download tools.

### Grant a group access

```json
{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"jms_asset_grant","arguments":{"target":"web-01","user_groups":["ops"],"accounts":["ssh"],"protocols":["ssh"],"actions":["connect"]}}}
```

```text
Granted web-01 to 0 user(s) and 1 user group(s) via permission grant-web-01 (id 8c1d0f22-…)
```

Leave `users` and `user_groups` out and the authenticated user becomes the principal.

### Inspect a session

List first, then fetch one by id: `jms_session_list` with `{"active_only":true,"limit":10}`, then `jms_session_show` with `{"session_id":"4d0e7a2c-…"}`.

```json
{
  "count": 1,
  "returned": 1,
  "items": [
    { "id": "4d0e7a2c-…", "user": "alice", "asset": "web-01", "date_start": "2026-11-10 14:02:11" }
  ]
}
```

### Rotate an account secret (write tools enabled)

```json
{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"jms_account_secret","arguments":{"asset":"web-01","username":"ops","confirm":"web-01/ops"}}}
```

```text
requested a secret rotation for ops on web-01
{
  "account_id": "…",
  "asset": "web-01",
  "username": "ops"
}
```

The new secret never appears in the result.

## Security notes

### No generic path tool

There is deliberately **no** management tool that takes a `path`, `url` or `raw_body` argument, not even a read-only one. The caller of an MCP tool is a model, so a `path` argument would put "read anything the token can see" one sentence away, and the boundary that matters — which fields are secrets, whose session log this is — cannot be expressed as a path prefix. Every management tool is a verb with a fixed shape instead. A test enforces this: it scans every registered management schema for those four names.

### The write gates

Three gates, in order: registration, domain list, `confirm`. A tool that is not registered does not appear in `tools/list` at all, so a model cannot be talked into calling it. `JMS_MCP_WRITE` must be exactly `limited`; `JMS_MCP_WRITE_DOMAINS` narrows it; the destructive operations need a `confirm` that repeats the target's name.

### `jms_asset_delete` has no confirm gate

`jms_asset_delete` is destructive and irreversible, and it asks for no `confirm` argument. It lives in the core block, so it is registered even on a `core` surface. If that is not what you want, keep the MCP surface away from a client that can act unattended.

### What is audited

Every call is written to the local JSONL audit stream, the same one `jms log show` prints.

| Event kind | Emitted by |
|---|---|
| `exec.start` | Every exec, SFTP, asset and grant call that starts |
| `exec.end` | The matching end event, with `duration_ms`, `output_bytes` and a truncated `preview` |
| `error` | A call that failed, including one that never started |
| `api.read` | The 14 management read tools |
| `api.write` | The 2 write tools, on a server-side outcome |
| `api.denied` | A write call refused locally: wrong domain, wrong `confirm` |

The audit preview is capped at **4096** bytes. That cap is independent of the 32 KiB result cap, and `JMS_AUDIT_FULL=1` cannot restore text the MCP layer already dropped.

A failure that happens before the session exists is still recorded, so an operator can tell a broken configuration from a server that is down. The reason string never carries a credential value — only a variable name.

The audit actor is `mcp` by default; `JMS_ACTOR` overrides it, which is how several clients on one machine stay distinguishable in a shared audit file.

### Credential handling

- The credential never goes into the configuration file. It lives in the OS credential store (Windows Credential Manager, macOS Keychain, Linux Secret Service) under the account name `<alias>/password`, or in `JMS_PASSWORD_<ALIAS>`.
- The environment variable wins over the credential store. That is the way to run on a headless machine, a CI runner or WSL.
- `jms_config_list` renders addresses and usernames only. A credential never appears in it.
- `jms_account_list` never returns secret values, and says so in its note.
- The new secret from `jms_account_secret` is generated server-side. It is never passed in and never returned.
- Error messages name the alias and the environment variable to set, never a credential value.
- The MCP server has no terminal, so a missing TOTP secret fails with an actionable message instead of prompting.

## Limitations

- How a real client renders a tool result with `isError: true` was **not** exercised here: no MCP client was available in this environment. The wire shape is confirmed; whether a client shows the text or aborts the turn is not.
- The exact truncation behaviour at the 32 KiB boundary when the JSON contains multi-byte UTF-8 was **not** measured against a live server. The renderer cuts the byte slice at 32768, which can split a UTF-8 sequence.
- The MCPB bundle's launcher was read but not executed. Only the manifest's `mcp_config` block was verified.
- The write tools and the destructive `jms_asset_delete` were **not** exercised against a live JumpServer. Their registration, schemas, gating and refusal paths were; no write was sent to any server.
- The file names and locations that Claude Desktop and Cursor read were **not** verified in this worktree, because neither client was installed here. The `mcpServers` JSON shape is what `jms mcp --print-config` prints.
- An invalid `JMS_MCP_BACKEND` is silently ignored, even though the source comment says it is reported.
- `jms mcp --config <path>` does not reach the MCP server. Use `JMS_CONFIG` or the per-tool `config_path` argument.
- The `jms_asset_grant` description says "Give at least one of users or user_groups", but the tool grants to the authenticated user when both are omitted. The behaviour is what the code does.
- The tool count is only meaningful together with the gate combination that produced it: 29 by default, 15 on a `core` surface, 32 at the maximum.
