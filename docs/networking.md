# Addresses and proxies

How the client picks an address and how it routes traffic.
The [README](../README.md) has the short version.

## How an address is chosen

When a server has both addresses, the client:

1. Puts the remembered last-good address first (kept for 10 minutes in `state.json`).
2. Orders the rest by `prefer` (default: internal first).
3. Probes each candidate with a 1.5-second TCP connect.
4. Logs into the first one that answers.

Failure to reach one address does not fail the command — it moves to the next. Two deliberate exceptions:

- **Authentication errors (401/403) never trigger a failover.** If the password is wrong, the other address would refuse it too.
- **`pin` disables failover entirely.** Use it when you know which network you are on and would rather see an honest error than a silent fallback.

A failed connection re-reads the state file and retries once through a fresh login, but only if the command had not already started producing output.

## Proxy policy

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
