# Troubleshooting

What each error means, what causes it, and the shortest fix.
The [README](../README.md) has the short version.
Siblings: [简体中文](troubleshooting.zh-CN.md) · [Command reference](command-reference.md) · [MCP server](mcp.md) · [Configuration](configuration.md) · [Addresses and proxies](networking.md).

## The 60-second triage

Run these four in order. The first one that fails tells you which section below to read.

| # | Command | What it tells you |
|---|---|---|
| 1 | `jms config test` | Whether each address answers, and whether the login works. Prints one row per address. |
| 2 | `jms ls` | Whether the credential and the API work end to end. |
| 3 | `jms exec web-01 true --timing` | Which stage is slow: credentials, endpoint/auth, or the command itself. |
| 4 | `jms tail --last 20` | What ran recently, from which surface, and with what result. |

`jms config test` is the fastest single check. It probes **every** configured address with a 1.5 s TCP timeout and gives **every reachable** address a login attempt, so a credential problem is reported separately from an unreachable network:

```console
$ jms config test
Server: bastion (default)

Endpoint  URL                  Reachable  Probe  Login
internal  http://10.0.0.5:2280/  yes        0ms    ok
external  https://bastion.example.com/  yes  7ms   auth failed

Credentials: keyring
```

The `Login` cell is `ok`, `auth failed`, `unreachable`, `failed` or `skipped`. The `Credentials:` line reads `env`, `keyring` or `missing`.

## Cannot reach the bastion

### `no reachable endpoint: server "bastion": internal: probe failed; …`

No candidate address answered the TCP connect.

Cause: the address in the config is not the one your current network reaches, the VPN is not connected, or the port is wrong.

Fix:

```sh
jms config test
```

Read the `Reachable` column per address. Then check the address and port in `config.toml`, and connect the VPN if the bastion needs one.

### `no reachable endpoint: server "bastion" has no address configured`

The server has neither `internal` nor `external` in the config file.

Fix: `jms config add bastion`.

### `server "bastion" has no external address configured; refusing to fall back to the other address`

You forced an address the server does not have. `--endpoint external` (or `JMS_MCP_ENDPOINT=external`) never falls back silently.

Fix: use the address the server has, or add the missing one:

```sh
jms ls --endpoint internal
```

### `no backend connected (ssh: …; ws: …)`

Both transport backends failed for the address that was selected. The two parts are the `ssh` and `ws` attempts.

Fix: try the other backend explicitly, then check the network:

```sh
jms exec web-01 hostname --backend ws
jms exec web-01 hostname --backend ssh
```

A working `ws` with a failing `ssh` almost always means KoKo's SSH port is closed. See [SFTP and SSH-port failures](#sftp-and-ssh-port-failures).

### `handshake: <url> returned HTTP 302`

The address answered the upgrade request with a redirect instead of a terminal. The address is probably the wrong kind for this path.

Fix: try the other address, and check that the URL is the bastion's own address rather than a proxy or a portal in front of it.

## Login and credential failures

### `no stored password for server "bastion": run `jms config add bastion` or set JMS_PASSWORD_BASTION`

No credential is stored for that alias, in the environment or in the OS credential store.

Fix:

```sh
jms config add bastion
```

Or set the environment variable, which is read-only and always wins over the store:

```sh
export JMS_PASSWORD_BASTION='…'
```

The alias is uppercased and every `-` becomes `_`, so the alias `my-wan` reads `JMS_PASSWORD_MY_WAN`. A blank value counts as unset, not as an empty credential.

### `the credential store is locked — unlock the keyring, or set JMS_PASSWORD_BASTION`

The OS credential store exists but cannot be read unattended. On Linux the Secret Service collection is locked; on Windows and macOS the OS call did not answer within 10 s.

Fix: unlock the keyring from the desktop session, or bypass the store entirely with the environment variable above. The message also names the variable to set.

`jms config list` shows the same condition in the `Creds` column and prints a warning:

```console
$ jms config list
Alias    Internal             External  Username  Creds    Default
bastion  http://10.0.0.5:2280/  -       ops       locked   *

Warning: the credential store is locked — unlock it, or set JMS_PASSWORD_BASTION, to use server "bastion"
```

`locked` is not the same as `missing`. `missing` means no credential exists; `locked` means one is there and the store cannot be opened.

### `cannot read the password for "bastion": …`

The store failed for a reason other than "locked" or "absent" — a D-Bus error, a missing Secret Service, or a timeout.

Fix: check that the session bus is reachable, and fall back to the environment variable:

```sh
export JMS_PASSWORD_BASTION='…'
```

### `the server "bastion" rejected the stored credentials for "bastion": …`

The server saw the credential and refused it (HTTP 400, 401, 403 or 422). This is terminal: no failover happens, by design, because the other address would refuse it too.

Fix: re-enter the credential with `jms config add bastion`. If MFA is on, also store the TOTP secret at the prompt.

### `MFA required but no TOTP secret configured`

The account needs a second factor and no TOTP secret is stored.

Fix: re-run `jms config add bastion` and answer the TOTP prompt, or set the secret in the environment:

```sh
export JMS_OTP_BASTION='<base32 secret>'
```

### `form login returned no session cookie`

The login form was accepted but the response carried no session cookie. Usually the address points at something that is not a JumpServer web login, or a reverse proxy rewrote the response.

Fix: `jms config test` and compare the address against the one in the web console's address bar.

### `the passwords do not match`

The two hidden prompts in `jms config add` did not match. Nothing was stored.

Fix: run `jms config add bastion` again and retype both entries.

## Command timeouts and marker problems

### `websocket: command did not complete within 30s — either it is still running, or it ends in '#' or '\' (or has unbalanced quotes) and swallowed the status marker: …`

The WebSocket backend did not see the completion marker within the timeout. Two different causes produce the same message:

| Cause | Check | Fix |
|---|---|---|
| The command really is still running. | The command is slow on the host. | Raise the timeout: `jms exec web-01 '…' -t 300`. |
| The command ends in `#` or `\`, or has unbalanced quotes, so the status marker was swallowed. | The remote command's own quoting. | Rewrite it so it terminates normally. |

The SSH backend reports the same condition as `command timed out after 30s: …`.

The timeout is the only defence against a swallowed marker: without it the client waits forever.

### `Command exited with status 1`

Not a client failure. The remote command ran and returned a non-zero status; the line goes to stderr and the same number becomes the exit code of `jms exec`.

### `remote command exited with status 1`

The same condition reported as an error instead of a diagnostic. It appears when the command is run through `jms ssh-pipe`.

## Slow connections

Use `--timing` to attribute the delay. It works on `jms exec` and `jms login` and prints one line per stage to stderr:

```console
$ jms exec web-01 hostname --timing
Timing credentials: 12ms
Timing endpoint/auth: 210ms
Timing command: 340ms
```

| Stage | Slow means |
|---|---|
| `Timing credentials:` | The OS credential store is slow to answer. On Linux the Secret Service call is bounded at 5 s; on Windows and macOS at 10 s. |
| `Timing endpoint/auth:` | The probe (1.5 s budget) plus the login. The API client retries transport errors 3 times with a 0.5 s / 1 s / 2 s backoff, and each request has a 15 s timeout. |
| `Timing command:` | The remote command itself. |

`jms login --timing` adds two more lines: `Timing asset resolve:` and `Timing <backend> connect:`.

Pool behaviour that shows up as latency:

| Behaviour | Value |
|---|---|
| Terminal idle time before it is reaped | 10 minutes |
| Asset resolution cache lifetime | 5 minutes |
| Reaper scan period | 60 seconds |
| A failed command is replayed | only when it produced **no output** |

A command that already printed output is never replayed, so a slow command is never run twice.

## SFTP and SSH-port failures

### `xfer: sftp connection token for <asset>: …` or `xfer: dial <host>:2222: …`

SFTP rides on KoKo's SSH port, which defaults to `2222` and is configured per server as `ssh_port`. The WebSocket backend does not use it, so `exec` can work while SFTP fails.

Fix: check that the port is open, or set it in the config file:

```toml
[servers.bastion]
internal = "http://10.0.0.5:2280/"
username = "ops"
ssh_port = 2222
```

### `please select one of the assets`

The connection token was issued for the wrong protocol. SFTP needs a token with `connect_method=web_sftp`; an `ssh` or `web_cli` token lands on the KoKo virtual root instead.

Fix: this is a server-side token configuration issue. Ask the bastion administrator to issue an SFTP-capable token.

### `md5 verification failed: local <hash>, remote <hash>`

The file was transferred but the digests differ. The verification runs `md5sum` on the remote host over an SSH exec terminal, so it can also fail because that command is missing or the SSH port is closed.

Fix: retry with `--no-verify`, and check the remote host:

```sh
jms sftp ./file web-01:/tmp/file --no-verify
```

### `recursive directory transfer (-R): requires a transfer seam that carries two resolved sessions (relay) or a directory walk (-R)`

`-R` and asset-to-asset relay are not implemented at the CLI seam. Both return this error.

Fix: transfer files one at a time. The MCP surface has `jms_sftp_relay` for asset-to-asset streaming.

### `neither argument is a remote path: use <asset>[@<server>]:<path> for the remote side`

Both sides of `jms sftp` were local paths. The direction is inferred from which side is remote.

Fix: use `cp` for a local-to-local copy, or name the remote side:

```sh
jms sftp ./file web-01:/tmp/file
```

## MCP client problems

### The AI client cannot see the configuration

`jms mcp --config <path>` does **not** reach the MCP server. The `--config` flag is resolved by the CLI's own config lookup; the MCP server is handed a different path. Steer it with the environment variable or the per-tool argument instead:

```sh
JMS_CONFIG=/path/to/config.toml jms mcp
```

Every tool also accepts a `config_path` argument, which wins over the environment variable for that one call.

### The AI client cannot see the credential

The MCP process may run as a different user, in a different session, or with a different environment than your shell. It then has no keyring and no `JMS_PASSWORD_*`.

Fix: pass the environment explicitly in the MCP client entry:

```json
{
  "mcpServers": {
    "jms": {
      "command": "jms",
      "args": ["mcp"],
      "env": {
        "JMS_CONFIG": "/home/ops/.config/jms/config.toml",
        "JMS_PASSWORD_BASTION": "…"
      }
    }
  }
}
```

`jms mcp --print-config` prints the same snippet without any credential.

### `ERROR: JMS_MCP_ENDPOINT="bogus": must be internal or external`

The steering variable has a value the server does not accept. The error reaches the caller on every tool call that needs a session.

Fix: use `internal` or `external`.

### `ERROR: JMS_MCP_EXEC_TIMEOUT="bogus": must be a duration ("90s", "10m") or a positive number of seconds`

The exec-timeout override is malformed. The error reaches the caller on `jms_exec` calls that omit `timeout`.

Fix: use a Go duration (`90s`, `10m`) or a bare number of seconds. The default when unset is 5 minutes.

### The backend override is ignored

`JMS_MCP_BACKEND` accepts `ssh`, `ws` or `auto`. An invalid value is **not** reported: the error is swallowed at the only call site, and the server falls back to the remembered backend for the selected endpoint, or `ws`. Check the value for a typo rather than expecting an error.

### The expected tool is missing

The tool set is decided at startup. `JMS_MCP_TOOLSET=core` registers the 15 core tools only; anything else registers the full management block. `JMS_MCP_WRITE=limited` is required for the two write tools, and `JMS_MCP_WRITE_DOMAINS` narrows them further.

A change to any of these needs a **restart** of the MCP process. Unsetting a variable restores the previous surface without touching disk.

### A tool call reports an error but the command worked in a shell

MCP failures are returned as text: the result body is `ERROR: <message>` and the result is marked as an error. The protocol itself does not fail, so the client shows the text rather than a transport error.

The most common causes are the ones above: a missing environment variable, an unreachable address, or a credential the MCP process cannot read.

### The result is cut off

Management reads and `jms_local_audit` are capped at 32768 bytes. The truncated body ends with:

```
… truncated at 32768 bytes; narrow the filters or lower limit
```

The cut happens in the serialized JSON, so a truncated body is not valid JSON. The 15 core tools are not subject to this cap.

## Audit problems

### `jms tail` shows nothing

The stream is the per-process JSONL files under the audit directory, `<config dir>/audit`. Each process writes its own file, named `audit-YYYYMMDD-<pid>.jsonl`, and several files are merged by timestamp.

Fix: check where the directory is and whether it contains files:

```sh
jms log show --last 20
ls "$(dirname "$(jms config list >/dev/null 2>&1; echo "$XDG_CONFIG_HOME/jms/config.toml")")/audit"
```

`JMS_AUDIT_DIR` relocates the directory for the writer and the readers alike. `JMS_AUDIT=off` disables the sink entirely, so nothing is written at all.

### `--since "2" is not a duration: use a form like 1h or 30m`

`--since` takes a Go duration. A bare number is rejected.

Fix: `--since 2h`, `--since 30m`.

### `unsupported export format "bad": use md`

`--export` accepts `md` (or `markdown`). Nothing else.

### `no retention window configured: pass --keep <days>, set JMS_AUDIT_KEEP_DAYS, or add audit_keep_days to the configuration file`

`jms log prune` refuses to delete anything without a window. The window comes from `--keep`, then `JMS_AUDIT_KEEP_DAYS`, then `audit_keep_days` in the config file.

Fix: pick one. Preview first:

```sh
jms log prune --keep 30 --dry-run
```

### `keep days must be at least 1`

The window is zero or negative. Zero means "never prune" in the config file, but the `--keep` flag rejects it.

### A preview is truncated and `JMS_AUDIT_FULL=1` does not restore it

The MCP layer truncates the preview to 4096 bytes before the event reaches the bus, so the writer's `JMS_AUDIT_FULL` and `JMS_AUDIT_PREVIEW_BYTES` cannot restore text that was already dropped. `JMS_AUDIT_FULL=1` affects previews that reach the writer untruncated.

### `Warning: observation channel unavailable: ipc: /…/jms-<hash>.sock is already in use`

Another `jms mcp` process already owns the observation socket. This is a warning, not a failure: MCP keeps serving, but `jms attach` cannot watch this process.

Fix: stop the other host, or ignore the warning if you do not need `jms attach`.

## Windows, WSL and headless keyrings

| Situation | What happens | Fix |
|---|---|---|
| Linux desktop, keyring unlocked | The credential is read from the Secret Service. | Nothing. |
| Linux desktop, keyring locked | The lock state is probed before any unlock attempt, so the command fails at once with `the credential store is locked — …`. | Unlock the keyring, or set the environment variable. |
| Linux, no graphical session (CI, container, SSH) | The Secret Service answers an unlock request with a prompt nobody can answer. The lock probe still prevents the hang: the failure is immediate, not a freeze. | Set `JMS_PASSWORD_<ALIAS>`. |
| WSL | The Secret Service is usually absent, so the store reports the entry as absent rather than locked. | Set `JMS_PASSWORD_<ALIAS>`. |
| Windows | The credential lives in Windows Credential Manager. The entry name is `jms-client:<alias>/password`. | Nothing on a desktop; set the environment variable in a service context. |
| macOS | The credential lives in the login Keychain. The service is `jms-client`, the account is `<alias>/password`. | Nothing on a desktop. |

Two rules that hold everywhere:

- The environment variable is **read-only**. `jms config add` cannot write to it; it always writes to the OS credential store. If the store is unusable, the environment variable is the only route.
- The variable wins over the store. `jms config list` shows `env` in the `Creds` column when it is set, even if the store also holds a credential.

`DISPLAY`, `WAYLAND_DISPLAY` and `XDG_SESSION_TYPE` only change how the "locked" message is phrased. They never decide whether the store is tried.

## Asset grant and visibility problems

### A new asset is not visible in `jms ls`

A newly created asset is **not connectable** until a permission grants it. `jms ls` lists the assets you are authorized to reach; `jms asset list` lists every asset that exists on the server.

Fix: grant it.

```sh
jms asset grant web-02 --self
```

Or create it with the grant in one step:

```sh
jms asset create web-02 192.168.10.12 --account ops --username ops --grant-me
```

`jms asset create` prints this when it succeeds:

```
Note: a new asset is not connectable until it is granted to your account;
      pass --grant-me, or run `jms asset grant <name> --self`.
```

### `asset created but the grant failed: …`

The asset exists, the grant did not. The asset is listed by `jms asset list` and not by `jms ls`.

Fix: run the grant again.

### `at least one user or user group is required: set UserIDs and/or GroupIDs`

A grant with no principal. Give at least one of `--user` or `--group`.

Fix:

```sh
jms asset grant web-02 --user ops
jms asset grant web-02 --group platform
```

Both accept an id or an **exact** username or group name. A name that matches more than one record is refused rather than guessed.

### `no %s matches %q; pass an id or an exact name`

The user, group, asset or permission name did not resolve. The matcher never guesses.

Fix: list the assets and find the id:

```sh
jms asset list
```

### `jms asset permissions <asset>` does not list a grant you know exists

The matcher compares only the `assets` relation of a permission, never the `nodes` relation. A rule that grants a **node** rather than the asset is therefore not matched, and it does not appear in the per-asset listing even though it covers the asset.

Fix: list every permission without an asset filter:

```sh
jms asset permissions
```

## Diagnostics reference

| Tool | What it does | How to use it |
|---|---|---|
| `jms config test` | Probes every address (1.5 s TCP timeout) and attempts a login on each reachable one. | The first thing to run. The `Login` cell separates a network problem from a credential problem. |
| `jms exec … --timing` | Prints `Timing credentials:`, `Timing endpoint/auth:`, `Timing command:` to stderr. | Attributes a slow run to one stage. |
| `jms login … --timing` | Adds `Timing asset resolve:` and `Timing <backend> connect:`. | The same, for an interactive session. |
| `JMS_WS_DUMP=1` | Dumps every WebSocket frame to stderr as `[wsdump] +<n> bytes: "<payload>"`. | Last resort. It prints payload bytes, so keep the output out of shared logs. |
| `jms tail` / `jms tail -f` | Follows the local audit stream. `-f` polls every 250 ms; `--json` emits raw JSONL; `--last N` replays the last N events. | Watch what an MCP client is actually doing. |
| `jms log show` | Reads the audit log as a file and returns immediately. `--since 1h`, `--asset`, `--failed`, `--export md`. | Looking back over a window. |
| `jms log prune --dry-run` | Reports what the retention window would delete. | Before deleting anything. |
| `jms_pool_status` | MCP debug tool. Reports `terminals:`, `hits:`, `cold:`. | Only registered when `JMS_MCP_DEBUG` is `1`, `true`, `yes` or `on`. |
| `JMS_MCP_DEBUG=1` | Registers the debug-only `jms_pool_status` tool. | Restart the MCP process after setting it. |

## Limitations

- **The SSH backend has not been exercised against a live JumpServer.** Every deployment reached so far had the KoKo SSH entry disabled. The code paths are unit-tested only.
- **`JMS_MCP_BACKEND` errors are swallowed.** The code comment claims an invalid value is reported; the only call site discards the error. The code wins: an invalid value falls back silently.
- **`jms mcp --config` does not reach the MCP server.** Use `JMS_CONFIG` or the per-tool `config_path` argument.
- **`-R` and asset-to-asset relay are not implemented at the CLI seam.** `jms sftp -R` returns an error rather than walking a directory.
- **`jms asset permissions <asset>` misses node-only grants.** The matcher never compares the `nodes` relation.
- **`jms_asset_update` cannot clear a field** — an empty new value is ignored. The CLI can, because it checks whether the flag was changed.
- **`jms asset update` rewrites the asset's protocols** to `ssh/22` and `sftp/22` on every update.
- **Deleting an asset does not revoke the permissions that referenced it.**
- **The MCP write tools were never probed against a live instance.** Their registration, schemas, gating and refusal paths are exercised; no write was sent to a server.
- **`JMS_AUDIT_FULL=1` cannot restore an MCP preview** that was already truncated by the MCP layer.
- **Windows-specific IPC and path behaviour was read from the source only.** It was not executed on Windows.
