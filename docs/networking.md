# Addresses, failover and proxies

How `jms` picks an address for a server, when it fails over, how it routes traffic and which transport backend it uses.
The [README](../README.md) has the short version.
Siblings: [简体中文](networking.zh-CN.md) · [Command reference](command-reference.md) · [MCP server](mcp.md) · [Configuration](configuration.md) · [Troubleshooting](troubleshooting.md).

## The address model

| Key | Meaning |
|---|---|
| `internal` | The address reachable from inside the network. |
| `external` | The address reachable from outside (the web mapping). |
| `prefer` | Which of the two to try first: `internal` or `external`. Absent means `internal`. |
| `pin` | Restricts the server to one address. No probing of the other, no failover. |
| `ssh_port` | The KoKo SSH port. Absent means `2222`. |

Rules enforced when the file is loaded:

| Rule | Error when violated |
|---|---|
| At least one of `internal` / `external` is required. | `server "bastion": at least one of internal/external URL is required` |
| Every address is an absolute `http://` or `https://` URL with a host. A bare `host:port` is rejected, and so is any other scheme. | `internal URL "127.0.0.1:2280" is not a valid URL: …` · `internal URL "ftp://127.0.0.1:21" must use http:// or https://` |
| `prefer` is `internal` or `external`. | `server "bastion": prefer must be "internal" or "external", got "wan"` |
| `pin` is `internal` or `external`. | `server "bastion": pin must be "internal" or "external", got "wan"` |
| `ssh_port` is `1..65535`. | `server "bastion": ssh_port must be between 1 and 65535, got 70000` |

The scheme must be explicit: it drives the failover policy and the probe port. `prefer` and `pin` are matched case-insensitively and stored lower-cased.

## Candidate ordering

| Priority | Source | Effect |
|---|---|---|
| 1 | `pin` | Exactly **one** candidate, the pinned kind. No probing of the other address, no failover. If the server has no address of that kind, the candidate set is empty and the command fails. |
| 2 | `--endpoint` / `JMS_MCP_ENDPOINT` | Exactly that kind. A forced address never falls back: `server "bastion" has no external address configured; refusing to fall back to the other address`. |
| 3 | Fresh last-good | The address that last worked, when it was recorded less than **10 minutes** ago. |
| 4 | `prefer` | The configured preference. `internal` when unset. |
| 5 | Remaining | Whatever address is left, `internal` before `external`. |

Each kind appears at most once. **`pin` wins over `--endpoint`**: the pin is applied first, so a pinned server never leaves the pinned address even when `--endpoint` names the other one.

Every candidate is probed with a **TCP connect and immediate close**, with a timeout of **1.5 seconds**. The probe port comes from the URL; when the URL has no port it is `443` for `https://` and `80` otherwise. A failed probe only skips that candidate — the probe is an accelerator, not a verdict.

### The last-good address

The address that last worked is written to `state.json` next to `config.toml`, with a timestamp. It is fresh for **10 minutes**.

| Aspect | Behaviour |
|---|---|
| File | `state.json` in the configuration directory. `--config` or `JMS_CONFIG` moves it with the config file. |
| Contents | `{"servers": {"<alias>": {"last_good": "internal", "at": "<RFC3339>", "backend": {"internal": "ws"}}}}` — field names taken from the struct tags; the file was not observed on disk in this worktree. |
| Write mode | Atomic write, `0600`; the directory is created `0700`. A corrupt file is not an error — it costs one extra probe. |
| Read | On **every** selection, never cached: a long-lived MCP host shares `state.json` with short-lived CLI runs. |

The address is bound to the **session**, not to the request. A cached session is reused only when it is bound to the address that was just selected; a mismatch forces a fresh login. That is what stops a long-running host from staying pinned to an abandoned address.

## Failover rules

The selection loop probes a candidate, and on a reachable one attempts the login.

| Outcome | Behaviour |
|---|---|
| Probe fails | Record `"<kind>: probe failed"`, move to the next candidate. |
| Login succeeds | Record the last-good address, stop. |
| Login fails, **network-class** error | Record the error, try the next candidate. |
| Login fails, **terminal** error | Return immediately. **No failover.** |
| Every probe failed | One full login attempt against the first candidate. |
| Everything failed | `no reachable endpoint: server "bastion": <joined failures>` |

| Class | Examples | Failover? |
|---|---|---|
| Terminal | `*api.AuthError` with status **400, 401, 403 or 422** | **No.** The server saw the credential and refused it; the other address would refuse it too. |
| Terminal | MFA not supplied, missing session cookie, cancelled context, expired deadline | **No.** The endpoint answered, or you asked it to stop. |
| Network-class | Transport failure, `api.APIError` with status `0` (TCP reachable, service behind it broken) | Yes. |
| Network-class | 5xx responses | Yes. |

### The single reconnect

A connection-class failure means the session or the remembered address may be stale. The MCP server then **re-selects once and retries once**: the state file is re-read, the candidates are re-ordered and a fresh login is performed. The retry happens **only** when something actually changed — a different address or a different backend — and **only** when the failed attempt produced no output. A command that already emitted output is never sent again, because the remote host has seen it and `rm`/`reboot` are not idempotent. A cancelled call, a command timeout and a credential rejection are never retried.

The connection pool follows the same safety rule for the CLI: a failure that produced no output is replayed once on a freshly built terminal; a failure after any output is not replayed. The CLI does not re-run the endpoint selection on that retry — the address stays the one the session is bound to.

### Pin semantics

`pin` is a promise from the operator: "I know which network I am on". Exactly one candidate is tried, the other address is not probed, and a login failure on the pinned address is reported as-is. A pinned kind the server does not have yields an empty candidate set: `no reachable endpoint: server "bastion" has no address configured`.

`pin` is the right tool when a silent fallback would hide a broken route. `prefer` is the right tool when you only want to change the order.

## Proxy policy

**The client connects directly by default and ignores `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY` and `NO_PROXY`.**

This is deliberate. On a developer machine those variables are often exported globally by a transparent-proxy tool, and the tool's own `DIRECT` rules frequently match the bastion address. The request then leaves the process through a proxy nobody asked for, and the proxy's error (typically an empty 502) is reported as if it came from the network. Worse, the TCP probe and the login would then be measuring two different paths. Verified in this worktree: with `HTTP_PROXY`, `https_proxy`, `ALL_PROXY` and `NO_PROXY` all set to bogus values, the request still reached the target directly.

| Value | Result |
|---|---|
| absent or empty | Direct. |
| `direct`, `none`, `off` | Direct. Case-insensitive. |
| `http://…` | HTTP CONNECT proxy. |
| `https://…` | HTTP CONNECT proxy, TLS to the proxy itself. |
| `socks5://…` | SOCKS5 proxy. Host names are sent to the proxy as domain names, so the proxy resolves them. |
| `socks5h://…` | SOCKS5 proxy. Accepted as a separate spelling, but it behaves exactly like `socks5` here. |

Anything else is an **error**, never silently ignored:

```
proxy "ftp://127.0.0.1:21" has unsupported scheme "ftp" (use http, https or socks5)
proxy "example.com" is missing a scheme (use http:// or socks5://)
proxy "http://" is missing a host
```

The global `proxy` key sits at the root of `config.toml`; a per-server `proxy` sits inside `[servers.<alias>]`. Precedence: the server's own value wins when it is not empty; otherwise the global value applies; an empty result means direct. A per-server `proxy = "direct"` therefore **overrides** a global proxy — "inherit the global default" and "force a direct connection" are two different intentions and do not collapse into one spelling.

The policy is resolved once per server and recorded on the session, so every connection derived from that session travels the same route. That covers the REST API, the TCP probe, the KoKo WebSocket, the KoKo SSH connection and the SFTP connection. Two exceptions:

- **`jms ssh-pipe` ignores the configured proxy.** Its bridge logs in and probes with the direct policy (read from the source; not reproduced at runtime). The data connection it opens afterwards is therefore direct as well.
- **`jms sftp` honours the proxy**, and the SFTP connection inherits the session's policy.

`NO_PROXY` is never consulted, not even when a proxy is configured. A configured proxy applies to every destination.

## Backends

The transport backend is how the command reaches the asset once the address is chosen.

| Aspect | `ws` (WebSocket) | `ssh` (KoKo SSH) |
|---|---|---|
| Path | `/koko/ws/terminal/` | TCP to the KoKo SSH port, default `2222` |
| Scheme | `ws` for `http://`, `wss` for `https://` | — |
| Authentication | `jms_sessionid` cookie + the `JMS-KOKO` subprotocol | Connection token: user `JMS-{token_id}`, the token's `value` as the credential — not the password |
| Command channel | One shared PTY, serialized by the pool's per-terminal lock | One exec channel per command |
| Exit status | Parsed from the `__JMSRC:N__` marker | **Native** remote exit code |
| Connect timeout | 15 s | 15 s for the TCP connect, 2 s for the SSH banner/handshake |
| Keepalive | Text-frame `PING` every 30 s | OpenSSH keepalive every 30 s, 3 consecutive misses tolerated |
| Token retries | 2 attempts, fresh token each | 1 attempt |

The WebSocket backend delimits output with a `__JMSDONE_<unix_nanos>__` marker: the text between the **last two** occurrences is the command output. It also merges stdout and stderr on the PTY, then strips ANSI sequences and folds CRLF to `\n`.

With `--timeout 0` (the default) the WebSocket backend applies its own ceiling of **30 seconds** and fails with:

```
websocket: command did not complete within 30s — either it is still running, or it ends in '#' or '\' (or has unbalanced quotes) and swallowed the status marker: command timeout
```

The SSH backend has no timer of its own; only the command context stops it. Pass `-t` for a longer ceiling (`jms exec -t 600 web-01 "./build.sh"`). The MCP `jms_exec` tool defaults to **5 minutes** instead (`JMS_MCP_EXEC_TIMEOUT` overrides it).

`auto` tries **ssh first, then ws**, and returns the first success. Each attempt creates its own connection token. The reason SSH goes first is that an external endpoint often permits the WebSocket path while the KoKo SSH port is blocked; the 2-second banner timeout exists so the fallback to WebSocket stays fast. When both fail, the error is `no backend connected (<failures joined by "; ">)`.

The backend that last worked is remembered per address kind in `state.json`, under `servers.<alias>.backend.<kind>`.

| Surface | Backend choice |
|---|---|
| `jms exec` | `ws`. `--backend` overrides. |
| `jms login` | `ws`. `--backend` overrides; `auto` is replaced by the remembered backend of the selected endpoint. |
| MCP `jms_exec` | `JMS_MCP_BACKEND`, then the remembered backend, then `ws`. |
| Library default | `auto` when nothing is set. |

An explicit `JMS_MCP_BACKEND` is not second-guessed. When the remembered backend has already failed for a call, the other concrete backend is substituted; `auto` is returned unchanged because it already means "try both". A pooled terminal keeps the backend it was built with: the backend is not part of the pool key.

`jms sftp` rides on KoKo's SSH port (`ssh_port`, default `2222`) and needs it reachable. The connection token must be created with `protocol=sftp` and `connect_method=web_sftp`; a token created for `ssh` or `web_cli` lands on the KoKo virtual root and every path fails with "please select one of the assets". The md5 verification runs `md5sum` on the remote host over an SSH exec terminal, so it needs the same port. The transfer itself uses 1 MiB chunks over 4 parallel streams.

## Worked decision table

| Configuration | `--endpoint` | Candidates tried | Failover? |
|---|---|---|---|
| `pin = "internal"` | unset | `internal` | No |
| `pin = "internal"` | `external` | `internal` (the pin wins) | No |
| `pin = "external"` | unset or `internal` | `external` | No |
| `pin` to a kind the server lacks | any | none → `no reachable endpoint` | n/a |
| no pin, `prefer = "internal"` or absent | unset | last-good (if fresh) → `internal` → `external` | Yes |
| no pin, `prefer = "external"` | unset | last-good (if fresh) → `external` → `internal` | Yes |
| no pin | `internal` | `internal` | No |
| no pin | `external` | `external` | No |

`jms login` forces `external` when `--endpoint` is absent; pass `--endpoint internal` for an internal-only bastion. `jms exec`, `jms ls`, `jms api get`, `jms asset *` and `jms sftp` force nothing — the failover policy decides. `--endpoint` exists on `ls`, `exec`, `login`, `sftp`, `api get` and every `asset` subcommand; it does **not** exist on `attach`, `tail`, `log show`, `config test` or `mcp`.

The chosen address is printed on **stderr** as `Using <kind> endpoint <url>`, so stdout stays a clean table.

## Configuration examples

Two addresses, external preferred, one server opting out of the global proxy, one pinned to an address:

```toml
version = 2
default_server = "bastion"
proxy = "socks5://127.0.0.1:1080"

[servers.bastion]
internal = "http://192.168.1.10:2280/"
external = "https://jumpserver.example.com/"
username = "alice"
prefer = "external"

[servers.staging]
external = "https://jumpserver.staging.example.com/"
username = "alice"
proxy = "direct"

[servers.lab]
internal = "http://192.168.1.11:2280/"
username = "alice"
pin = "internal"
ssh_port = 2222
```

Checking what the client sees, per address:

```console
$ jms config test
Server: bastion (default)

Endpoint  URL                              Reachable  Probe  Login
internal  http://192.168.1.10:2280/        yes        0ms    ok
external  https://jumpserver.example.com/  yes        7ms    auth failed

Credentials: keyring
```

`jms config test` probes **every** candidate with the 1.5 s timeout and gives **every reachable** address a login attempt, not just the first. The `Login` cell is `ok`, `auth failed`, `unreachable`, `failed` or `skipped`.

The help text says "the first reachable one gets a real login attempt"; the implementation attempts every reachable address, because a probe alone cannot tell which address really works.

## Common failure modes

| Message | Cause | Fix |
|---|---|---|
| `no reachable endpoint: server "bastion": internal: probe failed; …` | No address answered the TCP connect. | Check the address, the port and the VPN. `jms config test` breaks it down per address. |
| `server "bastion" has no external address configured; refusing to fall back to the other address` | `--endpoint external` (or `jms login` without it) on an internal-only server. | Add `--endpoint internal`, or configure the external address. |
| `no reachable endpoint: server "bastion" has no address configured` | `pin` names a kind the server does not have. | Fix `pin`, or add the address. |
| `authentication error (HTTP 401): …` | The credential was refused. No failover happened, by design. | Fix the credential. The same failure would occur on the other address. |
| `websocket: command did not complete within 30s` | The ws backend's own ceiling, or a command that swallowed the status marker. | Pass `-t`. Rewrite a command ending in `#` or `\`, or with unbalanced quotes. |
| `no backend connected (ssh: …; ws: …)` | Both backends failed for that address. | Check that KoKo's SSH port is open, or that the WebSocket path is allowed. |
| `handshake: <url> returned HTTP 302` | The address answered the upgrade with a redirect instead of a terminal. | The address is probably the wrong kind for this path. Try the other one. |
| Requests go through a proxy you did not configure | A `proxy` value in the config file, or a transparent interception at the network level. | Remove the config entry, or set `proxy = "direct"`. |
| SFTP or SSH fails while `exec` works | KoKo's SSH port is blocked. The ws backend does not need it. | Open the port, or use the ws backend. |

## Limitations

- **The SSH backend has not been exercised against a live JumpServer.** The source records this itself: every deployment reached so far had the KoKo SSH entry disabled. The code paths are unit-tested only.
- **`jms ssh-pipe` ignores the configured proxy.** Read from the source; not confirmed at runtime.
- **`jms config test` cannot be pointed at a specific address.** It has no `--endpoint` flag and always exercises the natural candidate order.
- **The exact on-disk text of `state.json` was not observed** in this worktree; the field names come from the struct tags.
- **`NO_PROXY` is not honoured even for a configured proxy.** There is no bypass list.
- **The proxy implementations are hand-rolled** (HTTP CONNECT and SOCKS5, RFC 1928/1929) and add no third-party dependency.
