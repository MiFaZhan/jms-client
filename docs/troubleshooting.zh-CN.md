# 故障排查

每条错误分别意味着什么、由什么原因引起，以及最短的修复方式。
[README](../README.md) 中有精简版。
相关文档：[English](troubleshooting.md) · [命令参考](command-reference.md) · [MCP 服务器](mcp.md) · [配置](configuration.md) · [地址与代理](networking.md)。

## 60 秒快速定位

按顺序执行下面四条命令。第一条失败的命令，就指向你该往下读哪一节。

| # | 命令 | 它能告诉你什么 |
|---|---|---|
| 1 | `jms config test` | 每个地址是否响应，以及登录是否成功。每个地址输出一行。 |
| 2 | `jms ls` | 凭据与 API 能否端到端正常工作。 |
| 3 | `jms exec web-01 true --timing` | 慢在哪个阶段：凭据、endpoint/auth，还是命令本身。 |
| 4 | `jms tail --last 20` | 最近运行了什么、从哪个入口运行、结果如何。 |

`jms config test` 是最快的单项检查。它以 1.5 s 的 TCP 超时探测**每一个**已配置的地址，并对**每一个可达的**地址发起一次登录尝试，因此凭据问题会与网络不可达分开报告：

```console
$ jms config test
Server: bastion (default)

Endpoint  URL                  Reachable  Probe  Login
internal  http://10.0.0.5:2280/  yes        0ms    ok
external  https://bastion.example.com/  yes  7ms   auth failed

Credentials: keyring
```

`Login` 一列的取值是 `ok`、`auth failed`、`unreachable`、`failed` 或 `skipped`。`Credentials:` 一行读作 `env`、`keyring` 或 `missing`。

## 无法连接到堡垒机

### `no reachable endpoint: server "bastion": internal: probe failed; …`

没有任何候选地址响应 TCP 连接。

原因：配置中的地址不是你当前网络能到达的地址、VPN 未连接，或者端口填错了。

修复：

```sh
jms config test
```

逐个地址查看 `Reachable` 列。然后检查 `config.toml` 中的地址与端口；如果堡垒机需要 VPN，请先连接 VPN。

### `no reachable endpoint: server "bastion" has no address configured`

配置文件中该服务器既没有 `internal` 也没有 `external`。

修复：`jms config add bastion`。

### `server "bastion" has no external address configured; refusing to fall back to the other address`

你强制指定了服务器并不具备的地址。`--endpoint external`（或 `JMS_MCP_ENDPOINT=external`）绝不会静默回退。

修复：改用服务器实际拥有的地址，或者补上缺失的那个：

```sh
jms ls --endpoint internal
```

### `no backend connected (ssh: …; ws: …)`

被选中的地址上两个传输后端都失败了。消息中的两部分分别是 `ssh` 与 `ws` 的尝试结果。

修复：显式尝试另一个后端，然后检查网络：

```sh
jms exec web-01 hostname --backend ws
jms exec web-01 hostname --backend ssh
```

`ws` 正常而 `ssh` 失败，几乎总是意味着 KoKo 的 SSH 端口未开放。参见 [SFTP 与 SSH 端口故障](#sftp-and-ssh-port-failures)。

### `handshake: <url> returned HTTP 302`

地址对升级请求返回了重定向，而不是终端。这个地址很可能不是这条路径该用的那类地址。

修复：换另一个地址，并确认该 URL 是堡垒机自身的地址，而不是它前面的代理或门户。

## 登录与凭据故障

### `no stored password for server "bastion": run `jms config add bastion` or set JMS_PASSWORD_BASTION`

该别名没有存储任何凭据，无论是环境变量里还是操作系统凭据存储里都没有。

修复：

```sh
jms config add bastion
```

或者设置环境变量，它是只读的，并且始终优先于凭据存储：

```sh
export JMS_PASSWORD_BASTION='…'
```

别名会被转成大写，每个 `-` 变为 `_`，因此别名 `my-wan` 对应 `JMS_PASSWORD_MY_WAN`。空值视为未设置，而不是一个空凭据。

### `the credential store is locked — unlock the keyring, or set JMS_PASSWORD_BASTION`

操作系统凭据存储存在，但无法在无人值守的情况下读取。在 Linux 上是 Secret Service 集合被锁定；在 Windows 和 macOS 上则是系统调用在 10 s 内没有响应。

修复：在桌面会话中解锁 keyring，或者用上面的环境变量完全绕过凭据存储。该消息同时会指明需要设置的环境变量。

`jms config list` 会在 `Creds` 列显示同样的情况，并打印一条警告：

```console
$ jms config list
Alias    Internal             External  Username  Creds    Default
bastion  http://10.0.0.5:2280/  -       ops       locked   *

Warning: the credential store is locked — unlock it, or set JMS_PASSWORD_BASTION, to use server "bastion"
```

`locked` 与 `missing` 不是一回事。`missing` 表示不存在凭据；`locked` 表示凭据存在，但存储打不开。

### `cannot read the password for "bastion": …`

存储失败的原因既不是 "locked" 也不是 "absent"——可能是 D-Bus 错误、缺少 Secret Service，或者超时。

修复：检查会话总线是否可达，并回退到环境变量：

```sh
export JMS_PASSWORD_BASTION='…'
```

### `the server "bastion" rejected the stored credentials for "bastion": …`

服务器收到了凭据但拒绝了它（HTTP 400、401、403 或 422）。这是终止性错误：按设计不会发生故障切换，因为另一个地址同样会拒绝它。

修复：用 `jms config add bastion` 重新录入凭据。如果启用了 MFA，同时在提示处保存 TOTP 密钥。

### `MFA required but no TOTP secret configured`

该账号需要第二重认证因子，但没有保存 TOTP 密钥。

修复：重新执行 `jms config add bastion` 并在提示处输入 TOTP，或者在环境变量中设置该密钥：

```sh
export JMS_OTP_BASTION='<base32 secret>'
```

### `form login returned no session cookie`

登录表单被接受了，但响应中没有携带会话 Cookie。通常是该地址指向的并不是 JumpServer 的 web 登录入口，或者反向代理改写了响应。

修复：执行 `jms config test`，并把地址与 web 控制台地址栏中的地址做对比。

### `the passwords do not match`

`jms config add` 中的两个隐藏输入框内容不一致。什么也没有保存。

修复：重新执行 `jms config add bastion`，把两处都重新输入一遍。

## 命令超时与状态标记问题

### `websocket: command did not complete within 30s — either it is still running, or it ends in '#' or '\' (or has unbalanced quotes) and swallowed the status marker: …`

WebSocket 后端在超时前没有看到完成标记。两种不同的原因会给出同样的消息：

| 原因 | 检查 | 修复 |
|---|---|---|
| 命令确实还在运行。 | 该命令在主机上执行很慢。 | 提高超时时间：`jms exec web-01 '…' -t 300`。 |
| 命令以 `#` 或 `\` 结尾，或者引号不配对，导致状态标记被吞掉。 | 远程命令自身的引号处理。 | 改写命令，使其能正常结束。 |

SSH 后端把同样的情况报告为 `command timed out after 30s: …`。

超时是防止状态标记被吞掉的唯一防线：没有它，客户端会一直等待下去。

### `Command exited with status 1`

这不是客户端故障。远程命令执行了并返回了非零状态；该行输出到 stderr，同样的数字也成为了 `jms exec` 的退出码。

### `remote command exited with status 1`

同样的情况，但以错误而非诊断信息的形式报告出来。命令通过 `jms ssh-pipe` 执行时会出现。

## 连接缓慢

用 `--timing` 来定位延迟来自哪一环。它适用于 `jms exec` 和 `jms login`，会向 stderr 每个阶段打印一行：

```console
$ jms exec web-01 hostname --timing
Timing credentials: 12ms
Timing endpoint/auth: 210ms
Timing command: 340ms
```

| 阶段 | 慢意味着 |
|---|---|
| `Timing credentials:` | 操作系统凭据存储响应慢。在 Linux 上 Secret Service 调用上限为 5 s；在 Windows 和 macOS 上为 10 s。 |
| `Timing endpoint/auth:` | 探测（预算 1.5 s）加上登录。API 客户端对传输错误重试 3 次，退避间隔为 0.5 s / 1 s / 2 s，每次请求超时 15 s。 |
| `Timing command:` | 远程命令本身。 |

`jms login --timing` 会额外多出两行：`Timing asset resolve:` 和 `Timing <backend> connect:`。

表现为延迟的连接池行为：

| 行为 | 取值 |
|---|---|
| 终端空闲多久后被回收 | 10 分钟 |
| 资产解析缓存的存活时间 | 5 分钟 |
| 回收器扫描周期 | 60 秒 |
| 失败的命令会被重放 | 仅当它**没有任何输出**时 |

已经产生过输出的命令绝不会被重放，因此慢的命令不会被执行两次。

## SFTP 与 SSH 端口故障

### `xfer: sftp connection token for <asset>: …` 或 `xfer: dial <host>:2222: …`

SFTP 走的是 KoKo 的 SSH 端口，默认值为 `2222`，按服务器以 `ssh_port` 配置。WebSocket 后端不使用它，所以 `exec` 能用而 SFTP 失败是可能的。

修复：确认端口已开放，或者在配置文件中设置它：

```toml
[servers.bastion]
internal = "http://10.0.0.5:2280/"
username = "ops"
ssh_port = 2222
```

### `please select one of the assets`

连接令牌是按错误的协议签发的。SFTP 需要带 `connect_method=web_sftp` 的令牌；`ssh` 或 `web_cli` 令牌会落到 KoKo 的虚拟根目录。

修复：这是服务器侧的令牌配置问题。请让堡垒机管理员签发一个支持 SFTP 的令牌。

### `md5 verification failed: local <hash>, remote <hash>`

文件传输完成了，但摘要不一致。校验会通过 SSH exec 终端在远程主机上运行 `md5sum`，所以也可能因为该命令缺失或 SSH 端口关闭而失败。

修复：用 `--no-verify` 重试，并检查远程主机：

```sh
jms sftp ./file web-01:/tmp/file --no-verify
```

### `recursive directory transfer (-R): requires a transfer seam that carries two resolved sessions (relay) or a directory walk (-R)`

在 CLI 这一层，`-R` 和资产到资产的中继都尚未实现。两者都会返回这个错误。

修复：一次传输一个文件。MCP 那一侧提供了 `jms_sftp_relay` 用于资产到资产的数据流传输。

### `neither argument is a remote path: use <asset>[@<server>]:<path> for the remote side`

`jms sftp` 的两端都是本地路径。方向是根据哪一端是远程来推断的。

修复：本地到本地的复制请用 `cp`，或者明确写出远程端：

```sh
jms sftp ./file web-01:/tmp/file
```

## MCP 客户端问题

### AI 客户端看不到配置

`jms mcp --config <path>` **无法**作用到 MCP 服务器。`--config` 标志由 CLI 自己的配置查找逻辑解析，而 MCP 服务器拿到的是另一个路径。请改用环境变量或按工具传参来指定：

```sh
JMS_CONFIG=/path/to/config.toml jms mcp
```

每个工具还都接受 `config_path` 参数，在该次调用中它的优先级高于环境变量。

### AI 客户端看不到凭据

MCP 进程可能以不同的用户、在不同的会话中，或带着与你的 shell 不同的环境运行。这样它既没有 keyring，也没有 `JMS_PASSWORD_*`。

修复：在 MCP 客户端配置项中显式传入环境：

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

`jms mcp --print-config` 打印的是同样的片段，但不含任何凭据。

### `ERROR: JMS_MCP_ENDPOINT="bogus": must be internal or external`

这个控制变量取了一个服务器不接受的取值。凡是需要会话的工具调用，都会把该错误返回给调用方。

修复：改用 `internal` 或 `external`。

### `ERROR: JMS_MCP_EXEC_TIMEOUT="bogus": must be a duration ("90s", "10m") or a positive number of seconds`

执行超时覆盖值的格式有误。未指定 `timeout` 的 `jms_exec` 调用都会把该错误返回给调用方。

修复：使用 Go 的时长格式（`90s`、`10m`）或一个纯秒数。未设置时的默认值是 5 分钟。

### 后端覆盖值被忽略

`JMS_MCP_BACKEND` 接受 `ssh`、`ws` 或 `auto`。非法取值**不会**被报告：错误在唯一的调用点被吞掉了，服务器会回退到所选 endpoint 上记住的后端，或者 `ws`。请自行检查取值有没有拼错，而不要指望会收到错误。

### 预期的工具不见了

工具集合在启动时确定。`JMS_MCP_TOOLSET=core` 只注册 15 个核心工具；其他任何取值都会注册完整的管理工具集。两个写工具还需要 `JMS_MCP_WRITE=limited`，`JMS_MCP_WRITE_DOMAINS` 则可以进一步收窄它们的范围。

其中任何一个变量的改动都需要**重启** MCP 进程。取消设置某个变量即可恢复之前的能力集合，且不会改动磁盘。

### 工具调用报错，但命令在 shell 里是好的

MCP 失败是以文本形式返回的：结果正文为 `ERROR: <message>`，同时结果被标记为错误。协议本身并没有失败，所以客户端显示的是这段文本，而不是传输层错误。

最常见的原因就是上面那些：缺少环境变量、地址不可达，或者凭据 MCP 进程读不到。

### 结果被截断

管理类读取和 `jms_local_audit` 的输出上限是 32768 字节。被截断的正文以这段结尾：

```
… truncated at 32768 bytes; narrow the filters or lower limit
```

截断发生在序列化后的 JSON 上，因此被截断的正文不是合法 JSON。15 个核心工具不受这个上限限制。

## 审计问题

### `jms tail` 没有任何输出

数据流由审计目录 `<config dir>/audit` 下的按进程划分的 JSONL 文件组成。每个进程写自己的文件，文件名为 `audit-YYYYMMDD-<pid>.jsonl`，多个文件按时间戳合并。

修复：确认目录位置以及其中是否已有文件：

```sh
jms log show --last 20
ls "$(dirname "$(jms config list >/dev/null 2>&1; echo "$XDG_CONFIG_HOME/jms/config.toml")")/audit"
```

`JMS_AUDIT_DIR` 会同时为写入端和读取端重新定位该目录。`JMS_AUDIT=off` 则完全关闭该 sink，于是什么都不会被写入。

### `--since "2" is not a duration: use a form like 1h or 30m`

`--since` 接受的是 Go 时长。纯数字会被拒绝。

修复：`--since 2h`、`--since 30m`。

### `unsupported export format "bad": use md`

`--export` 接受 `md`（或 `markdown`），没有其他取值。

### `no retention window configured: pass --keep <days>, set JMS_AUDIT_KEEP_DAYS, or add audit_keep_days to the configuration file`

没有保留窗口时，`jms log prune` 拒绝删除任何东西。窗口依次取自 `--keep`、`JMS_AUDIT_KEEP_DAYS`，以及配置文件中的 `audit_keep_days`。

修复：三选一。先做一次预览：

```sh
jms log prune --keep 30 --dry-run
```

### `keep days must be at least 1`

窗口为零或负数。在配置文件中零表示"永不清理"，但 `--keep` 标志会拒绝它。

### 预览被截断，且 `JMS_AUDIT_FULL=1` 无法恢复

MCP 层会在事件进入总线之前把预览截断到 4096 字节，因此写入端的 `JMS_AUDIT_FULL` 和 `JMS_AUDIT_PREVIEW_BYTES` 无法恢复已经被丢弃的文本。`JMS_AUDIT_FULL=1` 只对未被截断就到达写入端的预览生效。

### `Warning: observation channel unavailable: ipc: /…/jms-<hash>.sock is already in use`

已经有另一个 `jms mcp` 进程占用了观测套接字。这只是警告，不是故障：MCP 会继续提供服务，但 `jms attach` 无法观测该进程。

修复：停掉那个宿主进程；如果你不需要 `jms attach`，忽略这条警告即可。

## Windows、WSL 与无界面 keyring

| 场景 | 发生了什么 | 修复 |
|---|---|---|
| Linux 桌面，keyring 已解锁 | 凭据从 Secret Service 读取。 | 无需处理。 |
| Linux 桌面，keyring 已锁定 | 在任何解锁尝试之前都会先探测锁定状态，因此命令会立刻以 `the credential store is locked — …` 失败。 | 解锁 keyring，或设置环境变量。 |
| Linux，无图形会话（CI、容器、SSH） | Secret Service 对解锁请求的回应是一个没人能应答的提示框。锁定探测仍然能避免卡死：失败是立即返回，而不是挂起。 | 设置 `JMS_PASSWORD_<ALIAS>`。 |
| WSL | Secret Service 通常不存在，因此存储会把该条目报告为缺失而不是锁定。 | 设置 `JMS_PASSWORD_<ALIAS>`。 |
| Windows | 凭据保存在 Windows 凭据管理器中。条目名称为 `jms-client:<alias>/password`。 | 桌面环境下无需处理；在服务场景下设置环境变量。 |
| macOS | 凭据保存在登录钥匙串中。服务名为 `jms-client`，账户为 `<alias>/password`。 | 桌面环境下无需处理。 |

两条放之四海而皆准的规则：

- 环境变量是**只读**的。`jms config add` 无法写入它；它总是写入操作系统凭据存储。如果存储不可用，环境变量是唯一的途径。
- 变量优先于存储。设置了它时，`jms config list` 的 `Creds` 列会显示 `env`，即使存储里同时也有凭据。

`DISPLAY`、`WAYLAND_DISPLAY` 和 `XDG_SESSION_TYPE` 只影响 "locked" 这条消息的措辞。它们从不决定是否尝试使用凭据存储。

## 资产授权与可见性问题

### 新资产在 `jms ls` 中不可见

新建的资产在获得权限授权之前是**不可连接**的。`jms ls` 列出的是你有权访问的资产；`jms asset list` 列出的是服务器上存在的所有资产。

修复：给它授权。

```sh
jms asset grant web-02 --self
```

或者一步到位地在创建时一并授权：

```sh
jms asset create web-02 192.168.10.12 --account ops --username ops --grant-me
```

`jms asset create` 成功时会打印这段提示：

```
Note: a new asset is not connectable until it is granted to your account;
      pass --grant-me, or run `jms asset grant <name> --self`.
```

### `asset created but the grant failed: …`

资产创建成功了，但授权没有成功。该资产会出现在 `jms asset list` 中，而不会出现在 `jms ls` 中。

修复：重新执行一次授权。

### `at least one user or user group is required: set UserIDs and/or GroupIDs`

授权时没有指定主体。请至少给出 `--user` 或 `--group` 中的一个。

修复：

```sh
jms asset grant web-02 --user ops
jms asset grant web-02 --group platform
```

两者都接受 id，或者**精确**的用户名或组名。匹配到多条记录的名字会被拒绝，而不是随意猜测。

### `no %s matches %q; pass an id or an exact name`

用户、组、资产或权限名称没有解析成功。匹配器从不猜测。

修复：列出资产并找到对应的 id：

```sh
jms asset list
```

### `jms asset permissions <asset>` 没有列出你确知存在的授权

匹配器只比较权限的 `assets` 关系，从不比较 `nodes` 关系。因此，授权的是**节点**而非资产的规则不会被匹配到，即使它覆盖了该资产，也不会出现在按资产列出的结果中。

修复：不加资产过滤，列出所有权限：

```sh
jms asset permissions
```

## 诊断工具参考

| 工具 | 它做什么 | 怎么用 |
|---|---|---|
| `jms config test` | 探测每一个地址（TCP 超时 1.5 s），并在每个可达地址上尝试登录。 | 首先该跑的就是它。`Login` 一列能把网络问题与凭据问题区分开。 |
| `jms exec … --timing` | 向 stderr 打印 `Timing credentials:`、`Timing endpoint/auth:`、`Timing command:`。 | 把一次缓慢运行归因到某个阶段。 |
| `jms login … --timing` | 额外增加 `Timing asset resolve:` 和 `Timing <backend> connect:`。 | 同上，用于交互式会话。 |
| `JMS_WS_DUMP=1` | 把每个 WebSocket 帧以 `[wsdump] +<n> bytes: "<payload>"` 的形式倾倒到 stderr。 | 最后手段。它会打印负载字节，所以不要把输出写进共享日志。 |
| `jms tail` / `jms tail -f` | 跟踪本地审计流。`-f` 每 250 ms 轮询一次；`--json` 输出原始 JSONL；`--last N` 回放最后 N 条事件。 | 观察 MCP 客户端实际在做什么。 |
| `jms log show` | 把审计日志当作文件读取并立即返回。`--since 1h`、`--asset`、`--failed`、`--export md`。 | 回看某个时间窗口。 |
| `jms log prune --dry-run` | 报告保留窗口会删除哪些内容。 | 在删除任何东西之前先跑一遍。 |
| `jms_pool_status` | MCP 调试工具。报告 `terminals:`、`hits:`、`cold:`。 | 只有当 `JMS_MCP_DEBUG` 为 `1`、`true`、`yes` 或 `on` 时才注册。 |
| `JMS_MCP_DEBUG=1` | 注册仅调试用的 `jms_pool_status` 工具。 | 设置之后要重启 MCP 进程。 |

## 已知限制

- **SSH 后端尚未在真实的 JumpServer 上验证过。** 到目前为止接触过的所有部署都禁用了 KoKo 的 SSH 入口。这些代码路径只做了单元测试。
- **`JMS_MCP_BACKEND` 的错误被吞掉了。** 代码注释声称非法取值会被报告，但唯一的调用点丢弃了错误。代码说了算：非法取值会静默回退。
- **`jms mcp --config` 作用不到 MCP 服务器。** 请改用 `JMS_CONFIG` 或按工具传入的 `config_path` 参数。
- **`-R` 和资产到资产的中继在 CLI 这一层尚未实现。** `jms sftp -R` 会返回错误，而不是遍历目录。
- **`jms asset permissions <asset>` 会漏掉只针对节点的授权。** 匹配器从不比较 `nodes` 关系。
- **`jms_asset_update` 无法清空字段**——新的空值会被忽略。CLI 可以做到，因为它会检查该标志是否被改动过。
- **`jms asset update` 每次更新都会重写资产的协议**，改为 `ssh/22` 和 `sftp/22`。
- **删除资产不会撤销引用它的权限。**
- **MCP 写工具从未在真实实例上验证过。** 它们的注册、schema、开关控制和拒绝路径都被走查过，但没有向服务器发送过任何写操作。
- **`JMS_AUDIT_FULL=1` 无法恢复已被 MCP 层截断的预览。**
- **Windows 特有的 IPC 与路径行为只读过源码。** 它们没有在 Windows 上实际执行过。
