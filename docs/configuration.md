# Configuration

Where the configuration, the credentials, the endpoint memory and the audit log live, and how each one is read.
The [README](../README.md) has the short version.
Siblings: [简体中文](configuration.zh-CN.md) · [Command reference](command-reference.md) · [MCP server](mcp.md) · [Addresses and proxies](networking.md) · [Troubleshooting](troubleshooting.md).

## Where the files live

The configuration directory holds four things:

```
<config dir>/
  config.toml                  0600  metadata only — no credentials
  state.json                   0600  last-good address and remembered backend
  audit/                       0700
    audit-YYYYMMDD-<pid>.jsonl 0600  one file per process, JSONL
    .prune-stamp               0600  RFC3339 UTC, throttles automatic pruning
  jms-<hash>.sock              0600  Unix IPC socket for `jms attach`
```

`<config dir>` is the directory that holds `config.toml`, `state.json` and `audit/`. With `JMS_CONFIG` set it is `dirname(JMS_CONFIG)`; otherwise it is the platform config root plus `jms`.

| OS | Platform config root | Example config dir | Example file |
|---|---|---|---|
| Linux / other Unix | `$XDG_CONFIG_HOME`, else `$HOME/.config` | `~/.config/jms` | `~/.config/jms/config.toml` |
| macOS | `$HOME/Library/Application Support` | `~/Library/Application Support/jms` | `~/Library/Application Support/jms/config.toml` |
| Windows | `%AppData%` (Roaming) | `C:\Users\<u>\AppData\Roaming\jms` | `C:\Users\<u>\AppData\Roaming\jms\config.toml` |

### Which file is read, and what moves with it

| Rank | Source | Notes |
|---|---|---|
| 1 | `--config <path>` | persistent root flag, on every command |
| 2 | `JMS_CONFIG=<path>` | any non-blank value, used verbatim |
| 3 | platform default | `<platform config root>/jms/config.toml` |

A path set programmatically by the embedder (not reachable from the command line) outranks all three. Like `JMS_CONFIG`, it relocates the whole directory; unlike `--config`, it also moves `state.json` and `audit/` for the CLI process.

`JMS_CONFIG` relocates the whole directory: `config.toml`, `state.json` and `audit/` all follow it.

`--config` relocates **only** `config.toml`. `state.json` and `audit/` are resolved from the config directory, not from the flag. That difference bites when you point `--config` at a file in a scratch directory and then look for the audit log next to it — the log is not there.

```console
$ JMS_CONFIG=/tmp/jms/config.toml jms config list
$ jms --config /tmp/jms/config.toml config list    # reads the same file
```

The MCP server follows `JMS_CONFIG`, not `--config`: `jms --config other.toml mcp` still loads the default configuration, because the flag is not copied into the MCP server's config path.

### File and directory modes

Every file above is written `0600`, in a directory that is `0700`. A directory is created `0700` when it is missing; the audit directory is additionally re-`chmod`ed to `0700` on every write, so a directory left loose by an earlier run is tightened. The files are replaced rather than edited in place, so a world-readable `config.toml` from an earlier run ends up `0600` after the next save. The config file is written atomically: a temp file in the same directory, `fsync`, then `rename`.

On Windows the mode has no effect; the ACLs govern.

## The configuration file

The file is TOML. The schema is version 2.

### Schema

Root keys:

| Key | Type | Default | Notes |
|---|---|---|---|
| `version` | float | `2` | written as an integer literal. `0` or absent is read as `2` |
| `default_server` | string | `""` | alias of the default server. Must name an existing server |
| `proxy` | string | `""` | global proxy. Empty means direct |
| `audit_keep_days` | int | absent | `0` or absent means "never prune". Range `0`–`3650` |
| `[servers.<alias>]` | table | — | at least one server is required |

Keys inside `[servers.<alias>]`:

| Key | Type | Default | Notes |
|---|---|---|---|
| `internal` | string | `""` | absolute `http://` or `https://` URL with a host |
| `external` | string | `""` | same |
| `username` | string | — | required, non-empty after trimming |
| `prefer` | string | `""` | `internal` or `external`; empty means internal |
| `pin` | string | `""` | `internal` or `external`; restricts the server to one address, no failover |
| `proxy` | string | `""` | per-server override. `direct`, `none`, `off` force a direct connection |
| `ssh_port` | int | `0` | KoKo SSH port. `0` means `2222` |

At least one of `internal` and `external` must be set.

### An annotated example

```toml
version = 2                      # always written first
default_server = "bastion"        # the alias used when a target names no server
proxy = "http://proxy:3128"       # global proxy; empty means direct
audit_keep_days = 90              # prune audit files older than 90 days

[servers.bastion]
internal = "https://bastion.internal.example.com"   # reachable from the office network
external = "https://bastion.example.com"            # reachable from anywhere
username = "operator"
prefer = "external"               # try external first; empty means internal
pin = ""                          # empty: probe both, fail over
proxy = ""                        # empty: inherit the global proxy
ssh_port = 2222                   # 0 would mean 2222 as well
```

Servers are written in sorted alias order, so the file diffs cleanly. `default_server` and `username` are always written; the other keys are omitted when empty.

### Validation

Every rule below is checked on load and again before every save. A rejected entry never truncates the existing file.

| Rule | Error |
|---|---|
| alias not empty | `server alias must not be empty` |
| username present | `server %q: username is required` |
| one of internal/external | `server %q: at least one of internal/external URL is required` |
| absolute http(s) URL with a host | `internal URL %q must use http:// or https://` / `internal URL %q is missing a host` |
| `prefer` value | `server %q: prefer must be "internal" or "external", got %q` |
| `pin` value | `server %q: pin must be "internal" or "external", got %q` |
| `ssh_port` range | `server %q: ssh_port must be between 1 and 65535, got %d` |
| `proxy` grammar | `server %q: proxy %q ...` |
| `audit_keep_days` range | `invalid config %s: audit_keep_days must be between 0 and 3650, got %d` |
| at least one server | `no servers configured: <path> contains no servers — run \`jms config add <alias>\`` |
| default exists | `invalid config %s: default_server %q does not exist (available: %s)` |
| file not empty | `config file is empty: <path>` |
| valid TOML | `parse config %s: ...` |

The proxy grammar accepts an empty value, `direct` / `none` / `off` (case-insensitive), and the schemes `http`, `https`, `socks5`, `socks5h` with a host. A value with no scheme gives `proxy %q is missing a scheme (use http:// or socks5://)`; an unknown scheme gives `proxy %q has unsupported scheme %q (use http, https or socks5)`. A bare `host:port` counts as a scheme, so it is reported as unsupported rather than missing.

`HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` are never read. A proxy has to be configured explicitly.

The sentinel errors the loader and the credential store raise: `ErrNotFound` (`credential not found`), `ErrReadOnly` (`credential store is read-only`), `ErrConfigNotFound` (`config file not found`), `ErrNoServers` (`no servers configured`), `ErrServerNotFound` (`server not found`), `ErrLegacyConfig` (`detected legacy jms-cli config`), `ErrCredentialStoreLocked` (`credential store is locked`).

### Version handling

`version` is stored as written. The loader does not branch on the number: `version = 1`, `2`, `3` and `0` all load, and a file written with `version = 1` is saved back with `version = 1`. Only `0` and absent are normalised to `2`. **There is no v1-to-v2 schema migration in this code.**

The one migration refusal is the legacy Python `jms-cli` `config.yaml`. When `config.toml` is missing but a `config.yaml` sits next to it, the load fails with `detected legacy jms-cli config — unsupported by design; re-add with jms config add (found <path>)`. A *directory* named `config.yaml` does not trigger it.

### Commands that write the file

| Command | Effect on the file |
|---|---|
| `jms config add <alias>` | prompts for both addresses, the username, the Password (hidden, confirmed) and an optional TOTP secret. Validates the credential against a reachable address first, then stores it in the OS credential store, then writes the metadata file. A failed write rolls the credential back. `--set-default` also switches the default server |
| `jms config remove <alias>` | writes the metadata file, then deletes the credentials. Removing the last server deletes the file outright, because `Save` refuses to write a serverless configuration |
| `jms config set-default <alias>` | switches the default server |
| `jms config list` | read-only. The `Creds` column says where the credential comes from: `env`, `keyring`, `missing` or `locked` |

The credential never reaches `config.toml`.

## Credentials

### Where they live

| Order | Backend | Read | Write |
|---|---|---|---|
| 1 | environment variables | yes | no — returns `ErrReadOnly` |
| 2 | OS credential store | yes | yes |

**The environment wins over the OS store.** `jms config list` reports `env` for an alias whose variable is set, even when the keyring also holds an entry.

The OS store is the Secret Service (`org.freedesktop.secrets` over D-Bus) on Linux, the Keychain on macOS and the Windows Credential Manager on Windows.

Service name: `jms-client`. Account name: `<alias>/password` for the login secret and `<alias>/otp_secret` for the TOTP secret. The credential kinds are `password` and `otp`; an unknown kind is refused, so a typo cannot overwrite the login-secret entry.

### The environment variable rule

`JMS_PASSWORD_<ALIAS>` holds the login secret, `JMS_OTP_<ALIAS>` the TOTP secret. The alias is uppercased and every `-` becomes `_`; no other character is rewritten. So `bastion` reads `JMS_PASSWORD_BASTION`, `my-wan` reads `JMS_PASSWORD_MY_WAN`, `web-01` reads `JMS_PASSWORD_WEB_01`.

An exported-but-empty variable, or one holding only whitespace, counts as **absent**, not as an empty credential. Sending an empty credential to the server would be worse than reporting none.

### A locked store

A locked store is not a missing credential: the entry exists but cannot be read unattended. On Linux the lock state is probed before the read, so a headless machine fails in milliseconds instead of blocking on a prompt nobody can answer. Every call to the store is bounded — 5 s per D-Bus round trip on Linux, 10 s for the whole go-keyring call elsewhere.

The read message is `the credential store is locked — unlock the keyring, or set JMS_PASSWORD_<ALIAS>`. The delete message ends with `unlock the keyring before removing credentials`. On Linux the write message ends with `unlock the keyring before storing credentials`; on a desktop session it says `unlock the keyring from the desktop session first` instead.

Whether a desktop session exists is decided on Linux by `DISPLAY`, `WAYLAND_DISPLAY` or `XDG_SESSION_TYPE`. It only changes how the message is phrased, never whether the store is tried.

### What is never stored, never logged

- The configuration file has no credential field: the schema has nowhere to put one, so a rendered file cannot carry one. A package guard rejects any rendered file that declares a credential field or carries an encrypted-credential marker.
- Error and hint strings name the environment variable, never a value.
- Audit query parameters are masked: a parameter whose name contains `password`, `passwd`, `secret`, `token`, `key`, `passphrase` or `otp` is replaced with `***`; other values are truncated to 128 bytes.

## `state.json`

Location: `<config dir>/state.json`. It follows `JMS_CONFIG` and is **not** affected by `--config`.

```json
{
  "servers": {
    "bastion": {
      "last_good": "internal",
      "at": "2026-11-10T14:47:06.00579509+08:00",
      "backend": { "internal": "ssh", "external": "ws" }
    }
  }
}
```

| Field | Meaning | Lifetime |
|---|---|---|
| `last_good` | the address kind that last worked. Tried first on the next run | 10 minutes. After that the configured `prefer` decides again |
| `at` | when that selection happened | as above |
| `backend` | the transport backend that worked per address kind | no TTL. It persists until overwritten, and an empty value is ignored so a later run cannot erase what an earlier one learned |

A missing or corrupt file is not an error: it costs one extra probe. The file is written after every successful endpoint selection, and a write failure never turns a successful login into a failed command. The MCP host re-reads it on every tool call, so a backend another process recorded is picked up without a restart.

## The audit log

### Directory and file naming

| Item | Value |
|---|---|
| default directory | `<config dir>/audit` |
| relocation | `JMS_AUDIT_DIR` wins over the derived directory, for the writer and for every reader |
| file name | `audit-YYYYMMDD-<pid>.jsonl`, dated by the local date of the writer |
| one file per process | several MCP clients appending at once do not contend for one file |

The CLI opens its file on the first event, so a read-only command such as `jms version` leaves no empty file behind. `jms mcp` creates the audit directory at startup, so a broken location is reported before any tool call runs.

`JMS_AUDIT=off` (case-insensitive) disables the sink. The event bus still runs, so `jms attach` and the `--last` replay keep working. Auditing being off is not an error and never fails a command.

### Record format

One JSON object per line:

| Key | Type | Meaning |
|---|---|---|
| `ts` | RFC3339 | event time |
| `pid` | int | process id, stamped by the writer so it matches the file name |
| `actor` | string | `cli`, `mcp`, or the `JMS_ACTOR` override |
| `kind` | string | event kind, see below |
| `server` | string | server alias |
| `asset` | string | asset name |
| `backend` | string | `ssh` or `ws` |
| `endpoint` | string | `internal` or `external` |
| `command` | string | the remote command |
| `exit_code` | int | remote exit status, present only when known |
| `duration_ms` | int64 | wall time |
| `output_bytes` | int | size of the output, not of the preview |
| `preview` | string | truncated output preview |
| `pool` | string | `cold`, `hit`, `evict` or `reap` |
| `idle_ms` | int64 | idle time of the terminal |
| `reason` | string | short cause |
| `error` | string | error text |
| `path` | string | management-API request path, no query string |
| `params` | string | query string with credential-looking values masked |
| `status` | int | HTTP status of a management call |

Only `ts`, `pid` and `kind` are always present.

```json
{"ts":"2026-11-10T15:44:32.947322402+08:00","pid":2885952,"actor":"cli","kind":"api.denied","reason":"path is not allowed for read-only access: the path must start with /api/v1/","path":"../etc/passwd"}
```

### Event kinds

| Kind | Produced by |
|---|---|
| `exec.start` | MCP only |
| `exec.end` | MCP only |
| `pool.cold`, `pool.hit`, `pool.evict`, `pool.reap` | CLI and MCP |
| `session.login`, `session.relogin` | CLI and MCP |
| `error` | MCP, and the CLI on a failed management call |
| `api.read` | `jms api get` and the MCP management reads |
| `api.write` | the MCP write tools |
| `api.denied` | a call refused locally, before any request |
| `endpoint.select` | never emitted by any non-test code; the constant exists but no code path publishes it |

The CLI publishes pool and session events, but not `exec.start` / `exec.end`: those two kinds come from the MCP surface only.

### Preview truncation

The preview is cut to 4096 bytes, and the writer's cut never splits a UTF-8 rune: it walks back to the nearest rune boundary before truncating.

| Variable | Effect |
|---|---|
| `JMS_AUDIT_FULL=1` | the writer stores the preview untruncated |
| `JMS_AUDIT_PREVIEW_BYTES` | tunes the cap. A value that is not a positive integer leaves the default in place and auditing continues |

`JMS_AUDIT_FULL=1` cannot restore text the MCP layer already dropped: the MCP path truncates the preview to 4096 bytes before the event reaches the bus, and it cuts on a plain byte boundary, so a preview that came from the MCP surface can end in the middle of a rune. The CLI publishes no preview at all.

### Retention

| Source | Effect |
|---|---|
| `audit_keep_days` in the config file | the window in days. `0` or absent means never prune |
| `JMS_AUDIT_KEEP_DAYS` | overrides the config file. `0` means never prune |
| `jms log prune --keep <days>` | overrides both, for that run. `--keep 0` is not "never prune": it falls back to the configured window |

The window is resolved as `--keep` → `JMS_AUDIT_KEEP_DAYS` → `audit_keep_days`. With no window at all, `jms log prune` refuses:

```
no retention window configured: pass --keep <days>, set JMS_AUDIT_KEEP_DAYS, or add audit_keep_days to the configuration file
```

Pruning reads the age from the **file name**, not the modification time, so a copied directory does not silently keep files forever. The cutoff is local midnight of today minus the window; a file dated at or after the cutoff is kept. A file whose name carries no eight-digit date is skipped, not deleted. A missing directory is not an error.

Pruning also runs automatically at startup, from the root command's pre-run hook, so a one-shot `jms exec` and a long-lived `jms mcp` enforce the same window. It is throttled to once every 24 hours by the `.prune-stamp` file. A malformed `JMS_AUDIT_KEEP_DAYS` is reported on stderr and treated as "off":

```
jms: JMS_AUDIT_KEEP_DAYS="abc" ignored: keep days "abc" is not a number
```

A prune failure is reported on stderr and never fails the command.

### Reading the log

`jms tail` follows the stream, `jms log show` queries it as a file.

| Command | Flags | Effect |
|---|---|---|
| `jms tail` | `-f`/`--follow`, `--json`, `--last N` | merges every `audit-*.jsonl` in the directory and sorts by timestamp. `--follow` polls every 250 ms and tracks a byte offset per file; a partial trailing line is left unconsumed. Ctrl-C stops it cleanly |
| `jms log show` | `--since`, `--asset`, `--failed`, `--export md` | reads the log and returns immediately. `--since` takes a Go duration (`1h`, `30m`); a bare number is rejected with `--since "2" is not a duration: use a form like 1h or 30m`. `--export md` writes a Markdown table with the columns `Time`, `Server`, `Asset`, `Command`, `Exit`, `Duration`, `Actor` |
| `jms log prune` | `--keep`, `--dry-run`, `--dir` | deletes files older than the window. `--dry-run` reports what would go |

A missing audit directory is not an error for `jms tail`; it means an empty stream. `jms log show` prints `No audit events matched.` when nothing matches.

## Environment variables

Every variable the tool reads, in one place.

| Variable | Accepted value | Default | Read by | Effect |
|---|---|---|---|---|
| `JMS_CONFIG` | any non-blank path | platform config dir | `internal/config` | the configuration file path. Also relocates `state.json` and `audit/` |
| `JMS_PASSWORD_<ALIAS>` | the login secret | unset → the OS store | `internal/config` | the credential for one alias. Read-only: `jms config add` cannot write it |
| `JMS_OTP_<ALIAS>` | the base32 TOTP secret | unset → MFA is prompted for | `internal/config` | the TOTP secret for one alias. Read-only |
| `JMS_AUDIT` | `off` | auditing on | `internal/obs` | disables the audit sink. The bus keeps running |
| `JMS_AUDIT_DIR` | a directory path | `<config dir>/audit` | `internal/obs`, `internal/cli` | relocates the audit directory for the writer and the readers |
| `JMS_AUDIT_FULL` | exactly `1` | off | `internal/obs` | stores the preview untruncated |
| `JMS_AUDIT_PREVIEW_BYTES` | a positive integer | `4096` | `internal/obs` | the preview cap. A malformed value keeps the default |
| `JMS_AUDIT_KEEP_DAYS` | a non-negative integer, `0` = never prune | the config file value | `internal/cli` | the retention window in days |
| `JMS_ACTOR` | any non-blank string | `cli` / `mcp` | `internal/cli` | the actor name stamped on audit events |
| `JMS_MCP_TOOLSET` | `core` | admin tools registered | `internal/mcpserver` | `core` registers only the pre-MIFA-73 tools |
| `JMS_MCP_WRITE` | `limited` | no write tools registered | `internal/mcpserver` | `limited` registers the write tools |
| `JMS_MCP_WRITE_DOMAINS` | comma list of `node`, `account` | all registered domains | `internal/mcpserver` | narrows `JMS_MCP_WRITE=limited` |
| `JMS_MCP_DEBUG` | `1`, `true`, `yes`, `on` | off | `internal/mcpserver` | enables the debug-only `jms_pool_status` tool |
| `JMS_MCP_BACKEND` | `ssh`, `ws`, `auto` | no override | `internal/mcpserver` | forces the transport backend for the process |
| `JMS_MCP_ENDPOINT` | `internal`, `external` | no override | `internal/mcpserver` | forces the address kind for the process |
| `JMS_MCP_EXEC_TIMEOUT` | a Go duration (`90s`, `10m`) or a positive number of seconds | `5m` | `internal/mcpserver` | the ceiling for a `jms_exec` call with no explicit timeout |
| `JMS_WS_DUMP` | any non-empty value | off | `internal/transport` | dumps every WebSocket frame to stderr. Debug only: it prints payload bytes |

Non-`JMS_` variables that matter:

| Variable | Effect |
|---|---|
| `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` | never read |
| `DISPLAY`, `WAYLAND_DISPLAY`, `XDG_SESSION_TYPE` | Linux only. They decide how the "keyring is locked" message is phrased |
| `XDG_CONFIG_HOME`, `HOME`, `AppData` | locate the default configuration directory |

## Security boundaries

- **The configuration file never holds a secret.** Not the login secret, not a TOTP secret, not an encrypted one. The schema has no field for one.
- **The OS credential store holds one entry per alias and per kind**, under the service name `jms-client`. Removing the alias removes both entries.
- **The environment fallback is read-only.** A write to it fails with `credential store is read-only`, which is why `jms config add` falls through to the OS store.
- **An error message names the variable, never the value.** The audit log masks credential-looking query parameters on the way in, not on the way out.
- **The audit files are owner-only** (`0600` in a `0700` directory) and one file per process, so a rotated or copied log is not world-readable.
- **`JMS_AUDIT=off` is a deliberate opt-out.** It disables the file, not the event bus: `jms attach` still observes the host.
- **`JMS_WS_DUMP` prints protocol payloads to stderr.** It is a debugging switch and nothing else.

## Limitations

- **No v1-to-v2 migration.** The loader accepts any `version` value and does not convert an old schema. The only refused format is the legacy Python `jms-cli` `config.yaml`.
- **`--config` does not relocate `state.json` or `audit/`.** Only `JMS_CONFIG` moves the whole directory.
- **`--config` does not reach the MCP server.** `jms mcp` loads the configuration from `JMS_CONFIG` or the platform default.
- **The keyring lock is detected, not solved.** The client fails fast and names the environment variable; it cannot unlock the store itself.
- **`JMS_AUDIT_FULL` cannot restore MCP output.** The MCP layer truncates the preview before the event reaches the bus.
- **`endpoint.select` never appears.** The kind exists in the enum but no code path publishes it.
- **The retention window is a single number per audit directory**, no matter which alias produced the records.
- **The audit log is local.** Nothing is shipped to a central collector.
