# jms-client

[![Release](https://img.shields.io/github/v/release/MiFaZhan/jms-client)](https://github.com/MiFaZhan/jms-client/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](go.mod)

**A command-line client and MCP server for [JumpServer v4](https://docs.jumpserver.org/zh/v4/) bastion hosts.**
One self-contained binary that reaches the assets behind your bastion the same way the web terminal does — no VPN client, no SSH config, no per-host credentials.

Use it on its own to list assets, run commands, open shells and move files. Run `jms mcp` and an AI assistant (Claude Desktop, Claude Code, Codex, Cursor, pi, …) can do the same things through pooled, audited connections.

```console
$ jms exec web-01 "systemctl is-active nginx"
active
```

**English** · [简体中文](README.zh-CN.md)

---

## Why this exists

Reaching a bastion usually means a browser tab, or a `~/.ssh/config` you maintain by hand. Neither composes with a shell pipeline, a build script, or an AI agent.

`jms-client` talks to the same two JumpServer APIs the web console uses (Core's REST API and KoKo's terminal gateway), so:

- **Credentials never sit in a config file.** The password and TOTP secret go to your OS credential store. The config file holds addresses and usernames only, so it is safe to sync, back up, or commit.
- **One login serves many commands.** Connections are pooled per server/asset/account/protocol and reused.
- **The same config works inside and outside the office.** Give a server both an internal and an external address and the client picks whichever answers.
- **Everything is logged locally.** Every command an AI agent runs is written to a JSONL audit stream you can read, follow, or join interactively.

## Features

| | |
|---|---|
| **Single static binary** | No runtime dependencies. Linux, macOS, Windows on x86_64 and arm64. |
| **Dual-address failover** | Internal/external addresses probed and selected automatically, with a remembered last-good address for fast cold starts. |
| **Connection pooling** | Login sessions and terminals are reused per `server/asset/account/protocol`. Idle connections are reaped; broken ones are rebuilt. |
| **Credentials in the OS store** | Windows Credential Manager, macOS Keychain, Linux Secret Service. Environment-variable fallback for containers and CI. |
| **MCP server** | Seven tools over stdio for AI clients, sharing one pooled process. |
| **Local audit stream** | Structured JSONL per process; `jms tail` to follow it, `jms attach` to watch and join. |
| **Two terminal backends** | WebSocket (default, same path as the web console) and SSH, with automatic selection. |

## Requirements

- **JumpServer v4** with the **KoKo** component reachable from your machine. JumpServer v3 and earlier are not supported.
- An account on that JumpServer with the assets you want to reach already authorized.
- The WebSocket backend requires nothing beyond a reachable HTTP(S) address. The SFTP and SSH features additionally require KoKo's SSH port (default `2222`) to be open on the network path — many deployments only expose the web port, so check before relying on them.

## Install

### Prebuilt binaries (recommended)

Download the archive for your platform from [**Releases**](https://github.com/MiFaZhan/jms-client/releases/latest) and put the `jms` binary (`jms.exe` on Windows) on your `PATH`.

| Platform | File |
|---|---|
| Windows x86_64 | `jms-client_<version>_windows_amd64.zip` |
| Windows arm64 | `jms-client_<version>_windows_arm64.zip` |
| macOS Apple Silicon | `jms-client_<version>_darwin_arm64.tar.gz` |
| macOS Intel | `jms-client_<version>_darwin_amd64.tar.gz` |
| Linux x86_64 | `jms-client_<version>_linux_amd64.tar.gz` |
| Linux arm64 | `jms-client_<version>_linux_arm64.tar.gz` |

`checksums.txt` in the same release lets you verify the download:

```sh
sha256sum -c --ignore-missing checksums.txt     # Linux
shasum -a 256 -c --ignore-missing checksums.txt # macOS
```

### Scoop (Windows)

```powershell
scoop bucket add mifazhan https://github.com/MiFaZhan/scoop-bucket
scoop install jms-client
```

Upgrade later with `scoop update jms-client`.

### Go

Requires Go 1.25 or newer:

```sh
go install github.com/MiFaZhan/jms-client/cmd/jms@latest
```

### From source

```sh
git clone https://github.com/MiFaZhan/jms-client
cd jms-client
go build -o jms ./cmd/jms
```

### For MCP clients only

If all you want is the AI integration and you would rather not manage a binary yourself, download **`jms-client.mcpb`** from [Releases](https://github.com/MiFaZhan/jms-client/releases/latest) and open it with your MCP client (Claude Desktop and other MCPB-aware clients handle this by double-click). That single bundle contains the binaries for all six platform/architecture combinations; a bundled launcher picks the right one at startup.

## Quick start

### 1. Store your server

```console
$ jms config add bastion
Internal URL (http(s)://..., empty to skip): https://jumpserver.corp.example.com/
External URL (http(s)://..., empty to skip):
Username: alice
Password:
Confirm password:
TOTP secret (base32, empty to enter the code each time):
Server "bastion" saved to /home/alice/.config/jms/config.toml
Server "bastion" is now the default.
```

Either address may be left empty, but at least one is required. The credentials are validated against a reachable address **before** anything is written, so a mistyped password changes nothing on disk. The first server you add becomes the default.

### 2. Check it

```console
$ jms config test
Server: bastion (default)

Endpoint  URL                                     Reachable  Probe  Login
external  https://jumpserver.corp.example.com/     yes        12ms   ok

Credentials: keyring
```

`config test` probes every configured address and attempts a real login on each reachable one, so a network problem is reported separately from a rejected password, and you can see which addresses actually work. An address can pass the TCP probe and still fail the login — a VPN or proxy can accept the connection with nothing useful behind it.

### 3. Use it

```console
$ jms ls                                   # assets you are authorized for
$ jms ls -q web                            # search by keyword
$ jms exec web-01 "uptime"                 # one command, exit code passes through
$ jms login web-01                         # interactive shell (Ctrl+] to exit)
$ jms sftp web-01:/var/log/app.log .       # download
$ jms sftp ./config.yml web-01:/etc/app/   # upload
```

## Connecting an AI assistant (MCP)

`jms mcp` runs a [Model Context Protocol](https://modelcontextprotocol.io) server over stdio. Any MCP-capable client can then list assets, run commands and transfer files through the same pooled connections your shell uses.

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

Print this snippet at any time with:

```sh
jms mcp --print-config
```

Because the MCP process has no command-line flags (your client launches one long-lived process and cannot add arguments per call), the endpoint, backend and command timeout are steered with environment variables in that client entry:

```json
{
  "mcpServers": {
    "jms": {
      "command": "jms",
      "args": ["mcp"],
      "env": {
        "JMS_MCP_BACKEND": "ws",
        "JMS_MCP_EXEC_TIMEOUT": "10m"
      }
    }
  }
}
```

### Tools

| Tool | Purpose | Required arguments |
|---|---|---|
| `jms_ls` | List or search authorized assets | — |
| `jms_resolve_asset` | Resolve an asset to its address, account and protocol | `asset` |
| `jms_exec` | Run one command and return its output | `asset`, `command` |
| `jms_sftp_upload` | Upload a local file to an asset | `src`, `asset`, `dst` |
| `jms_sftp_download` | Download a file from an asset | `asset`, `src`, `dst` |
| `jms_sftp_relay` | Stream a file directly between two assets | `src_spec`, `dst_spec` |
| `jms_config_list` | List configured servers and their credential source | — |

An eighth tool, `jms_pool_status`, is registered when `JMS_MCP_DEBUG=1` and reports the shape of the connection pool. It is for diagnosing the server, not for daily use.

Most tools accept optional `server`, `config_path`, `account`, `protocol` and `timeout` arguments. Remote specs use the form `<asset>[@<server>]:<path>`.

Two behaviours worth knowing when you write prompts for an agent:

- **A non-zero exit code is not reported as a failure.** `jms_exec` returns the command's output; the remote exit status is recorded in the audit log but not surfaced in the tool result. If your agent must branch on success, make the command say so (`… && echo OK || echo FAIL`).
- **Commands are never replayed.** If a call fails after the command was already delivered to the host, the client reports the error instead of retrying, because `rm`, `reboot` and friends are not idempotent. A dropped connection triggers one recovery attempt through a fresh login, but only for a command that produced no output at all.

## Command reference

```
jms config  add | list | remove | set-default | test    Server metadata and credentials
jms ls      [server] [-q keyword] [-n limit] [--endpoint]   List or search assets
jms exec    <target> <command...> [flags]               Run one command
jms login   <target> [flags]                            Interactive shell
jms sftp    <src> <dst> [flags]                         Upload or download a file
jms ssh-pipe <target> [command...]                      Bridge stdio to an asset (rsync -e)
jms tail    [-f] [--json] [--last N]                    Follow the local audit stream
jms attach  [--attach] [--asset] [--last]               Watch a running host, optionally join
jms log     show [--since] [--asset] [--failed] [--export md]   Query the audit log as a file
jms mcp     [--print-config]                            Run the stdio MCP server
jms version                                             Print version information
```

**Target syntax.** `<asset>@<server>` names an asset on a specific server; omitting `@<server>` uses the default server. The last `@` separates the two, so an asset name may contain `@`.

### `jms config`

| Command | What it does |
|---|---|
| `add <alias>` | Prompt for addresses, username, password and an optional TOTP secret; validate, then store. `--set-default` also makes it the default. |
| `list` | One row per server, with the address(es), username, credential source, and which is the default. Warns on stderr about any server whose password is missing. |
| `remove <alias>` | Delete the server and its stored credentials. `-y` skips the confirmation. |
| `set-default <alias>` | Change the default server. |
| `test [alias]` | Probe every configured address and attempt a login on the first reachable one. |

A configuration file with no servers is never written — `jms config add` would otherwise be unable to read back its own file. Removing your **last** server therefore deletes the file outright, returning you to the pre-`add` state, and reports `no servers remain`.

### `jms ls`

| Flag | Meaning |
|---|---|
| `-q`, `--search` | Keyword to search for; lists everything when omitted. |
| `-n`, `--limit` | Maximum number of results (default 50). |
| `--endpoint` | Force `internal` or `external`. |

The chosen address is printed to **stderr**, so stdout stays a clean table you can pipe.

### `jms exec`

| Flag | Meaning |
|---|---|
| `-t`, `--timeout` | Timeout in **seconds**. See the note below — the default is not unlimited. |
| `--backend` | `ws`, `ssh` or `auto` (default `ws`). |
| `--endpoint` | Force `internal` or `external`. |
| `--account`, `--protocol` | Override the account or protocol used to reach the asset. |

The command is passed through as one argument; quoting is yours to do:

```sh
jms exec web-01 'df -h /'                 # good: one argument
jms exec web-01 df -h /                   # WRONG: -h is parsed by jms, not by the host
jms exec web-01 -- nginx -t               # use -- when the remote command starts with a dash
```

The remote exit code becomes the exit code of `jms`, so this works:

```sh
jms exec web-01 "test -f /etc/app.conf" && echo "present"
```

Diagnostics (`Using external endpoint …`, `Command exited with status N`) go to stderr, keeping stdout pure command output for `$(…)` capture.

> **The default timeout depends on the backend.** With the WebSocket backend, `jms exec` without `-t` is capped at **30 seconds**, and a longer command fails with a "did not complete within 30s" error. Pass `-t` for anything slower:
>
> ```sh
> jms exec -t 600 web-01 "./build.sh"
> ```
>
> The MCP `jms_exec` tool defaults to **5 minutes** instead, tunable with `JMS_MCP_EXEC_TIMEOUT`. A command ending in `#` or `\`, or with unbalanced quotes, can swallow the status marker and will also hit the timeout — the error message says so, because from the client's side it looks identical to a slow command.

### `jms login`

Opens an interactive shell and attaches your terminal. The session belongs to that process and is not pooled. `Ctrl+]` exits. `--timing` prints a breakdown of the login stages, which is the first thing to reach for when a connection is slow.

> **`jms login` forces the external address by default.** It does not fall back to the internal one, and on a server that only has an internal address it fails with `server "…" has no external address configured`. Pass `--endpoint internal` if your bastion has no external address. Other commands (`ls`, `exec`, `sftp`) apply the normal failover policy instead.

### `jms sftp`

Exactly one side must be a remote spec (`<asset>[@<server>]:<path>`); the direction is inferred from which side is remote. Two local paths are an error — use `cp`.

| Flag | Meaning |
|---|---|
| `--verify` / `--no-verify` | MD5 verification. **On by default**; `--verify` exists only for symmetry. |
| `--chunk` | Chunk size in bytes (default 1 MiB). |
| `--parallel` | Concurrent chunk streams (default 4). |
| `--account`, `--endpoint` | Override the account or forced address. |

```sh
jms sftp web-01:/var/log/app.log ./app.log     # download
jms sftp ./app.yml web-01:/etc/app/app.yml     # upload
```

**Not yet available from the CLI:** recursive directory transfers (`-R`) and asset-to-asset relay. The flags and help text mention them, but the command returns an error rather than copying through a local temporary file. For asset-to-asset moves, use the MCP tool `jms_sftp_relay`, or go through your local machine with two `jms sftp` calls. Requiring KoKo's SSH port (default 2222) to be reachable.

### `jms ssh-pipe`

Bridges stdin/stdout to an asset, speaking the remote side of the protocol on stdout. It exists so `rsync` can transfer through the bastion:

```sh
rsync -av -e "jms ssh-pipe" ./dist/ web-01:/opt/app/dist/
```

Nothing but protocol bytes is written to stdout; all diagnostics go to stderr.

### Observing what ran

| Command | Use |
|---|---|
| `jms tail -f` | Follow the audit stream live. `--json` for raw JSONL, `--last N` to start with history. |
| `jms attach` | Watch a running `jms mcp` host's event stream. Observation only by default. |
| `jms attach --attach` | Also enable the operator prompt, so you can run commands on the host's pooled connections. |
| `jms log show` | Read the log as a file: returns immediately. `--since 1h`, `--asset`, `--failed`, `--export md`. |

`jms attach` deliberately refuses input unless you pass `--attach`, so a window left open cannot turn a stray keystroke into a command on a production host. Operator commands and AI commands share one pool and serialize on the same connection rather than racing for it.

## Configuration

Config lives at:

| Platform | Path |
|---|---|
| Windows | `%AppData%\jms\config.toml` |
| Linux / macOS | `~/.config/jms/config.toml` (or `$XDG_CONFIG_HOME/jms/config.toml`) |

Override with the `--config` flag or the `JMS_CONFIG` environment variable. Relocating the file also relocates the audit directory, because both live beside it.

```toml
version = 2
default_server = "bastion"

# Optional global proxy policy. Omitted means: connect directly.
# proxy = "socks5://127.0.0.1:1080"

[servers.bastion]
internal = "http://192.168.1.10:2280/"          # internal address (optional)
external = "https://bastion.example.com/"       # external address (optional)
username = "alice"

# prefer = "external"       # try this address first; default is internal
# pin = "internal"          # use only this address: no probing, no failover
# ssh_port = 2222           # KoKo SSH port if not the default
# proxy = "direct"          # force a direct connection for this server

[servers.staging]
external = "https://jumpserver.staging.example.com/"
username = "alice"
prefer = "external"
```

Every field except the username is optional, but at least one address is required.

### How an address is chosen

When a server has both addresses, the client:

1. Puts the remembered last-good address first (kept for 10 minutes in `state.json`).
2. Orders the rest by `prefer` (default: internal first).
3. Probes each candidate with a 1.5-second TCP connect.
4. Logs into the first one that answers.

Failure to reach one address does not fail the command — it moves to the next. Two deliberate exceptions:

- **Authentication errors (401/403) never trigger a failover.** If the password is wrong, the other address would refuse it too.
- **`pin` disables failover entirely.** Use it when you know which network you are on and would rather see an honest error than a silent fallback.

A failed connection re-reads the state file and retries once through a fresh login, but only if the command had not already started producing output.

### Credentials

| Data | Where it lives |
|---|---|
| Aliases, addresses, username, preferences | `config.toml`, plain text (0600) |
| **password**, **TOTP secret** | The OS credential store, under the service name `jms-client` |

The config file is written atomically and never contains a credential, which is what makes it safe to sync between machines. Acknowledge the boundary this implies: it protects against file leakage and copying a config to another machine, not against a malicious process running as the same user.

For headless environments (containers, CI, WSL without a Secret Service) there are environment-variable fallbacks. **The environment takes precedence over the credential store.**

| Variable | Meaning |
|---|---|
| `JMS_PASSWORD_<ALIAS>` | Password for that server alias. |
| `JMS_OTP_<ALIAS>` | TOTP secret for that server alias. |

Aliases are upper-cased and `-` becomes `_`, so `bastion-eu` becomes `JMS_PASSWORD_BASTION_EU`. An exported-but-empty variable counts as unset rather than as an empty password. If neither source has a credential, the command fails with a message naming the variable to set — it never falls back to storing a plaintext password.

One case is worth calling out on its own: **the credential store exists but is locked**, which is the usual state of a GNOME Keyring on a machine with no desktop session. Commands then fail within milliseconds with a "credential store is locked" message and the environment variable to set as the way out, rather than hanging on an unlock prompt that nobody can answer. To see which stage a run reached, add `--timing`.

### Proxy policy

**The client connects directly by default and ignores `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`.**

This is a deliberate departure from the usual convention. On developer machines those variables are frequently exported globally by a transparent-proxy tool, and its own `DIRECT` rules often match the bastion's address — so requests leave through a proxy nobody asked for, and the proxy's failures (typically a 502 with an empty body) are reported as if they came from the network. Worse, the TCP probe and the actual login would then be testing two different network paths.

To route through a proxy, say so explicitly, either globally or per server:

```toml
proxy = "socks5://127.0.0.1:1080"   # http, https, socks5 and socks5h are accepted
```

```toml
[servers.bastion]
proxy = "direct"                    # override a global proxy for this server
```

Configured proxies take precedence over the environment; `NO_PROXY` is not consulted for them either.

### Environment variables

| Variable | Effect |
|---|---|
| `JMS_CONFIG` | Path to the configuration file. |
| `JMS_PASSWORD_<ALIAS>`, `JMS_OTP_<ALIAS>` | Credential fallback (see above). |
| `JMS_ACTOR` | Name recorded in the audit log; use it to tell several clients on one machine apart. |
| `JMS_AUDIT` | Set to `off` to disable the audit log. |
| `JMS_AUDIT_DIR` | Relocate the audit directory. |
| `JMS_AUDIT_FULL` | Set to `1` to record untruncated output previews. |
| `JMS_AUDIT_PREVIEW_BYTES` | Truncation limit for output previews (default 4096). |
| `JMS_MCP_ENDPOINT` | MCP only: force `internal` or `external`. |
| `JMS_MCP_BACKEND` | MCP only: force `ssh`, `ws` or `auto`. |
| `JMS_MCP_EXEC_TIMEOUT` | MCP only: default `jms_exec` timeout, e.g. `10m` or `600`. |
| `JMS_MCP_DEBUG` | MCP only: set to `1` to register `jms_pool_status`. |
| `JMS_WS_DUMP` | Dump raw WebSocket frames, for protocol debugging. |

## Terminal backends

JumpServer's KoKo component offers two terminal channels, and `jms-client` speaks both.

| Backend | Channel | Notes |
|---|---|---|
| `ws` (default) | `/koko/ws/terminal/` | The same path the web console uses. Works wherever the web console works. 30s default command timeout. |
| `ssh` | KoKo SSH port (default 2222) | Native exit statuses per command, no output-marker parsing. Requires the port to be reachable, and SFTP depends on it. |

Select it with `--backend ssh`, or `--backend auto` to try SSH and fall back to WebSocket. The backend that last succeeded for an address is remembered in `state.json`, so a cold start goes straight to it instead of waiting out a handshake on a blocked port. In MCP, the remembered backend also lets the process recover from a stale connection without a restart.

Tools that need SFTP — `jms sftp`, `jms_sftp_upload`, `jms_sftp_download`, `jms_sftp_relay` — require KoKo's SSH port to be open. If file transfers hang or fail while `exec` works fine, that port is the first thing to check.

## Audit log

Every command is recorded as one JSON object per line:

```json
{"ts":"2026-10-05T14:32:01.123+08:00","pid":12345,"actor":"mcp:pi","kind":"exec.end","server":"prod","asset":"web-01","backend":"ssh","endpoint":"external","command":"systemctl status nginx","exit_code":0,"duration_ms":118,"output_bytes":2140,"preview":"● nginx.service ...","pool":"hit","idle_ms":68000}
```

Files are written to `<config dir>/audit/audit-YYYYMMDD-<pid>.jsonl`, one file per process, with `0600` permissions on both the directory and the files. Per-process files avoid the Windows problem of several processes appending to one locked file, and they let a long-running host and a one-shot CLI invocation coexist.

Event kinds: `exec.start`, `exec.end`, `pool.cold`, `pool.hit`, `pool.evict`, `pool.reap`, `session.login`, `session.relogin`, `endpoint.select`, `error`.

Output previews are truncated to 4096 bytes by default (`JMS_AUDIT_PREVIEW_BYTES`, or `JMS_AUDIT_FULL=1` for everything). Nothing in the log interpolates a credential.

## Troubleshooting

**`server "…" has no external address configured`**
You ran `jms login` against a server with only an internal address. Add `--endpoint internal`.

**`command did not complete within 30s`**
You are on the WebSocket backend (the default) and the command ran longer than the default timeout. Raise it with `-t`. If the command ends in `#` or `\` or has unbalanced quotes, the client never sees its completion marker — the same error appears; rewrite the command so it terminates normally.

**`no reachable endpoint: … probe failed`**
Neither address answered a TCP connect. Check that the address in your config is the one you reach from your current network, that a VPN is connected if one is required, and that the port is right. Run `jms config test` for a per-address breakdown.

**Requests go through a proxy you did not configure**
They should not: the client ignores `HTTP_PROXY` and friends. If you see a proxy error anyway, it is a configured `proxy` value in the config file, or a transparent interception at the network level. Remove the config entry, or force `proxy = "direct"`.

**SFTP or SSH fails while `exec` works**
KoKo's SSH port (default 2222) is probably blocked. SFTP and the `ssh` backend both ride on it; the WebSocket backend does not.

**`jms login` renders garbage or leaves my terminal broken**
Reported mainly on older Windows consoles. `jms login` restores your console mode on exit; if a crash left it in raw mode, `reset` (Unix) or closing the terminal (Windows) recovers it.

**The password prompt fails in a container**
There is no OS credential store there. Set `JMS_PASSWORD_<ALIAS>` instead, or set up a Secret Service. `jms config list` shows where each server's password is coming from.

**Every command works in a shell but an AI client reports an error**
Check `jms config list` again: the MCP process may be started by a different user or with a different environment, and so may not see your credential store or `JMS_CONFIG`. Pass explicit `env` entries in the MCP client entry.

**A connection worked and then stopped**
Connections are reaped after 10 minutes idle, and a session's address is re-validated before reuse. A failure re-reads the state file and re-selects the address and backend once. If it persists, restart the MCP host.

## Limitations

- **JumpServer v4 only.** The v3 authentication and terminal APIs differ.
- **`jms sftp` does not do recursive copies or asset-to-asset relay.** Use the MCP `jms_sftp_relay` tool for the latter, or relay through your local machine.
- **32-bit platforms are not built.** Only x86_64 and arm64.
- **The audit log is local.** Nothing is forwarded to a central collector; ship the files yourself if you need to aggregate.
- **A jump-host chain (proxy/jump through another asset) is not implemented.**

## Development

```sh
go build ./...     # compile
go test ./...      # unit and protocol-regression tests
go vet ./...
gofmt -l .         # must print nothing
```

Integration tests connect to a real JumpServer and are gated behind an environment variable; without it they skip, so a fresh clone is green:

```sh
JMS_TEST_SERVER=bastion go test ./...
```

Architecture, the reverse-engineered KoKo protocol details, pool design and failure-handling rules live in [DESIGN.md](DESIGN.md) (written in Chinese).

Releases are cut from GitHub Actions:

```sh
gh workflow run release --repo MiFaZhan/jms-client -f tag=v0.1.7
```

## License

MIT © MiFaZhan. See [LICENSE](LICENSE).

The design and the KoKo protocol findings build on [GCS-ZHN/jms-cli](https://github.com/GCS-ZHN/jms-cli).
