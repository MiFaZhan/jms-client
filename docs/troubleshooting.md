# Troubleshooting

What each error message means and what to do about it.
The [README](../README.md) has the short version.


**`the credential store is locked`**
The OS credential store exists but cannot be read unattended. On a machine with no desktop session this is the usual state of a GNOME Keyring: the Secret Service answers an unlock request with a prompt, and nobody is there to type the unlock password.

The command fails within milliseconds and names the environment variable to set. Either unlock the keyring from a desktop session, or bypass the store entirely:

```sh
export JMS_PASSWORD_BASTION='…'
```

To see which stage a run reached, add `--timing`; the credential stage is timed on the failure path too.

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
