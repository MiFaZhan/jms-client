# Command reference

The full description of every subcommand, its flags and its behaviour.
The [README](../README.md) has the short version.


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
