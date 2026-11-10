# Command reference

Every command, flag, default and exit code of `jms` v0.2.4.
The [README](../README.md) has the short version.
Siblings: [简体中文](command-reference.zh-CN.md) · [MCP server](mcp.md) · [Configuration](configuration.md) · [Addresses and proxies](networking.md) · [Troubleshooting](troubleshooting.md).

## Conventions used here

| Convention | Meaning |
|---|---|
| `console` fence | A session transcript. `$` is your shell prompt. |
| `sh` fence | A snippet you paste into a shell. |
| stdout | The data the command produces. Safe to pipe or capture. |
| stderr | Diagnostics, prompts, endpoint lines, warnings. Never part of the data. |

Every command also accepts the built-in `-h, --help`.

The commands that talk to a server (`ls`, `exec`, `login`, `sftp`, `api get`, every `asset` subcommand) print the address they used to stderr as `Using <kind> endpoint <url>`, so stdout stays a clean table or a clean JSON document.

## Target syntax

Most commands that act on an asset take a **target**:

```
<asset>@<server>    asset on a named server
<asset>             asset on the default server
```

| Rule | Behaviour |
|---|---|
| Split point | The **last** `@` separates asset from server. |
| No `@` | The asset lives on the default server (`jms config set-default`). |
| `host@` or `@host` | The whole string is the asset name, on the default server. Not an empty server. |
| `a@b@c` | Asset `a@b`, server `c`. |
| Empty target | `empty target: expected <asset>[@<server>]` |

The `@` form applies to `exec`, `login`, `sftp` (the host part of a remote spec) and `ssh-pipe`.
`jms ls`, `jms asset *` and `jms config test` take the server as a **separate positional argument** instead.

```console
$ jms exec web-01@bastion "uptime"
$ jms exec web-01 "uptime"                 # web-01 on the default server
```

## Global flags and configuration precedence

`--config` is the only global flag. It is persistent, so it is accepted before or after the subcommand name.

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `--config` | string | `""` | Configuration file. Overrides `JMS_CONFIG`. |

The file is chosen in this order:

```
--config <path>   >   JMS_CONFIG   >   <UserConfigDir>/jms/config.toml
```

`<UserConfigDir>` is `$XDG_CONFIG_HOME` or `~/.config` on Unix, `%AppData%` on Windows.
`JMS_CONFIG` is honoured when it is set and non-empty.

Choosing a configuration file also moves two other things, because they are derived from the configuration **directory**:

| Artifact | Location |
|---|---|
| `config.toml` | the `--config` path itself |
| `state.json` | `<config dir>/state.json` |
| audit log directory | `<config dir>/audit` |

```console
$ JMS_CONFIG=/tmp/a.toml jms --config /tmp/b.toml config list   # --config wins
```

A legacy `config.yaml` next to the configuration file is refused on purpose:
`detected legacy jms-cli config — unsupported by design; re-add with jms config add`.

## Exit codes

| Situation | Exit code |
|---|---|
| Success | 0 |
| Any generic error (bad flag value, missing credential, unreachable server, refused path, unconfirmed delete, …) | 1 |
| `jms exec` where the **remote** command exited non-zero | the remote status |
| `jms ssh-pipe` where the remote command exited non-zero | the remote status |
| `jms ssh-pipe` bridge failure | 1 |
| `jms attach` ended with Ctrl-C | 0 |
| `jms tail -f` ended with Ctrl-C | 0 |
| `jms` with no arguments, or a group command with no subcommand | 0 (help) |

A generic failure prints one line to stderr: `Error: <message>`.
A command that carries its own status prints **no** `Error:` line.
The audit log is flushed after every command; a flush failure prints `Warning: audit log: <err>` and does not change the exit code.

---

## `jms version`

```console
$ jms version
jms v0.2.4 (linux/amd64)
```

`jms version [flags]` — no positional arguments, no flags. Output: `jms <version> (<GOOS>/<GOARCH>)` on stdout.

The version is the one stamped into the binary at build time. A binary built from a working copy reports a pseudo-version such as `v0.2.5-0.2026…+dirty`.

## `jms ls`

```console
$ jms ls
Name      Address         Platform  Type
web-01    192.168.10.11   Linux     host
db-01     192.168.10.12   Linux     host

Total: 2 asset(s)
```

`jms ls [server] [flags]` — positional: optional server alias, at most 1.
Output: table `Name`, `Address`, `Platform`, `Type`, trailer `Total: N asset(s)`; `No assets found.` when empty.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--search` | string | `""` | `-q` | Search keyword |
| `--limit` | int | `50` | `-n` | Maximum number of results |
| `--endpoint` | string | `""` | — | Force an address: `internal` or `external` |

- `jms ls` shows only the assets your account may reach. `jms asset list` shows every asset on the bastion.
- `--search` and `--limit` do not combine: with a keyword the whole collection is fetched and filtered server-side, and `--limit` is **not applied**. `--limit` only truncates the plain list, and it is applied on the client after the full list was fetched.
- The endpoint line goes to stderr, so stdout is pipeable.

```console
$ jms ls -q web
$ jms ls bastion -n 10
```

## `jms exec`

```console
$ jms exec web-01 "systemctl is-active nginx"
active
```

`jms exec <target> <command...> [flags]` — positional: the target plus at least one command word (2 or more in total).
Output: remote stdout on stdout; endpoint, timing and status lines on stderr.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--account` | string | `""` | — | Account override |
| `--protocol` | string | `""` | — | Protocol override |
| `--backend` | string | `""` | — | Backend: `ssh`, `ws` or `auto` |
| `--endpoint` | string | `""` | — | Force an address: `internal` or `external` |
| `--timeout` | int | `0` | `-t` | Command timeout in seconds |
| `--timing` | bool | `false` | — | Print exec stage timings |

- The command words are joined with a **single space** and sent as one string. Quoting is your job: `jms exec web-01 'echo hello world'`.
- The default backend is **ws**. `--backend auto` tries `ssh` first, then `ws`. An invalid value gives `invalid --backend "x": must be ssh, ws or auto`.
- stdout carries **only** the remote output. The endpoint line and `Command exited with status N` go to stderr, so `out=$(jms exec web-01 cmd)` stays clean.
- The exit code is the **remote** exit status. A non-zero status also prints `Command exited with status N` on stderr.
- The connection is pooled. A second `exec` on the same asset reuses the live terminal.
- `--timeout 0` (the default) means "no timeout asked for". The backend then applies its own ceiling: the WebSocket transport falls back to **30 seconds**, while the SSH backend has no timer of its own and only the command context stops it. Pass `-t` to set an explicit ceiling.
- `--timing` prints `Timing credentials:`, `Timing endpoint/auth:` and `Timing command:` to stderr. The credential line is printed even when the run fails at that stage.

```console
$ jms exec web-01@bastion "df -h /"
$ jms exec web-01 "sleep 600" -t 900
```

## `jms login`

```console
$ jms login web-01
Connected to web-01 (ws backend). Press Ctrl+] to exit.
```

`jms login <target> [flags]` — positional: the target, exactly 1. The console is handed over; the status line goes to stderr.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--account` | string | `""` | — | Account override |
| `--protocol` | string | `""` | — | Protocol override |
| `--backend` | string | `""` | — | Backend: `ssh`, `ws` or `auto` |
| `--endpoint` | string | `""` | — | Force an address: `internal` or `external` |
| `--timing` | bool | `false` | — | Print login stage timings |

- **`login` defaults to the external address.** Every other command treats an empty `--endpoint` as "no force". Use `--endpoint internal` to reach the internal address.
- The default backend is **ws**. `--backend auto` first tries the backend remembered for the selected endpoint in `state.json`.
- The session is **not pooled**; it is bound to this process and ends with it. `Ctrl+C` cancels the command and ends the session.
- `--timing` prints `Timing credentials:`, `Timing endpoint/auth:`, `Timing asset resolve:`, `Timing <backend> connect:` and `Timing to interactive:`.

```console
$ jms login web-01@bastion --endpoint internal
```

## `jms sftp`

```console
$ jms sftp report.csv web-01:/tmp/report.csv
Transferred 20480 bytes in 1.2s
MD5 verified: 9a0364b9e99bb480dd25e1f0284c8555
```

`jms sftp <src> <dst> [flags]` — positional: exactly 2, the source and the destination.
Output: `Transferred N bytes in <duration>` and, when verified, `MD5 verified: <digest>`.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--recursive` | bool | `false` | `-R` | Recurse into directories — **not implemented**, see below |
| `--verify` | bool | `false` | — | Verify with md5 (already the default) |
| `--no-verify` | bool | `false` | — | Skip md5 verification |
| `--parallel` | int | `0` | — | Concurrent chunk streams; `0` means 4 |
| `--chunk` | int | `0` | — | Chunk size in bytes; `0` means 1 MiB |
| `--endpoint` | string | `""` | — | Force an address: `internal` or `external` |
| `--account` | string | `""` | — | Account override |

- A remote spec is `<asset>[@<server>]:<path>`. The **first colon** splits host from path, so the path may contain further colons. An empty remote path is refused.
- The direction is inferred: a remote destination is an upload, a remote source is a download.
- A side is remote when it contains a colon, except a Windows drive path (`C:\…`, `C:/…`), which is always local. Two local paths are an error: `neither argument is a remote path: use <asset>[@<server>]:<path> for the remote side`.
- The transfer runs over the bastion's **KoKo SSH port** (default `2222`) on the address the session was bound to. That port must be reachable on your network path.
- md5 verification runs `md5sum` on the remote through an SSH exec session, so the account needs SSH exec rights. A digest mismatch fails with `md5 verification failed`.
- `--verify` and `--no-verify` together are refused: `--verify and --no-verify are mutually exclusive`. `--verify` alone is a no-op, verification is on by default.

```console
$ jms sftp web-01:/var/log/app.log ./app.log --no-verify
$ jms sftp report.csv web-01:/tmp/report.csv --chunk 1048576 --parallel 8
```

## `jms ssh-pipe`

```console
$ jms ssh-pipe web-01 uname -a
```

`jms ssh-pipe <target> [command...] [flags]` — positional: 1 or more, everything is forwarded verbatim.
The command has **no flags of its own**: flag parsing is disabled, so `jms ssh-pipe --help` does not print help. It fails with `jms ssh-pipe: unrecognized option "--help"` and exit 1.

The bridge understands this grammar:

| Form | Meaning |
|---|---|
| `<asset>[@<server>]` | the target; the last `@` splits |
| `-l <asset> [server]` | rsync's ssh-style form |
| `--config <path>` / `--config=<path>` | the configuration file for the bridge |
| anything else | the remote command, joined with spaces |

Any argument starting with `-` other than `-l` and `--config` is refused.

- **stdout is the protocol channel.** The bridge writes nothing of its own to stdout; every diagnostic goes to stderr as `jms ssh-pipe: <message>`.
- The remote exit code becomes the process exit code.
- The bridge is non-interactive. A server that demands MFA with no stored TOTP secret fails instead of prompting.
- The bridge loads the configuration and resolves server, credentials, asset and an SSH token itself. It does not reuse the CLI session.
- Because flag parsing is disabled, the bridge parses its own `--config` argument. `JMS_CONFIG` is honoured as well.

Errors: `expected '-l <asset> <server>' or '<asset>@<server>' from rsync`, `no remote command in arguments`, `-l needs an asset name`, `--config needs a path`.

```sh
# rsync over the bridge
rsync -av -e "jms ssh-pipe web-01" ./dist/ web-01:/srv/app/
```

## `jms tail`

```console
$ jms tail --last 5
14:02:11  bastion/web-01  ws  [外]  ♻命中  exec  [alice]  -> 0  $ uptime  exit 0  120ms  64B
```

`jms tail [flags]` — no positional arguments. Output: one line per event, oldest first.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--follow` | bool | `false` | `-f` | Keep following |
| `--json` | bool | `false` | — | Emit raw JSONL |
| `--last` | int | `0` | — | Start with the last N events |

- `--last N` keeps only the newest N of the events read at startup.
- The follow loop polls every 250 ms. It is a poll, not a filesystem watch, so it behaves the same on every platform.
- `Ctrl+C` ends the follow cleanly and the command exits 0.
- Several MCP clients writing at once produce several files; the files are merged by timestamp.
- `--json` prints the raw JSONL line instead of the rendered one.

## `jms attach`

```console
$ jms attach --last 10
```

`jms attach [flags]` — no positional arguments. Output: one line per event.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--asset` | string | `""` | — | Only show this asset |
| `--readonly` | bool | `false` | — | Observe only |
| `--last` | int | `0` | — | Replay the last N events first |
| `--attach` | bool | `false` | — | Enable the operator prompt (off by default) |

- **Observation is the default.** A plain `jms attach` never accepts input. `--attach` is required to enable the operator prompt.
- `--readonly` wins over `--attach`: the operator role is active only when `--attach` is set **and** `--readonly` is not.
- Operator commands share the host's terminal pool, so a human command and an AI command serialise on the same connection instead of racing for it.
- The client connects to the IPC endpoint of the **configuration directory**: a unix socket `<config dir>/jms-<hash>.sock` on Unix, a named pipe `\\.\pipe\jms-<hash>` on Windows.
- It blocks until Ctrl-C. Cancellation is a normal end, not a failure, so the exit code is 0.
- Events are rendered like `jms tail`. An event that cannot be parsed is printed raw rather than dropped.

## `jms log show`

```console
$ jms log show --since 1h --failed
```

`jms log show [flags]` — no positional arguments. Output: one line per event, oldest first.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--since` | string | `""` | — | Only events newer than this (e.g. `1h`, `30m`) |
| `--asset` | string | `""` | — | Only this asset |
| `--failed` | bool | `false` | — | Only failed commands |
| `--export` | string | `""` | — | Export format: `md` |

- `--since` takes a **Go duration**. A bare number is refused: `--since "2" is not a duration: use a form like 1h or 30m`.
- `--failed` keeps events that carry an error or a non-zero exit code.
- `--export md` prints a Markdown table with the header `| Time | Server | Asset | Command | Exit | Duration | Actor |`. `markdown` is accepted too. Any other value gives `unsupported export format "x": use md`.
- With no matching events the default form prints `No audit events matched.`
- It reads the local audit files. No host has to be running.

## `jms log prune`

```console
$ jms log prune --keep 30 --dry-run
jms: audit prune in /home/you/.config/jms/audit: would delete 3 file(s), 245760 bytes; kept 12 within the window (cutoff 20260911); skipped 0
```

`jms log prune [flags]` — no positional arguments. Output: `jms: audit prune in <dir>: <summary>`.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--keep` | int | `0` | — | Keep this many days (overrides the configured window) |
| `--dry-run` | bool | `false` | — | Report what would be deleted without deleting |
| `--dir` | string | `""` | — | Audit directory (default: the configured one) |

- The window is `--keep`, else `JMS_AUDIT_KEEP_DAYS`, else `audit_keep_days` in the configuration file.
- With no window configured the command refuses: `no retention window configured: pass --keep <days>, set JMS_AUDIT_KEEP_DAYS, or add audit_keep_days to the configuration file`.
- The age is read from the **file name** (`audit-YYYYMMDD-<pid>.jsonl`), not from the modification time.
- A file whose name carries no date is **skipped, not deleted**.

## `jms api get`

```console
$ jms api get /api/v1/assets/hosts/ -q limit=3 --raw
```

`jms api get <path> [server] [flags]` — positional: the path plus an optional server alias (1 or 2 in total).
Output: the JSON body, indented with two spaces by default.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--endpoint` | string | `""` | — | Force an address: `internal` or `external` |
| `--query` | stringArray | `nil` | `-q` | Query parameter `key=value` (repeatable) |
| `--raw` | bool | `false` | — | Print the response without indenting |
| `--max-bytes` | int | `16384` | — | Truncate the printed body to this many bytes (`0` = no limit) |

- The path must start with `/api/v1/`, must end with a trailing slash, must not contain `..` or `//`, must not contain `?` or `#`, must not be an absolute URL, and must be at most 512 bytes.
- It must fall under one of the allowed prefixes:

  ```
  /api/v1/audits/
  /api/v1/assets/
  /api/v1/accounts/
  /api/v1/labels/
  /api/v1/notifications/
  /api/v1/ops/
  /api/v1/orgs/
  /api/v1/perms/
  /api/v1/rbac/
  /api/v1/settings/
  /api/v1/terminal/
  /api/v1/tickets/
  /api/v1/users/
  ```

- Credential-carrying path segments are refused even under an allowed prefix: `authentication`, `auth`, `secret`, `secrets`, `token`, `tokens`, `key`, `keys`, `passphrase`, `password`. The match is exact per segment, so a `password`-change-log path under the audit prefix stays readable.
- `-q` must be `key=value`; anything else gives `-q "<pair>" is not key=value`.
- Truncation prints `<body>` followed by `… truncated at <n> of <total> bytes (use --max-bytes 0 to print all)`. An empty body prints `No content.`
- **Every call is audited.** A successful call is an `api.read` event with the status and the body size; a refused path is an `api.denied` event carrying the reason. Query values that look like credentials are masked before the record is written.
- `jms api` is deliberately not mirrored into the MCP surface.

## `jms mcp`

```console
$ jms mcp --print-config
{
  "mcpServers": {
    "jms": {
      "command": "jms",
      "args": ["mcp"]
    }
  }
}
```

`jms mcp [flags]` — no positional arguments. stdout is the MCP protocol channel, and nothing else.

Flags: `--print-config` (bool, default `false`) prints client configuration snippets; it contains no credential.

- `--print-config` prints the snippet above and exits 0.
- Diagnostics go to stderr. stdout is reserved for the protocol.
- The process also hosts the local observation channel, so `jms attach` clients see the same pool and the same event stream.
- A host that cannot listen is a warning, not a failure: `Warning: observation channel unavailable: <err>`.
- The audit actor is retitled to `mcp`, or to `JMS_ACTOR` when that variable is set.
- There are **no per-call flags**. The address and the backend are steered by the environment: `JMS_MCP_ENDPOINT` (`internal` or `external`) and `JMS_MCP_BACKEND` (`ssh`, `ws` or `auto`). With neither set, the backend remembered for the selected endpoint in `state.json` is used, and `ws` is the last resort.
- A connection failure re-selects the address and retries once instead of requiring a restart.

The MCP tool schemas are documented in [mcp.md](mcp.md).

## `jms config`

`jms config` is a group. Running it without a subcommand prints the help and exits 0.

### `jms config add`

```console
$ jms config add bastion
Internal URL (http(s)://..., empty to skip): http://10.0.0.10
External URL (http(s)://..., empty to skip): https://jumpserver.example.com
Username: alice
Password:
Confirm Password:
TOTP secret (base32, empty to enter the code each time):
Server "bastion" saved to /home/you/.config/jms/config.toml
```

`jms config add <alias> [flags]` — positional: the alias, exactly 1.
Output: `Server "<alias>" saved to <path>`, plus `Server "<alias>" is now the default.` with `--set-default`.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--set-default` | bool | `false` | — | Set as the default server |

| # | Prompt | Kind |
|---|---|---|
| 1 | `Internal URL (http(s)://..., empty to skip): ` | visible, may be empty |
| 2 | `External URL (http(s)://..., empty to skip): ` | visible, may be empty |
| 3 | `Username: ` | visible, required |
| 4 | `Password: ` | hidden |
| 5 | `Confirm Password: ` | hidden, must match #4 |
| 6 | `TOTP secret (base32, empty to enter the code each time): ` | hidden, optional |

- The command is **fully interactive**. There is no flag for the URL, the username, the password or the TOTP secret. When stdin is not a terminal the hidden prompts read a plain line, so piped input works.
- At least one of the two addresses is required, and every non-empty address must be an absolute `http(s)` URL.
- The password is validated against a reachable address **before anything is written**, so a rejected password leaves no trace on disk.
- The first server added becomes the default.
- A TOTP secret that is not usable base32 is a warning, not a failure: it is stored anyway.
- The credential goes to the OS credential store first and the configuration file second; a failed file write rolls the credential back.
- The file is written atomically with mode `0600` on Unix, and the parent directory with `0700`.
- The password never goes into the configuration file. It lives in the OS credential store, or in `JMS_PASSWORD_<ALIAS>` where the alias is uppercased and every `-` becomes `_` (`JMS_PASSWORD_BASTION`). The environment variable wins over the credential store, which is the way to run on a headless machine.

### `jms config list`

```console
$ jms config list
Alias     Internal           External                  Username  Creds    Default
bastion   http://10.0.0.10    https://jumpserver...     alice     keyring  *
```

`jms config list [flags]` — no positional arguments, no flags.
Output: table `Alias`, `Internal`, `External`, `Username`, `Creds`, `Default`, trailer `Total: N server(s)`. Empty cells render as `-`, and the `Default` column is `*` for the default server.

The `Creds` column has exactly four values:

| Value | Meaning |
|---|---|
| `env` | a `JMS_PASSWORD_<ALIAS>` variable is set |
| `keyring` | the OS credential store holds the password |
| `missing` | no password is stored |
| `locked` | the store holds it but cannot be read unattended |

With no servers configured the command fails: `no servers configured: run 'jms config add <alias>'`.
Two warnings go to stderr, one per server with a problem: a missing password and a locked credential store.

### `jms config remove`

```console
$ jms config remove bastion
Remove server "bastion"? [y/N]: y
Server "bastion" removed (/home/you/.config/jms/config.toml)
```

`jms config remove <alias> [flags]` — positional: the alias, exactly 1. Output: `Server "<alias>" removed (<path>)`.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--yes` | bool | `false` | `-y` | Do not ask for confirmation |

- Without `--yes` it asks `Remove server "<alias>"?`. Declining gives `aborted by user` and exit 1.
- Removing the **last** server deletes the configuration file outright and prints `Server "<alias>" removed (<path>) - no servers remain`.
- If the credential cleanup fails, the error names both facts: the server was removed from the file, but its stored credentials could not be deleted.

### `jms config set-default`

```console
$ jms config set-default bastion
Server "bastion" is now the default (/home/you/.config/jms/config.toml)
```

`jms config set-default <alias> [flags]` — positional: the alias, exactly 1. No flags.
Output: `Server "<alias>" is now the default (<path>)`. An unknown alias gives `server not found`.

### `jms config test`

```console
$ jms config test
Server: bastion (default)

Endpoint   URL                                Reachable  Probe  Login
internal   http://10.0.0.10                   yes        3ms    ok
external   https://jumpserver.example.com     yes        180ms  ok

Credentials: keyring
```

`jms config test [alias] [flags]` — positional: an optional alias, at most 1. No flags.
Output: `Server: <alias> (default)`, then the table `Endpoint`, `URL`, `Reachable`, `Probe`, `Login`, then `Credentials: <source>`.

- Every configured address is probed for TCP reachability with a **1.5 second** timeout, and the probe follows the server's proxy policy.
- **Every** reachable address gets a login attempt, not only the first one. The first error is kept and reported.
- The `Login` cell is `ok`, `auth failed`, `unreachable` or `failed`. An unreachable row shows `skipped`. The `Probe` cell is the probe duration in milliseconds.
- A server with no address gives `server "<name>" has no address configured: run 'jms config add <name>'`.
- The failure wording separates the two causes:
  - rejected credentials → `the server "<name>" rejected the stored credentials for "<url>": <err>`
  - anything else → `login to "<url>" failed for server "<name>" (network or server error, not a credential problem): <err>`
  - nothing reachable → `all addresses unreachable: server "<name>" is unreachable on every configured address (<detail>)`

## `jms asset`

`jms asset` is a group. Running it without a subcommand prints the help and exits 0.
Every `asset` subcommand takes an optional trailing `[server]` positional and an `--endpoint` flag, and prints the chosen address to stderr.

### `jms asset list`

```console
$ jms asset list
Name      Address         Platform  Comment
web-01    192.168.10.11   Linux     production
```

`jms asset list [server] [flags]` — positional: an optional server alias, at most 1. Flags: `--endpoint` only.
Output: table `Name`, `Address`, `Platform`, `Comment`, trailer `Total: N asset(s)`; `No assets found.` when empty.

This lists **every** asset on the bastion, including assets that are not granted to you. It is not filtered by your permissions, and it has no `--limit` or `--search` flag.

### `jms asset show`

```console
$ jms asset show web-01
ID:       4b1c…
Name:     web-01
Address:  192.168.10.11
Platform: Linux
Comment:  production
Nodes:    Default
Accounts: root(web)
```

`jms asset show <name-or-id> [server] [flags]` — positional: the asset name or id plus an optional server alias (1 or 2 in total). Flags: `--endpoint` only.
Output: a `key: value` block, not a table.

The block is, in order: `ID`, `Name`, `Address`, `Platform`, `Comment`, `Nodes`, `Accounts`. Empty values render as `-`, and accounts render as `name(username)`.
An id is tried first, then an exact name match.

### `jms asset create`

```console
$ jms asset create web-02 192.168.10.12 --account ops --username ops --password '...'
Created web-02 (id 7f3a…)
Note: a new asset is not connectable until it is granted to your account;
      pass --grant-me, or run `jms asset grant <name> --self`.
```

`jms asset create <name> <address> [server] [flags]` — positional: the name, the address, plus an optional server alias (2 or 3 in total).
Output: `Created <name> (id <id>)`.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--endpoint` | string | `""` | — | Force an address: `internal` or `external` |
| `--account` | string | `""` | — | Account display name to attach |
| `--username` | string | `""` | — | Login name on the target host (defaults to `--account`) |
| `--password` | string | `""` | — | Account password (sent in the request, not stored) |
| `--node` | string | `""` | — | Asset node id (defaults to the default node) |
| `--platform` | int | `0` | — | Platform id (`0` means Linux) |
| `--comment` | string | `""` | — | Free-text note |
| `--protocol` | strings | `nil` | — | Protocols to enable (default `ssh,sftp`) |
| `--grant-me` | bool | `false` | — | Also grant the current user access |

- `--platform 0` is translated to the built-in Linux platform id `1`.
- `--protocol` is a comma-separated list. When empty, the account gets `ssh` and `sftp`, both on port 22.
- `--username` falls back to `--account` when empty. `--node` empty means the server's default node.
- The account password is sent in the create request and never written locally.
- A name clash returns `asset name already exists`.
- Without `--grant-me` the command prints the "not connectable" note on stderr. With `--grant-me` it creates a permission named `grant-<asset-name>` and prints `Granted to the current user via permission <name>`.

### `jms asset update`

```console
$ jms asset update web-01 --comment "production, eu-west"
Updated web-01 (id 4b1c…)
```

`jms asset update <name-or-id> [server] [flags]` — positional: the asset name or id plus an optional server alias (1 or 2 in total).
Output: `Updated <name> (id <id>)`.

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `--endpoint` | string | `""` | Force an address |
| `--name` | string | `""` | New asset name |
| `--comment` | string | `""` | New comment |
| `--address` | string | `""` | New address |


Only the flags you actually set are sent. An unset flag is never written as an empty value, so `--comment ""` is what clears a comment — omitting the flag leaves it alone. The platform id and the node cannot be changed by this command.

### `jms asset delete`

```console
$ jms asset delete web-02
About to delete asset "web-02" (id 7f3a…, address 192.168.10.12).
Delete "web-02"? [y/N]: y
Deleted web-02 (id 7f3a…)
```

`jms asset delete <name-or-id> [server] [flags]` — positional: the asset name or id plus an optional server alias (1 or 2 in total).
Output: `Deleted <name> (id <id>)`.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--endpoint` | string | `""` | — | Force an address |
| `--yes` | bool | `false` | `-y` | Do not ask for confirmation |

- Without `--yes` the command prints the "About to delete" line on stdout and then asks `Delete "<name>"?`. The prompt is `<prompt> [y/N]: ` and **anything other than `y` or `yes` means no**.
- Declining is an error: `not confirmed; nothing was deleted`, exit 1.
- It deletes **only the asset**. Accounts, nodes and permissions that referenced it are left in place; a permission pointing at a deleted asset stays in `jms asset permissions` until you revoke it.

### `jms asset grant`

```console
$ jms asset grant web-02 --user alice --group ops --protocol ssh
Granted web-02 to 1 user(s) and 1 user group(s) via permission grant-web-02 (id 91cd…)
```

`jms asset grant <asset-name-or-id> [server] [flags]` — positional: the asset name or id plus an optional server alias (1 or 2 in total).
Output: `Granted <asset> to <N> user(s) and <M> user group(s) via permission <name> (id <id>)`.

| Flag | Type | Default | Short | Meaning |
|---|---|---|---|---|
| `--endpoint` | string | `""` | — | Force an address |
| `--self` | bool | `false` | — | Also grant the authenticated user |
| `--user` | strings | `nil` | — | User id or exact username to grant (repeatable) |
| `--group` | strings | `nil` | — | User group id or exact name to grant (repeatable) |
| `--account` | strings | `nil` | — | Accounts to allow (default `@ALL`) |
| `--protocol` | strings | `nil` | — | Protocols to allow (default all) |
| `--action` | strings | `nil` | — | Actions to allow (default `connect`) |
| `--name` | string | `""` | — | Permission name (default `grant-<asset>`) |
| `--comment` | string | `""` | — | Free-text note |

- `--user` and `--group` take a list: repeat the flag or comma-separate the values.
- A principal reference is resolved as follows: a UUID-shaped value is tried as an id first, and a 404 falls through to the name path; anything else is filtered server-side by exact `username` (users) or exact `name` (groups). No match gives `no user matches "<ref>"; pass an id or an exact name`, several matches give `"<ref>" matches N users; pass the id instead`. A blank reference is an error, never skipped.
- **The authenticated user is the default principal.** With neither `--user` nor `--group` the current user is granted. `--self` is additive on top of an explicit list.
- The permission name defaults to `grant-<asset-name>`.
- Server-side defaults apply when the flags are empty: accounts `@ALL`, protocols all, actions `connect`.
- At least one user or group is required.

### `jms asset permissions`

```console
$ jms asset permissions
Name          Users   Groups  Assets   Nodes  Protocols  Actions
grant-web-01  alice   ops     web-01   -      all        connect

Total: 1 permission(s)
```

`jms asset permissions [asset-name-or-id] [server] [flags]` — positional: an optional asset name or id plus an optional server alias (0 to 2 in total). Flags: `--endpoint` only.
Output: table `Name`, `Users`, `Groups`, `Assets`, `Nodes`, `Protocols`, `Actions`, trailer `Total: N permission(s)`; `No permissions found.` when empty.

Without an asset argument it lists **all** permissions; with one it lists the permissions that reference that asset. The `Users`, `Groups`, `Assets` and `Nodes` cells hold names joined by commas, not counts. Rows whose relation counts are not expanded yet are re-read through the detail endpoint, so group-only and node-only rules are not skipped. Empty cells render as `-`.

### `jms asset revoke`

```console
$ jms asset revoke grant-web-02
Revoke permission "grant-web-02"? [y/N]: y
Revoked grant-web-02 (id 91cd…)
```

`jms asset revoke <permission-name-or-id> [server] [flags]` — positional: the permission name or id plus an optional server alias (1 or 2 in total).
Output: `Revoked <name> (id <id>)`.

Flags: `--endpoint` (string, default `""`) forces an address; `--yes`/`-y` (bool, default `false`) skips the confirmation.

The target is matched against the permission id or exact name. No match gives `no permission matches "<target>"`. Without `--yes` the command asks `Revoke permission "<name>"?`; declining gives `not confirmed; nothing was revoked` and exit 1. Only the authorization goes away — the asset stays.

## `jms completion`

`jms completion` is a cobra built-in. It generates the completion script for one shell: `bash`, `fish`, `powershell` or `zsh`.

Flags: `--no-descriptions` (bool, default `false`) disables completion descriptions.

```console
$ jms completion bash --help
  source <(jms completion bash)
  jms completion bash > /etc/bash_completion.d/jms        # Linux
  jms completion bash > $(brew --prefix)/etc/bash_completion.d/jms   # macOS
```

## Observability

Three commands read the same local audit stream. The stream is a set of per-process JSONL files under the audit directory (`<config dir>/audit`, or `JMS_AUDIT_DIR`), named `audit-YYYYMMDD-<pid>.jsonl`.

| Command | Reads | Live | Machine-readable |
|---|---|---|---|
| `jms tail` | the files | with `-f` | with `--json` |
| `jms attach` | the IPC channel of a running host | yes | no |
| `jms log show` | the files | no | with `--export md` |

One event renders as a single line:

```
HH:MM:SS  <server>/<asset>  <backend>  [<endpoint badge>]  [<pool badge>]  <kind>  [<actor>]  <path>  -> <status>  $ <command>  exit <n>  <n>ms  <n>B  ! <error>
```

| Badge | Values |
|---|---|
| endpoint | `[内]` for internal, `[外]` for external |
| pool | `⚡冷连` for a cold connect, `♻命中` for a pool hit |

- `jms tail` works whether or not a host is running, because it reads the files.
- `jms attach` needs a running host: it attaches to the IPC endpoint of the configuration directory and can join the same pooled connections.
- `jms log show` is for looking back over a window; `jms tail -f` is for watching.

The audit writer itself is steered by `JMS_AUDIT` (`off` disables it), `JMS_AUDIT_DIR`, `JMS_AUDIT_FULL` and `JMS_AUDIT_PREVIEW_BYTES`. The retention window is `JMS_AUDIT_KEEP_DAYS` or `audit_keep_days` in the configuration file. See [configuration.md](configuration.md).

## Gotchas

| Gotcha | What happens |
|---|---|
| `login` picks the external address | An empty `--endpoint` means "no force" everywhere **except** `login`, which defaults to `external`. Pass `--endpoint internal` to go the other way. |
| `exec` has a 30 second ceiling on the WebSocket backend | `--timeout 0` means "no timeout asked for", and the ws transport then applies its own 30 s deadline. The SSH backend has no timer of its own. The MCP server bounds the same call at 5 minutes. |
| `ls --search` ignores `--limit` | A keyword switches to the server-side search, which returns everything that matches. |
| `sftp` needs the KoKo SSH port | The transfer and the md5 check run over the bastion's SSH port (default `2222`). Many deployments expose only the web port. |
| `sftp` has no `-R` and no relay | Both fail with `requires a transfer seam that carries two resolved sessions (relay) or a directory walk (-R)`. |
| `asset delete` leaves permissions behind | Only the asset is deleted. Revoke the permission yourself. |
| `asset update` never clears a field by accident | Only flags you set are sent. |
| `asset grant` grants you by default | With no `--user` and no `--group` the authenticated user is the principal. |
| `ssh-pipe` has no help | Flag parsing is disabled; `--help` is an error. |

## Limitations

- `jms sftp` cannot copy between two assets, and cannot recurse into directories.
- `jms api get` is read-only and limited to the allowlist above. There is no write counterpart in the CLI.
- `jms attach --attach` was verified by reading the source only; no running host was available to exercise the operator prompt.
- `jms exec`'s stdout purity is verified from the code; the exact bytes a real server sends were not observed in this worktree.
- Windows-specific IPC and path behaviour was read from source and not executed on Windows.
- `jms ssh-pipe` is non-interactive and fails instead of prompting when the server demands MFA.
