# jms-client

[![Release](https://img.shields.io/github/v/release/MiFaZhan/jms-client)](https://github.com/MiFaZhan/jms-client/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](go.mod)

**A command-line client and MCP server for [JumpServer v4](https://docs.jumpserver.org/zh/v4/) bastion hosts.**
One static binary that reaches the assets behind your bastion the same way the web console does — no VPN client, no SSH config, no per-host credentials.

```console
$ jms exec web-01 "systemctl is-active nginx"
active
```

**English** · [简体中文](README.zh-CN.md) · **Docs:** [Command reference](docs/command-reference.md) · [MCP](docs/mcp.md) · [Configuration](docs/configuration.md) · [Networking](docs/networking.md) · [Troubleshooting](docs/troubleshooting.md) · [Design](DESIGN.md)

## Why this exists

Reaching a bastion normally means a browser tab, or a `~/.ssh/config` you maintain by hand. Neither composes with a shell pipeline, a CI job, or an AI agent. `jms-client` talks to the same two JumpServer APIs the web console uses (Core's REST API and KoKo's terminal gateway):

- **No VPN, no SSH config, no per-host credentials.** Configure one server alias; the client logs in and reaches the assets behind it.
- **One login serves many commands.** Logins are pooled per server and terminals per `server/asset/account/protocol`; both are reused.
- **The same config works inside and outside the office.** Give a server an internal and an external address; the client probes them in turn, fails over to the one that answers, and remembers the last good one.
- **Everything is logged locally, and an AI assistant can drive the same pool.** `jms mcp` speaks the Model Context Protocol over stdio, so Claude Desktop, Claude Code, Codex, Cursor and friends use the same pooled, audited connections.

## Requirements

- **JumpServer v4** with the **KoKo** component reachable from your machine. JumpServer v3 and earlier are not supported.
- A JumpServer account already authorized for the assets you want to reach.
- The WebSocket terminal backend needs only a reachable HTTP(S) address. SFTP and the SSH backend additionally need KoKo's SSH port (default `2222`) open — many deployments expose only the web port.

## Install

Prebuilt archives live in [**Releases**](https://github.com/MiFaZhan/jms-client/releases/latest). Put `jms` (`jms.exe` on Windows) on your `PATH`.

| Platform | Archive |
|---|---|
| Linux x86_64 | `jms-client_<version>_linux_amd64.tar.gz` |
| Linux arm64 | `jms-client_<version>_linux_arm64.tar.gz` |
| macOS Intel | `jms-client_<version>_darwin_amd64.tar.gz` |
| macOS Apple Silicon | `jms-client_<version>_darwin_arm64.tar.gz` |
| Windows x86_64 | `jms-client_<version>_windows_amd64.zip` |
| Windows arm64 | `jms-client_<version>_windows_arm64.zip` |

Each archive also contains `LICENSE` and `README.md`. Verify the download against `checksums.txt` from the same release: `sha256sum -c --ignore-missing checksums.txt` on Linux, `shasum -a 256 -c --ignore-missing checksums.txt` on macOS.

**Scoop (Windows)** — the manifest lives in `MiFaZhan/scoop-bucket` under the app name `jms-client`:

```powershell
scoop bucket add mifazhan https://github.com/MiFaZhan/scoop-bucket
scoop install jms-client
```

**Go** (1.25 or newer) — `go install github.com/MiFaZhan/jms-client/cmd/jms@latest`. **From source** — `git clone https://github.com/MiFaZhan/jms-client && cd jms-client && go build -o jms ./cmd/jms`

**MCPB bundle** — download `jms-client.mcpb` from [Releases](https://github.com/MiFaZhan/jms-client/releases/latest) and open it with an MCPB-aware client. It carries a binary for all six platform/architecture combinations above plus a launcher that picks the right one, so no Go toolchain is needed.

## Quick start

The credential is validated against a reachable address **before** anything is written, so a mistyped value leaves no trace on disk; it goes to the OS credential store, never into `config.toml`. Either address may be left empty, but at least one is required, and the first server you add becomes the default. A target is `<asset>` or `<asset>@<server>`; without `@<server>` the default server is used.

```console
$ jms config add bastion
Internal URL (http(s)://..., empty to skip): https://jumpserver.example.com/
External URL (http(s)://..., empty to skip):
Username: alice
Password:
Confirm Password:
TOTP secret (base32, empty to enter the code each time):
Server "bastion" saved to /home/alice/.config/jms/config.toml
Server "bastion" is now the default.
$ jms config test
Server: bastion (default)

Endpoint  URL                                Reachable  Probe  Login
external  https://jumpserver.example.com/    yes        12ms   ok

Credentials: keyring
$ jms ls
Using external endpoint https://jumpserver.example.com/
Name     Address     Platform  Type
web-01   10.20.0.11  Linux     Host
db-01    10.20.0.32  Linux     Host

Total: 2 asset(s)
$ jms exec web-01 "uptime"
 14:22:31 up 12 days,  3:07,  2 users,  load average: 0.21, 0.18, 0.15
$ jms login web-01
Connected to web-01 (ws backend). Press Ctrl+] to exit.
$ jms sftp web-01:/var/log/app.log .
Transferred 2048 bytes in 120ms
MD5 verified: 5d41402abc4b2a76b9719d911017c592
```

## Connect an AI assistant (MCP)

`jms mcp` runs a [Model Context Protocol](https://modelcontextprotocol.io) server over stdio. Any MCP-capable client can then list assets, run commands and move files through the same pooled connections your shell uses.

```json
{
  "mcpServers": {
    "jms": { "command": "jms", "args": ["mcp"], "env": { "JMS_MCP_ENDPOINT": "external", "JMS_MCP_BACKEND": "ws", "JMS_MCP_EXEC_TIMEOUT": "10m" } }
  }
}
```

`jms mcp --print-config` prints that entry without the `env` block; it contains no credential. The MCP process has no per-call flags — your client starts one long-lived process — so the endpoint (`internal`|`external`), the backend (`ssh`|`ws`|`auto`) and the exec timeout are steered by environment variables. `jms mcp --config <path>` does **not** reach the MCP server; use `JMS_CONFIG` or the per-tool `config_path` argument.

| Block | Tools | Count |
|---|---|---|
| Core | `jms_ls`, `jms_resolve_asset`, `jms_exec`, `jms_sftp_upload`, `jms_sftp_download`, `jms_sftp_relay`, `jms_config_list`, `jms_asset_list`, `jms_asset_show`, `jms_asset_create`, `jms_asset_update`, `jms_asset_delete`, `jms_asset_grant`, `jms_asset_permissions`, `jms_asset_revoke` | 15 |
| Management reads | `jms_audit_list`, `jms_session_list`, `jms_session_show`, `jms_command_list`, `jms_node_tree`, `jms_platform_list`, `jms_account_list`, `jms_identity_list`, `jms_identity_show`, `jms_job_list`, `jms_job_execution_list`, `jms_ticket_list`, `jms_ticket_show`, `jms_local_audit` | 14 |
| Writes (off by default) | `jms_node_write`, `jms_account_secret` | +2 |
| Debug (off by default) | `jms_pool_status` | +1 |

Registered tools: **29** by default (15 core + 14 management reads), **15** with `JMS_MCP_TOOLSET=core`, **31** with `JMS_MCP_WRITE=limited`, **32** — the maximum — with `JMS_MCP_WRITE=limited` and `JMS_MCP_DEBUG=1`. The write tools are **off by default**: `JMS_MCP_WRITE` must be exactly `limited`, `JMS_MCP_WRITE_DOMAINS` narrows them to `node`, `account` or both, and the destructive operations require a `confirm` argument. The gates are read when the server starts, so change them and restart the client.

**Details:** [docs/mcp.md](docs/mcp.md) · [简体中文](docs/mcp.zh-CN.md)

## Provision assets and permissions

JumpServer separates "an asset exists" from "a user may reach it": only an `AssetPermission` exposes an asset to a user. A newly created asset therefore shows up in `jms asset list` (the admin view) and **not** in `jms ls` (the per-user view) until it has been granted. `--grant-me` does both steps in one command:

```console
$ jms asset create "web-02" 10.20.0.44 --account ops --username ops --password '…' --grant-me
Created web-02 (id 3f9c2a1e-8d44-4c0a-9d0e-1b7c5e2a4411)
Granted to the current user via permission grant-web-02
$ jms asset grant web-02 --user alice --group ops
Granted web-02 to 1 user(s) and 1 user group(s) via permission grant-web-02 (id 8c1d0f22-...)
$ jms asset permissions web-02
Name          Users   Groups  Assets  Nodes  Protocols  Actions
grant-web-02  alice   ops     web-02  -      all        connect

Total: 1 permission(s)
$ jms asset revoke grant-web-02 --yes
Revoked grant-web-02 (id 8c1d0f22-...)
```

Without `--grant-me` the create command prints a note on stderr that the asset is not connectable yet, and exits 0. `--user` takes a user id or an exact username, `--group` a group id or an exact group name; both may be repeated or comma-separated. With neither `--user` nor `--group` the authenticated user is granted, and `--self` is additive. Server-side defaults: accounts `@ALL`, protocols `all`, actions `connect`. `jms asset permissions` prints users, groups, assets and nodes, so group-only and node-only rules are readable. Deleting an asset does not delete the permissions that referenced it.

**Details:** [docs/command-reference.md](docs/command-reference.md) · [简体中文](docs/command-reference.zh-CN.md)

## Command map

Every top-level command and its subcommands. `--config <path>` is the only global flag; full flag tables are in [docs/command-reference.md](docs/command-reference.md).

| Command | Subcommands | What it does |
|---|---|---|
| `jms api` | `get <path> [server]` | Read the management API by path (read-only, path allowlist) |
| `jms asset` | `list`, `show`, `create`, `update`, `delete`, `grant`, `permissions`, `revoke` | Manage assets and who may reach them |
| `jms attach` | — | Watch a running host's event stream, optionally run commands |
| `jms completion` | `bash`, `fish`, `powershell`, `zsh` | Shell completion script |
| `jms config` | `add`, `list`, `remove`, `set-default`, `test` | Manage server entries and their credentials |
| `jms exec` | — | Run one command on an asset |
| `jms login` | — | Open an interactive shell on an asset |
| `jms ls` | — | List or search the assets you are authorized for |
| `jms log` | `show`, `prune` | Query and prune the local audit log |
| `jms mcp` | — | Run the stdio MCP server |
| `jms sftp` | — | Upload or download a file |
| `jms ssh-pipe` | — | Bridge stdio to an asset (for `rsync -e`) |
| `jms tail` | — | Follow the local audit stream |
| `jms version` | — | Print version information |

## Configuration

| Platform | Path |
|---|---|
| Linux | `$XDG_CONFIG_HOME/jms/config.toml`, else `~/.config/jms/config.toml` |
| macOS | `~/Library/Application Support/jms/config.toml` |
| Windows | `%AppData%\jms\config.toml` |

`--config` overrides `JMS_CONFIG`, which overrides the platform default. Those five root keys — `version`, `default_server`, `proxy`, `audit_keep_days` and `[servers.<alias>]` — are the whole schema. At least one of `internal` / `external` is required per server; `prefer` picks the address to try first, `pin` restricts the server to one address (no failover), `ssh_port` overrides the KoKo SSH port.

```toml
version = 2
default_server = "bastion"
# proxy = "socks5://127.0.0.1:1080"     # global proxy; omitted means direct
# audit_keep_days = 90                  # retention days; 0 means never prune
[servers.bastion]
internal = "http://192.168.1.10:2280/"       # optional
external = "https://jumpserver.example.com/" # optional
username = "alice"
```

| Variable | Effect |
|---|---|
| `JMS_CONFIG` | Configuration file path |
| `JMS_PASSWORD_<ALIAS>` | Credential for that alias (alias uppercased, `-` becomes `_`) |
| `JMS_OTP_<ALIAS>` | TOTP secret for that alias |
| `JMS_ACTOR` | Actor name stamped on audit events |
| `JMS_AUDIT`, `JMS_AUDIT_DIR`, `JMS_AUDIT_KEEP_DAYS` | Disable the audit log, relocate it, set the retention window |
| `JMS_MCP_ENDPOINT`, `JMS_MCP_BACKEND`, `JMS_MCP_EXEC_TIMEOUT` | MCP only: address kind, backend, exec timeout |

**Details:** [docs/configuration.md](docs/configuration.md) · [简体中文](docs/configuration.zh-CN.md) · [docs/networking.md](docs/networking.md)

## Security and audit

- The credential and the TOTP secret live in the OS credential store (Windows Credential Manager, macOS Keychain, Linux Secret Service) under the service name `jms-client`. The config file is written atomically with mode `0600` and never contains a credential.
- The environment variable takes precedence over the credential store, which is how you run headless (SSH, CI, a container). A blank value counts as unset.
- Every command is written to a local JSONL audit stream in `<config dir>/audit/audit-YYYYMMDD-<pid>.jsonl`, one file per process, mode `0600`. The CLI records pool and session events; the MCP surface records `exec.start` / `exec.end` for every tool call. Output previews are truncated to 4096 bytes and credential-looking query values are masked.
- `jms log show` queries that stream, `jms tail` follows it, `jms attach` watches a running host's live events.

## Limitations

- **JumpServer v4 only.** The v3 authentication and terminal APIs differ.
- **No jump-host chain.** Proxying or jumping through another asset is not implemented.
- **`jms sftp` does not do recursive copies or asset-to-asset relay.** The MCP tool `jms_sftp_relay` relays; the CLI does not.
- **32-bit platforms are not built.** Only x86_64 and arm64.
- **The audit log is local.** Nothing is forwarded to a central collector; ship the files yourself.
- **`jms asset permissions <asset>` matches the asset relation only.** A rule that grants a *node* instead of the asset is not listed for that asset, and a failed detail read there is skipped silently.
- **Management writes are gated.** The MCP write tools need `JMS_MCP_WRITE=limited`, and `jms_asset_delete` and `jms_asset_revoke` have no confirmation argument, unlike their CLI counterparts.
- **The SSH backend has not been exercised against a live bastion** in this tree; the KoKo SSH entry is disabled on several deployments.

## Documentation

- [docs/command-reference.md](docs/command-reference.md) — every command, flag, default, exit code
- [docs/mcp.md](docs/mcp.md) — MCP setup, tools, gates, error semantics
- [docs/configuration.md](docs/configuration.md) — config schema, credential stores, audit format
- [docs/networking.md](docs/networking.md) — addresses, failover, proxies, backends
- [docs/troubleshooting.md](docs/troubleshooting.md) — symptom → cause → fix
- [DESIGN.md](DESIGN.md) — architecture, protocol findings, failure rules (Chinese)

## Development

Build, test and format with `go build ./... && go test ./... && go vet ./... && gofmt -l .` — `gofmt` must print nothing. The test suite runs against local fakes and needs no JumpServer, so a fresh clone is green. Releases are cut with GoReleaser; `scripts/release.sh` stamps the MCPB manifest and publishes the Scoop manifest.

## License

MIT © MiFaZhan — see [LICENSE](LICENSE). The design and the KoKo protocol findings build on [GCS-ZHN/jms-cli](https://github.com/GCS-ZHN/jms-cli).
