# 命令参考

`jms` v0.2.4 的全部命令、参数、默认值与退出码。
[README](../README.zh-CN.md) 里有简版。
相关页面：[MCP 服务](mcp.md) · [配置](configuration.md) · [地址与代理](networking.md) · [故障排查](troubleshooting.md) · [English](command-reference.md)。

## 本文约定

| 约定 | 含义 |
|---|---|
| `console` 代码块 | 一段终端会话记录，`$` 是你的 shell 提示符。 |
| `sh` 代码块 | 直接粘进 shell 执行的片段。 |
| stdout | 命令产出的数据，可以管道、可以重定向。 |
| stderr | 诊断信息、提示、地址行、告警，不属于数据。 |

每个命令都还支持内置的 `-h, --help`。

会访问服务器的命令（`ls`、`exec`、`login`、`sftp`、`api get` 以及所有 `asset` 子命令）都会把实际使用的地址打到 stderr，形如 `Using <kind> endpoint <url>`，因此 stdout 始终是干净的表格或干净的 JSON。

## 目标语法

大多数针对资产的命令都接受一个**目标**：

```
<asset>@<server>    指定服务器上的资产
<asset>             默认服务器上的资产
```

| 规则 | 行为 |
|---|---|
| 切分点 | 用**最后一个** `@` 区分资产与服务器。 |
| 没有 `@` | 资产位于默认服务器上（由 `jms config set-default` 决定）。 |
| `host@` 或 `@host` | 整串都是资产名，仍然落在默认服务器上，不是空服务器名。 |
| `a@b@c` | 资产是 `a@b`，服务器是 `c`。 |
| 目标为空 | `empty target: expected <asset>[@<server>]` |

`@` 形式适用于 `exec`、`login`、`sftp`（远端规格里的主机部分）和 `ssh-pipe`。
`jms ls`、`jms asset *` 与 `jms config test` 则把服务器作为**单独的位置参数**传入。

```console
$ jms exec web-01@bastion "uptime"
$ jms exec web-01 "uptime"                 # 默认服务器上的 web-01
```

## 全局参数与配置文件优先级

`--config` 是唯一的全局参数。它是 persistent 类型，因此放在子命令名前后都可以。

| 参数 | 类型 | 默认值 | 含义 |
|---|---|---|---|
| `--config` | string | `""` | 配置文件路径，优先级高于 `JMS_CONFIG`。 |

配置文件按以下顺序选取：

```
--config <path>   >   JMS_CONFIG   >   <UserConfigDir>/jms/config.toml
```

Unix 上 `<UserConfigDir>` 是 `$XDG_CONFIG_HOME` 或 `~/.config`，Windows 上是 `%AppData%`。
`JMS_CONFIG` 被设置且非空时生效。

指定配置文件同时会改变另外两样东西，因为它们都是从配置**目录**推导出来的：

| 产物 | 位置 |
|---|---|
| `config.toml` | `--config` 指向的路径本身 |
| `state.json` | `<配置目录>/state.json` |
| 审计日志目录 | `<配置目录>/audit` |

```console
$ JMS_CONFIG=/tmp/a.toml jms --config /tmp/b.toml config list   # --config 优先
```

配置文件旁边若存在旧版 `config.yaml`，命令会直接拒绝：
`detected legacy jms-cli config — unsupported by design; re-add with jms config add`。

## 退出码

| 情况 | 退出码 |
|---|---|
| 成功 | 0 |
| 任何通用错误（参数值非法、缺少凭据、服务器不可达、路径被拒、删除未确认等） | 1 |
| `jms exec` 中**远端**命令返回非零 | 远端的退出码 |
| `jms ssh-pipe` 中远端命令返回非零 | 远端的退出码 |
| `jms ssh-pipe` 桥接失败 | 1 |
| `jms attach` 被 Ctrl-C 结束 | 0 |
| `jms tail -f` 被 Ctrl-C 结束 | 0 |
| `jms` 不带参数，或分组命令不带子命令 | 0（打印帮助） |

通用失败会在 stderr 打印一行 `Error: <message>`。
自带退出状态码的命令**不会**打印 `Error:` 行。
每条命令结束后都会刷写审计日志；刷写失败会打印 `Warning: audit log: <err>`，但不影响退出码。

---

## `jms version`

```console
$ jms version
jms v0.2.4 (linux/amd64)
```

`jms version [flags]` —— 无位置参数，无参数选项。输出 `jms <version> (<GOOS>/<GOARCH>)` 到 stdout。

版本号是构建时打进二进制的。从工作副本直接构建出来的二进制会显示伪版本，例如 `v0.2.5-0.2026…+dirty`。

## `jms ls`

```console
$ jms ls
Name      Address         Platform  Type
web-01    192.168.10.11   Linux     host
db-01     192.168.10.12   Linux     host

Total: 2 asset(s)
```

`jms ls [server] [flags]` —— 位置参数：可选的服务器别名，最多 1 个。
输出：表头 `Name`、`Address`、`Platform`、`Type`，结尾 `Total: N asset(s)`；无结果时输出 `No assets found.`。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--search` | string | `""` | `-q` | 搜索关键字 |
| `--limit` | int | `50` | `-n` | 结果条数上限 |
| `--endpoint` | string | `""` | — | 强制使用地址：`internal` 或 `external` |

- `jms ls` 只列出当前账号有权访问的资产；`jms asset list` 列出堡垒机上的全部资产。
- `--search` 与 `--limit` 不叠加：给了关键字时会取回整个集合并在服务端过滤，`--limit` **不生效**。`--limit` 只截断普通列表，且是在客户端全量取回之后截断。
- 地址行打到 stderr，stdout 可以直接管道出去。

```console
$ jms ls -q web
$ jms ls bastion -n 10
```

## `jms exec`

```console
$ jms exec web-01 "systemctl is-active nginx"
active
```

`jms exec <target> <command...> [flags]` —— 位置参数：目标加至少一个命令词（合计 2 个及以上）。
输出：远端 stdout 走 stdout；地址行、耗时行、状态行走 stderr。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--account` | string | `""` | — | 覆盖账号 |
| `--protocol` | string | `""` | — | 覆盖协议 |
| `--backend` | string | `""` | — | 后端：`ssh`、`ws` 或 `auto` |
| `--endpoint` | string | `""` | — | 强制使用地址：`internal` 或 `external` |
| `--timeout` | int | `0` | `-t` | 命令超时（秒） |
| `--timing` | bool | `false` | — | 打印各阶段耗时 |

- 命令词用**单个空格**拼接后作为一条命令整体发送。引号由你负责：`jms exec web-01 'echo hello world'`。
- 默认后端是 **ws**。`--backend auto` 先试 `ssh`，再试 `ws`。非法取值会报 `invalid --backend "x": must be ssh, ws or auto`。
- stdout **只有**远端输出。地址行和 `Command exited with status N` 走 stderr，因此 `out=$(jms exec web-01 cmd)` 得到的内容是干净的。
- 退出码就是**远端**退出码。远端非零时 stderr 还会打印 `Command exited with status N`。
- 连接是池化的。同一资产上的第二条 `exec` 会复用已有终端。
- `--timeout 0`（默认值）表示“没有要求超时”。此时由后端自己的上限兜底：WebSocket 传输回落到 **30 秒**，SSH 后端没有自己的定时器，只靠命令上下文终止。需要明确上限时请传 `-t`。
- `--timing` 会向 stderr 打印 `Timing credentials:`、`Timing endpoint/auth:`、`Timing command:`。凭据那一行即使在失败路径上也会打印。

```console
$ jms exec web-01@bastion "df -h /"
$ jms exec web-01 "sleep 600" -t 900
```

## `jms login`

```console
$ jms login web-01
Connected to web-01 (ws backend). Press Ctrl+] to exit.
```

`jms login <target> [flags]` —— 位置参数：目标，恰好 1 个。本地控制台直接交给远端，状态行走 stderr。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--account` | string | `""` | — | 覆盖账号 |
| `--protocol` | string | `""` | — | 覆盖协议 |
| `--backend` | string | `""` | — | 后端：`ssh`、`ws` 或 `auto` |
| `--endpoint` | string | `""` | — | 强制使用地址：`internal` 或 `external` |
| `--timing` | bool | `false` | — | 打印各阶段耗时 |

- **`login` 默认走外部地址。** 其他所有命令里 `--endpoint` 留空都表示“不强制”，只有 `login` 例外。要走内网地址请显式加 `--endpoint internal`。
- 默认后端是 **ws**。`--backend auto` 会先尝试 `state.json` 中为当前地址类型记住的后端。
- 会话**不进池**，与本进程绑定，进程结束即结束。`Ctrl+C` 会取消命令并断开会话。
- `--timing` 会打印 `Timing credentials:`、`Timing endpoint/auth:`、`Timing asset resolve:`、`Timing <backend> connect:` 和 `Timing to interactive:`。

```console
$ jms login web-01@bastion --endpoint internal
```

## `jms sftp`

```console
$ jms sftp report.csv web-01:/tmp/report.csv
Transferred 20480 bytes in 1.2s
MD5 verified: 9a0364b9e99bb480dd25e1f0284c8555
```

`jms sftp <src> <dst> [flags]` —— 位置参数：恰好 2 个，源与目标。
输出：`Transferred N bytes in <duration>`，校验通过时还有 `MD5 verified: <digest>`。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--recursive` | bool | `false` | `-R` | 递归目录 —— **尚未实现**，见下文 |
| `--verify` | bool | `false` | — | 用 md5 校验（本来就是默认行为） |
| `--no-verify` | bool | `false` | — | 跳过 md5 校验 |
| `--parallel` | int | `0` | — | 并发分块流数；`0` 表示 4 |
| `--chunk` | int | `0` | — | 分块大小（字节）；`0` 表示 1 MiB |
| `--endpoint` | string | `""` | — | 强制使用地址：`internal` 或 `external` |
| `--account` | string | `""` | — | 覆盖账号 |

- 远端规格是 `<asset>[@<server>]:<path>`。按**第一个冒号**切分主机与路径，所以路径里可以再有冒号。远端路径为空会被拒绝。
- 方向自动推断：目标是远端就是上传，源是远端就是下载。
- 一侧包含冒号即视为远端，但 Windows 盘符路径（`C:\…`、`C:/…`）始终算本地。两侧都是本地路径会报错：`neither argument is a remote path: use <asset>[@<server>]:<path> for the remote side`。
- 传输走堡垒机的 **KoKo SSH 端口**（默认 `2222`），具体是会话绑定到的那个地址。该端口必须在你的网络路径上可达。
- md5 校验会在远端通过 SSH exec 会话执行 `md5sum`，因此账号需要 SSH 执行权限。摘要不一致会失败并报 `md5 verification failed`。
- `--verify` 与 `--no-verify` 不能同时出现：`--verify and --no-verify are mutually exclusive`。单独写 `--verify` 是空操作，默认就开启校验。

```console
$ jms sftp web-01:/var/log/app.log ./app.log --no-verify
$ jms sftp report.csv web-01:/tmp/report.csv --chunk 1048576 --parallel 8
```

## `jms ssh-pipe`

```console
$ jms ssh-pipe web-01 uname -a
```

`jms ssh-pipe <target> [command...] [flags]` —— 位置参数：1 个及以上，全部原样转发。
该命令**没有任何自己的参数**：标志解析被关闭，所以 `jms ssh-pipe --help` 不会打印帮助，而是失败并输出 `jms ssh-pipe: unrecognized option "--help"`，退出码 1。

桥接器能理解的语法：

| 形式 | 含义 |
|---|---|
| `<asset>[@<server>]` | 目标，用最后一个 `@` 切分 |
| `-l <asset> [server]` | rsync 的 ssh 风格写法 |
| `--config <path>` / `--config=<path>` | 桥接器使用的配置文件 |
| 其他一切 | 远端命令，用空格拼接 |

除 `-l` 和 `--config` 外，任何以 `-` 开头的参数都会被拒绝。

- **stdout 是协议通道。** 桥接器自己不往 stdout 写任何东西，所有诊断信息以 `jms ssh-pipe: <message>` 形式走 stderr。
- 远端退出码即进程退出码。
- 桥接器是非交互的。服务器要求 MFA 而本地又没存 TOTP 密钥时，它会失败而不是提示输入。
- 桥接器自己加载配置，自行解析服务器、凭据、资产与 SSH 令牌，不复用 CLI 的会话。
- 由于标志解析被关闭，桥接器会自己解析 `--config` 参数；`JMS_CONFIG` 同样生效。

错误信息：`expected '-l <asset> <server>' or '<asset>@<server>' from rsync`、`no remote command in arguments`、`-l needs an asset name`、`--config needs a path`。

```sh
# 通过桥接器跑 rsync
rsync -av -e "jms ssh-pipe web-01" ./dist/ web-01:/srv/app/
```

## `jms tail`

```console
$ jms tail --last 5
14:02:11  bastion/web-01  ws  [外]  ♻命中  exec  [alice]  -> 0  $ uptime  exit 0  120ms  64B
```

`jms tail [flags]` —— 无位置参数。输出：每条事件一行，由旧到新。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--follow` | bool | `false` | `-f` | 持续跟随 |
| `--json` | bool | `false` | — | 输出原始 JSONL |
| `--last` | int | `0` | — | 从最后 N 条事件开始 |

- `--last N` 只保留启动时读到的最新 N 条事件。
- 跟随循环每 250 毫秒轮询一次。这是轮询而不是文件系统监听，所以在各平台上表现一致。
- `Ctrl+C` 会干净地结束跟随，命令退出码为 0。
- 多个 MCP 客户端同时写入会生成多个文件，这些文件按时间戳合并。
- `--json` 直接打印原始 JSONL 行，而不是渲染后的单行。

## `jms attach`

```console
$ jms attach --last 10
```

`jms attach [flags]` —— 无位置参数。输出：每条事件一行。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--asset` | string | `""` | — | 只显示该资产 |
| `--readonly` | bool | `false` | — | 只观察 |
| `--last` | int | `0` | — | 先重放最后 N 条事件 |
| `--attach` | bool | `false` | — | 启用操作员提示（默认关闭，只观察） |

- **默认只观察。** 直接执行 `jms attach` 绝不接受输入；必须加 `--attach` 才会启用操作员提示。
- `--readonly` 优先于 `--attach`：只有同时满足“设了 `--attach` 且没设 `--readonly`”时才是操作员角色。
- 操作员命令与宿主共用同一个终端池，所以人工命令与 AI 命令会在同一条连接上串行执行，不会互相抢。
- 客户端连接的是**配置目录**下的 IPC 端点：Unix 上是 unix socket `<配置目录>/jms-<hash>.sock`，Windows 上是命名管道 `\\.\pipe\jms-<hash>`。
- 命令会一直阻塞到 Ctrl-C。取消被视为正常结束而不是失败，因此退出码为 0。
- 事件渲染方式与 `jms tail` 相同。无法解析的事件会原样打印，不会被丢掉。

## `jms log show`

```console
$ jms log show --since 1h --failed
```

`jms log show [flags]` —— 无位置参数。输出：每条事件一行，由旧到新。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--since` | string | `""` | — | 只看该时间之后的事件（例如 `1h`、`30m`） |
| `--asset` | string | `""` | — | 只看该资产 |
| `--failed` | bool | `false` | — | 只看失败的命令 |
| `--export` | string | `""` | — | 导出格式：`md` |

- `--since` 接受 **Go duration**。裸数字会被拒绝：`--since "2" is not a duration: use a form like 1h or 30m`。
- `--failed` 只保留带错误信息或非零退出码的事件。
- `--export md` 输出 Markdown 表格，表头是 `| Time | Server | Asset | Command | Exit | Duration | Actor |`。`markdown` 同样可用。其他取值会报 `unsupported export format "x": use md`。
- 没有匹配事件时，默认形式打印 `No audit events matched.`。
- 它读的是本地审计文件，不需要有宿主在运行。

## `jms log prune`

```console
$ jms log prune --keep 30 --dry-run
jms: audit prune in /home/you/.config/jms/audit: would delete 3 file(s), 245760 bytes; kept 12 within the window (cutoff 20260911); skipped 0
```

`jms log prune [flags]` —— 无位置参数。输出 `jms: audit prune in <dir>: <summary>`。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--keep` | int | `0` | — | 保留这么多天（覆盖配置里的窗口） |
| `--dry-run` | bool | `false` | — | 只报告将删除哪些文件，不实际删除 |
| `--dir` | string | `""` | — | 审计目录（默认用配置好的那个） |

- 保留窗口的取值顺序是 `--keep`，其次 `JMS_AUDIT_KEEP_DAYS`，最后是配置文件里的 `audit_keep_days`。
- 三个来源都没有配置窗口时，命令会拒绝执行：`no retention window configured: pass --keep <days>, set JMS_AUDIT_KEEP_DAYS, or add audit_keep_days to the configuration file`。
- 文件年龄取自**文件名**（`audit-YYYYMMDD-<pid>.jsonl`），不是修改时间。
- 文件名里没有日期的文件会被**跳过，而不是删除**。

## `jms api get`

```console
$ jms api get /api/v1/assets/hosts/ -q limit=3 --raw
```

`jms api get <path> [server] [flags]` —— 位置参数：路径加可选的服务器别名（1 到 2 个）。
输出：JSON 响应体，默认缩进两格。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--endpoint` | string | `""` | — | 强制使用地址：`internal` 或 `external` |
| `--query` | stringArray | `nil` | `-q` | 查询参数 `key=value`（可重复） |
| `--raw` | bool | `false` | — | 不缩进，直接打印响应 |
| `--max-bytes` | int | `16384` | — | 打印的响应体最多这么多字节（`0` 表示不限制） |

- 路径必须以 `/api/v1/` 开头，必须以斜杠结尾，不能含 `..` 或 `//`，不能含 `?` 或 `#`，不能是绝对 URL，长度不超过 512 字节。
- 必须落在下列允许的前缀之下：

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

- 即使在允许的前缀之下，携带凭据的路径段也会被拒绝：`authentication`、`auth`、`secret`、`secrets`、`token`、`tokens`、`key`、`keys`、`passphrase`、`password`。匹配按整段精确比较，因此审计前缀下的 `password` 变更日志路径仍然可读。
- `-q` 必须是 `key=value`，否则报 `-q "<pair>" is not key=value`。
- 截断时会在响应体后打印 `… truncated at <n> of <total> bytes (use --max-bytes 0 to print all)`。响应体为空时打印 `No content.`。
- **每次调用都会被审计。** 成功调用记为 `api.read` 事件，含状态码与响应体大小；被拒绝的路径记为 `api.denied` 事件并带上原因。看起来像凭据的查询参数在写入审计记录前会被脱敏。
- `jms api` 有意不对外暴露为 MCP 工具。

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

`jms mcp [flags]` —— 无位置参数。stdout 是 MCP 协议通道，除此之外不写任何东西。

参数：`--print-config`（bool，默认 `false`）打印客户端配置片段，其中不含任何凭据。

- `--print-config` 打印上面的片段后以退出码 0 结束。
- 诊断信息走 stderr，stdout 专供协议使用。
- 该进程同时承载本地观察通道，因此 `jms attach` 客户端看到的是同一个连接池、同一条事件流。
- 监听失败只是告警，不是错误：`Warning: observation channel unavailable: <err>`。
- 审计里的 actor 会被改写为 `mcp`；若设置了 `JMS_ACTOR` 则用它的值。
- **没有任何按调用生效的参数。** 地址与后端由环境变量控制：`JMS_MCP_ENDPOINT`（`internal` 或 `external`）和 `JMS_MCP_BACKEND`（`ssh`、`ws` 或 `auto`）。两者都没设时，先用 `state.json` 中为当前地址类型记住的后端，最后才回落到 `ws`。
- 连接失败时会重新选择地址并重试一次，不需要重启进程。

MCP 工具的参数结构见 [mcp.md](mcp.md)。

## `jms config`

`jms config` 是分组命令。不带子命令时打印帮助并以退出码 0 结束。

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

`jms config add <alias> [flags]` —— 位置参数：别名，恰好 1 个。
输出：`Server "<alias>" saved to <path>`；带 `--set-default` 时追加 `Server "<alias>" is now the default.`。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--set-default` | bool | `false` | — | 设为默认服务器 |

| # | 提示 | 类型 |
|---|---|---|
| 1 | `Internal URL (http(s)://..., empty to skip): ` | 明文，可为空 |
| 2 | `External URL (http(s)://..., empty to skip): ` | 明文，可为空 |
| 3 | `Username: ` | 明文，必填 |
| 4 | `Password: ` | 隐藏输入 |
| 5 | `Confirm Password: ` | 隐藏输入，必须与第 4 项一致 |
| 6 | `TOTP secret (base32, empty to enter the code each time): ` | 隐藏输入，可为空 |

- 该命令是**全交互式**的。地址、用户名、password 和 TOTP 密钥都没有对应的参数，只能靠提示输入。stdin 不是终端时，隐藏提示会从共享的带缓冲读取器里读一整行，因此管道输入也能用。
- 两个地址至少要有一个；每个非空地址都必须是绝对的 `http(s)` URL。
- password 会**先拿一个可达地址做校验，通过之后才写盘**，因此被拒绝的 password 不会在磁盘上留下任何痕迹。
- 第一个添加的服务器会成为默认服务器。
- TOTP 密钥不是合法 base32 只是告警，不算失败：照样会被保存。
- 先写凭据存储，再写配置文件；配置文件写失败会把凭据回滚。
- 配置文件在 Unix 上以 `0600` 权限原子写入，父目录为 `0700`。
- password 永远不写进配置文件。它存放在操作系统凭据存储里，或者放在 `JMS_PASSWORD_<ALIAS>` 环境变量中（别名转为大写，其中每个 `-` 变成 `_`，例如 `JMS_PASSWORD_BASTION`）。环境变量优先级高于凭据存储，无头机器上应当用环境变量。

### `jms config list`

```console
$ jms config list
Alias     Internal           External                  Username  Creds    Default
bastion   http://10.0.0.10    https://jumpserver...     alice     keyring  *
```

`jms config list [flags]` —— 无位置参数，无参数选项。
输出：表头 `Alias`、`Internal`、`External`、`Username`、`Creds`、`Default`，结尾 `Total: N server(s)`。空单元格显示 `-`，默认服务器的 `Default` 列是 `*`。

`Creds` 列只有四种取值：

| 取值 | 含义 |
|---|---|
| `env` | 设置了 `JMS_PASSWORD_<ALIAS>` 环境变量 |
| `keyring` | 操作系统凭据存储里有 password |
| `missing` | 没有保存 password |
| `locked` | 存储里有，但无人值守时读不出来 |

一个服务器都没配置时命令会失败：`no servers configured: run 'jms config add <alias>'`。
两条告警走 stderr，每个有问题的服务器一条：password 缺失，以及凭据存储被锁。

### `jms config remove`

```console
$ jms config remove bastion
Remove server "bastion"? [y/N]: y
Server "bastion" removed (/home/you/.config/jms/config.toml)
```

`jms config remove <alias> [flags]` —— 位置参数：别名，恰好 1 个。输出 `Server "<alias>" removed (<path>)`。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--yes` | bool | `false` | `-y` | 不询问确认 |

- 不带 `--yes` 会提示 `Remove server "<alias>"?`。拒绝会报 `aborted by user`，退出码 1。
- 删除**最后一个**服务器时，配置文件会被整个删掉，并输出 `Server "<alias>" removed (<path>) - no servers remain`。
- 如果凭据清理失败，错误里会同时说明两件事：服务器已从配置文件中移除，但它存储的凭据没能删掉。

### `jms config set-default`

```console
$ jms config set-default bastion
Server "bastion" is now the default (/home/you/.config/jms/config.toml)
```

`jms config set-default <alias> [flags]` —— 位置参数：别名，恰好 1 个。无参数选项。
输出 `Server "<alias>" is now the default (<path>)`。别名不存在时报 `server not found`。

### `jms config test`

```console
$ jms config test
Server: bastion (default)

Endpoint   URL                                Reachable  Probe  Login
internal   http://10.0.0.10                   yes        3ms    ok
external   https://jumpserver.example.com     yes        180ms  ok

Credentials: keyring
```

`jms config test [alias] [flags]` —— 位置参数：可选别名，最多 1 个。无参数选项。
输出：`Server: <alias> (default)`，然后是表头 `Endpoint`、`URL`、`Reachable`、`Probe`、`Login` 的表格，最后是 `Credentials: <source>`。

- 每个配置了地址都会做一次 TCP 可达性探测，超时 **1.5 秒**，探测同样遵循该服务器的代理策略。
- **每一个**可达地址都会尝试一次登录，而不是只试第一个。第一个错误会被保留并报出。
- `Login` 单元格取值为 `ok`、`auth failed`、`unreachable` 或 `failed`；不可达的行显示 `skipped`。`Probe` 单元格是探测耗时（毫秒）。
- 服务器没有配置任何地址时报 `server "<name>" has no address configured: run 'jms config add <name>'`。
- 失败文案区分两类原因：
  - 凭据被拒 → `the server "<name>" rejected the stored credentials for "<url>": <err>`
  - 其他错误 → `login to "<url>" failed for server "<name>" (network or server error, not a credential problem): <err>`
  - 全部不可达 → `all addresses unreachable: server "<name>" is unreachable on every configured address (<detail>)`

## `jms asset`

`jms asset` 是分组命令。不带子命令时打印帮助并以退出码 0 结束。
每个 `asset` 子命令都接受一个可选的尾部 `[server]` 位置参数和 `--endpoint` 参数，并把选中的地址打印到 stderr。

### `jms asset list`

```console
$ jms asset list
Name      Address         Platform  Comment
web-01    192.168.10.11   Linux     production
```

`jms asset list [server] [flags]` —— 位置参数：可选服务器别名，最多 1 个。参数只有 `--endpoint`。
输出：表头 `Name`、`Address`、`Platform`、`Comment`，结尾 `Total: N asset(s)`；无结果时输出 `No assets found.`。

它列出堡垒机上的**全部**资产，包括尚未授权给你的资产。列表不按你的权限过滤，也没有 `--limit` 或 `--search` 参数。

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

`jms asset show <name-or-id> [server] [flags]` —— 位置参数：资产名或 id，加可选的服务器别名（1 到 2 个）。参数只有 `--endpoint`。
输出是 `键: 值` 形式的块，不是表格。

块内字段依次是：`ID`、`Name`、`Address`、`Platform`、`Comment`、`Nodes`、`Accounts`。空值显示为 `-`，账号显示为 `name(username)`。
先按 id 查找，再按精确名称匹配。

### `jms asset create`

```console
$ jms asset create web-02 192.168.10.12 --account ops --username ops --password '...'
Created web-02 (id 7f3a…)
Note: a new asset is not connectable until it is granted to your account;
      pass --grant-me, or run `jms asset grant <name> --self`.
```

`jms asset create <name> <address> [server] [flags]` —— 位置参数：名称、地址，加可选的服务器别名（2 到 3 个）。
输出：`Created <name> (id <id>)`。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--endpoint` | string | `""` | — | 强制使用地址：`internal` 或 `external` |
| `--account` | string | `""` | — | 要挂载的账号显示名 |
| `--username` | string | `""` | — | 目标主机上的登录名（默认取 `--account`） |
| `--password` | string | `""` | — | 账号 password（随请求发送，不落盘） |
| `--node` | string | `""` | — | 资产节点 id（默认使用默认节点） |
| `--platform` | int | `0` | — | 平台 id（`0` 表示 Linux） |
| `--comment` | string | `""` | — | 自由文本备注 |
| `--protocol` | strings | `nil` | — | 要启用的协议（默认 `ssh,sftp`） |
| `--grant-me` | bool | `false` | — | 同时给当前用户授权 |

- `--platform 0` 会被换算成内置的 Linux 平台 id `1`。
- `--protocol` 是逗号分隔的列表。为空时账号会拿到 `ssh` 和 `sftp`，端口都是 22。
- `--username` 为空时回退到 `--account`。`--node` 为空表示使用服务器的默认节点。
- 账号 password 随创建请求发送，本地不做任何留存。
- 名称重复会返回 `asset name already exists`。
- 不带 `--grant-me` 时，命令会在 stderr 打印“尚不可连接”的提示。带上 `--grant-me` 时会创建名为 `grant-<资产名>` 的权限，并打印 `Granted to the current user via permission <name>`。

### `jms asset update`

```console
$ jms asset update web-01 --comment "production, eu-west"
Updated web-01 (id 4b1c…)
```

`jms asset update <name-or-id> [server] [flags]` —— 位置参数：资产名或 id，加可选的服务器别名（1 到 2 个）。
输出：`Updated <name> (id <id>)`。

| 参数 | 类型 | 默认值 | 含义 |
|---|---|---|---|
| `--endpoint` | string | `""` | 强制使用地址 |
| `--name` | string | `""` | 新资产名 |
| `--comment` | string | `""` | 新备注 |
| `--address` | string | `""` | 新地址 |

只有你显式设置过的参数才会被发送。没设置的参数绝不会以空值写入，所以要清空备注得写 `--comment ""`，不写这个参数则会保持原值。平台 id 和节点无法用该命令修改。

### `jms asset delete`

```console
$ jms asset delete web-02
About to delete asset "web-02" (id 7f3a…, address 192.168.10.12).
Delete "web-02"? [y/N]: y
Deleted web-02 (id 7f3a…)
```

`jms asset delete <name-or-id> [server] [flags]` —— 位置参数：资产名或 id，加可选的服务器别名（1 到 2 个）。
输出：`Deleted <name> (id <id>)`。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--endpoint` | string | `""` | — | 强制使用地址 |
| `--yes` | bool | `false` | `-y` | 不询问确认 |

- 不带 `--yes` 时，命令会先在 stdout 打印“About to delete”那一行，再询问 `Delete "<name>"?`。提示格式为 `<prompt> [y/N]: `，**除 `y` 和 `yes` 之外的任何输入都算拒绝**。
- 拒绝是一种错误：`not confirmed; nothing was deleted`，退出码 1。
- 它**只删除资产本身**。账号、节点以及引用它的权限都留在原地；指向已删资产的权限会继续出现在 `jms asset permissions` 里，直到你手动撤销。

### `jms asset grant`

```console
$ jms asset grant web-02 --user alice --group ops --protocol ssh
Granted web-02 to 1 user(s) and 1 user group(s) via permission grant-web-02 (id 91cd…)
```

`jms asset grant <asset-name-or-id> [server] [flags]` —— 位置参数：资产名或 id，加可选的服务器别名（1 到 2 个）。
输出：`Granted <asset> to <N> user(s) and <M> user group(s) via permission <name> (id <id>)`。

| 参数 | 类型 | 默认值 | 简写 | 含义 |
|---|---|---|---|---|
| `--endpoint` | string | `""` | — | 强制使用地址 |
| `--self` | bool | `false` | — | 同时给当前认证用户授权 |
| `--user` | strings | `nil` | — | 要授权的用户 id 或精确用户名（可重复） |
| `--group` | strings | `nil` | — | 要授权的用户组 id 或精确组名（可重复） |
| `--account` | strings | `nil` | — | 允许的账号（默认 `@ALL`） |
| `--protocol` | strings | `nil` | — | 允许的协议（默认全部） |
| `--action` | strings | `nil` | — | 允许的动作（默认 `connect`） |
| `--name` | string | `""` | — | 权限名（默认 `grant-<资产名>`） |
| `--comment` | string | `""` | — | 自由文本备注 |

- `--user` 与 `--group` 接受列表：可以重复写这个参数，也可以用逗号分隔。
- 主体引用的解析顺序：形如 UUID 的取值先按 id 试，404 则回落到名称路径；其他取值按服务端精确过滤（用户按 `username`，用户组按 `name`）。没有匹配报 `no user matches "<ref>"; pass an id or an exact name`，多个匹配报 `"<ref>" matches N users; pass the id instead`。空引用是错误，绝不会被跳过。
- **默认主体就是当前认证用户。** 既没给 `--user` 也没给 `--group` 时，授权给当前用户。`--self` 是在显式列表之上追加的。
- 权限名默认是 `grant-<资产名>`。
- 参数为空时由服务端套用默认值：账号 `@ALL`、协议 all、动作 `connect`。
- 用户与用户组至少要有一个。

### `jms asset permissions`

```console
$ jms asset permissions
Name          Users   Groups  Assets   Nodes  Protocols  Actions
grant-web-01  alice   ops     web-01   -      all        connect

Total: 1 permission(s)
```

`jms asset permissions [asset-name-or-id] [server] [flags]` —— 位置参数：可选的资产名或 id，加可选的服务器别名（0 到 2 个）。参数只有 `--endpoint`。
输出：表头 `Name`、`Users`、`Groups`、`Assets`、`Nodes`、`Protocols`、`Actions`，结尾 `Total: N permission(s)`；无结果时输出 `No permissions found.`。

不带资产参数时列出**全部**权限；带上则只列出引用该资产的权限。`Users`、`Groups`、`Assets`、`Nodes` 单元格填的是名称（逗号分隔），不是数量。关系数量尚未展开的行会再读一次详情接口，因此只含用户组或只含节点的规则不会被漏掉。空单元格显示为 `-`。

### `jms asset revoke`

```console
$ jms asset revoke grant-web-02
Revoke permission "grant-web-02"? [y/N]: y
Revoked grant-web-02 (id 91cd…)
```

`jms asset revoke <permission-name-or-id> [server] [flags]` —— 位置参数：权限名或 id，加可选的服务器别名（1 到 2 个）。
输出：`Revoked <name> (id <id>)`。

参数：`--endpoint`（string，默认 `""`）强制使用地址；`--yes`/`-y`（bool，默认 `false`）跳过确认。

目标按权限 id 或精确名称匹配。没有匹配报 `no permission matches "<target>"`。不带 `--yes` 时会询问 `Revoke permission "<name>"?`；拒绝会报 `not confirmed; nothing was revoked`，退出码 1。撤销的只是授权，资产本身保留。

## `jms completion`

`jms completion` 是 cobra 内置命令，为某一种 shell 生成补全脚本：`bash`、`fish`、`powershell` 或 `zsh`。

参数：`--no-descriptions`（bool，默认 `false`）关闭补全描述。

```console
$ jms completion bash --help
  source <(jms completion bash)
  jms completion bash > /etc/bash_completion.d/jms        # Linux
  jms completion bash > $(brew --prefix)/etc/bash_completion.d/jms   # macOS
```

## 可观测性

三条命令读的是同一份本地审计流。审计流是审计目录下的一组按进程划分的 JSONL 文件（`<配置目录>/audit`，或 `JMS_AUDIT_DIR`），文件名为 `audit-YYYYMMDD-<pid>.jsonl`。

| 命令 | 读什么 | 是否实时 | 机器可读 |
|---|---|---|---|
| `jms tail` | 那些文件 | 加 `-f` 时实时 | 加 `--json` 时 |
| `jms attach` | 运行中宿主的 IPC 通道 | 实时 | 否 |
| `jms log show` | 那些文件 | 否 | 加 `--export md` 时 |

一条事件渲染成一行：

```
HH:MM:SS  <server>/<asset>  <backend>  [<endpoint badge>]  [<pool badge>]  <kind>  [<actor>]  <path>  -> <status>  $ <command>  exit <n>  <n>ms  <n>B  ! <error>
```

| 标记 | 取值 |
|---|---|
| 地址 | 内网为 `[内]`，外网为 `[外]` |
| 连接池 | 冷连接为 `⚡冷连`，命中池为 `♻命中` |

- `jms tail` 读的是文件，因此有没有宿主在跑都能用。
- `jms attach` 需要有宿主在运行：它连接配置目录下的 IPC 端点，可以加入同一批池化连接。
- `jms log show` 用来回看某个时间窗口，`jms tail -f` 用来实时盯。

审计写入本身由 `JMS_AUDIT`（设为 `off` 可关闭）、`JMS_AUDIT_DIR`、`JMS_AUDIT_FULL` 和 `JMS_AUDIT_PREVIEW_BYTES` 控制。保留窗口是 `JMS_AUDIT_KEEP_DAYS` 或配置文件里的 `audit_keep_days`。详见 [configuration.md](configuration.md)。

## 容易踩的坑

| 坑 | 实际行为 |
|---|---|
| `login` 走的是外部地址 | 除了 `login`，`--endpoint` 留空都表示“不强制”；`login` 默认 `external`。要走内网就加 `--endpoint internal`。 |
| `exec` 在 WebSocket 后端上有 30 秒上限 | `--timeout 0` 表示“没有要求超时”，此时 ws 传输会套用自己的 30 秒截止时间。SSH 后端没有自己的定时器。同一调用在 MCP 服务端的上限是 5 分钟。 |
| `ls --search` 会忽略 `--limit` | 给了关键字就切换到服务端搜索，返回全部匹配项。 |
| `sftp` 需要 KoKo SSH 端口 | 传输和 md5 校验都走堡垒机的 SSH 端口（默认 `2222`）。很多部署只开放了 Web 端口。 |
| `sftp` 没有 `-R`，也没有中转 | 两者都会失败并提示 `requires a transfer seam that carries two resolved sessions (relay) or a directory walk (-R)`。 |
| `asset delete` 不会删权限 | 只删除资产本身，权限要自己撤销。 |
| `asset update` 不会误清字段 | 只有你设置过的参数才会被发送。 |
| `asset grant` 默认授权给你自己 | 既没给 `--user` 也没给 `--group` 时，主体就是当前认证用户。 |
| `ssh-pipe` 没有帮助 | 标志解析被关闭，`--help` 会直接报错。 |

## 限制

- `jms sftp` 不能在两个资产之间复制，也不能递归目录。
- `jms api get` 只读，且受上面的白名单限制；CLI 里没有对应的写入命令。
- `jms attach --attach` 只做了源码层面的核对，没有可用的宿主来验证操作员提示。
- `jms exec` 的 stdout 纯净性来自代码判断；真实服务器返回的确切字节未在本工作副本中观测到。
- Windows 特有的 IPC 与路径行为来自源码阅读，未在 Windows 上实际执行。
- `jms ssh-pipe` 是非交互的，服务器要求 MFA 时它会失败而不是提示输入。
