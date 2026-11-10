# 地址、故障转移与代理

`jms` 如何为服务器挑选地址、何时故障转移、流量怎么走、用哪个传输后端。
[README](../README.zh-CN.md) 里有简版。
相关页面：[命令参考](command-reference.zh-CN.md) · [MCP 服务](mcp.zh-CN.md) · [配置](configuration.zh-CN.md) · [故障排查](troubleshooting.zh-CN.md) · [English](networking.md)。

## 地址模型

| 键 | 含义 |
|---|---|
| `internal` | 内网可达的地址。 |
| `external` | 外网可达的地址（Web 映射）。 |
| `prefer` | 两个地址里先试哪个：`internal` 或 `external`。不填即 `internal`。 |
| `pin` | 把服务器限定为一个地址。不探测另一个，也不故障转移。 |
| `ssh_port` | KoKo 的 SSH 端口。不填即 `2222`。 |

加载配置时强制执行的规则：

| 规则 | 违反时的错误 |
|---|---|
| `internal` / `external` 至少要填一个。 | `server "bastion": at least one of internal/external URL is required` |
| 每个地址必须是带主机名的绝对 `http://` 或 `https://` URL。裸的 `host:port` 会被拒绝，其他 scheme 同样被拒绝。 | `internal URL "127.0.0.1:2280" is not a valid URL: …` · `internal URL "ftp://127.0.0.1:21" must use http:// or https://` |
| `prefer` 只能是 `internal` 或 `external`。 | `server "bastion": prefer must be "internal" or "external", got "wan"` |
| `pin` 只能是 `internal` 或 `external`。 | `server "bastion": pin must be "internal" or "external", got "wan"` |
| `ssh_port` 必须在 `1..65535`。 | `server "bastion": ssh_port must be between 1 and 65535, got 70000` |

scheme 必须写明，因为它决定了故障转移策略和探测端口。`prefer` 与 `pin` 大小写不敏感，落盘时统一转成小写。

## 候选排序

同一台服务器的地址按下面顺序尝试：

| 优先级 | 来源 | 效果 |
|---|---|---|
| 1 | `pin` | 只剩**一个**候选，就是被 pin 的那种。不探测另一个地址，也不故障转移。服务器没有这种地址时候选集为空，命令直接失败。 |
| 2 | `--endpoint` / `JMS_MCP_ENDPOINT` | 只剩指定的那一种。强制指定绝不静默回退：`server "bastion" has no external address configured; refusing to fall back to the other address`。 |
| 3 | 新鲜的 last-good | 上次成功过的地址，记录时间在 **10 分钟**以内时算新鲜。 |
| 4 | `prefer` | 配置里的偏好。不填即 `internal`。 |
| 5 | 剩下的 | 剩下的那个地址，`internal` 排在 `external` 前面。 |

每种地址最多出现一次。**`pin` 优先于 `--endpoint`**：先应用 pin，所以被 pin 的服务器即使 `--endpoint` 写了另一个地址也不会离开被 pin 的那个。

每个候选都用**一次 TCP 连接后立即断开**来探测，超时 **1.5 秒**。探测端口取自 URL；URL 里没写端口时，`https://` 用 `443`，其他用 `80`。探测失败只会跳过该候选 —— 探测是加速器，不是判决。

### last-good 地址

上次成功过的地址连同时间戳写进 `config.toml` 旁边的 `state.json`，保鲜期 **10 分钟**。

| 方面 | 行为 |
|---|---|
| 文件 | 配置目录下的 `state.json`。`--config` 或 `JMS_CONFIG` 会把它一起挪走。 |
| 内容 | `{"servers": {"<alias>": {"last_good": "internal", "at": "<RFC3339>", "backend": {"internal": "ws"}}}}` —— 字段名取自 struct tag；本次工作中没有真正在磁盘上观察到该文件。 |
| 写权限 | 原子写，`0600`；目录按 `0700` 创建。文件损坏不算错误，只是多付一次探测。 |
| 读取 | **每次**选择都重新读，不缓存：常驻的 MCP 进程和一次性的 CLI 调用共用同一个 `state.json`。 |

地址绑定的是**会话**而不是单次请求。缓存的会话只有在绑定地址与本次选中的地址一致时才会复用；不一致就重新登录。正是这一条防止了长驻进程一直钉在已被放弃的地址上。

## 故障转移规则

选择流程先探测候选，探测通了就发起登录。

| 结果 | 行为 |
|---|---|
| 探测失败 | 记下 `"<kind>: probe failed"`，换下一个候选。 |
| 登录成功 | 记下 last-good 地址，结束。 |
| 登录失败，**网络类**错误 | 记下错误，换下一个候选。 |
| 登录失败，**终止类**错误 | 立即返回。**不故障转移。** |
| 所有探测都失败 | 对第一个候选做一次完整登录尝试。 |
| 全部失败 | `no reachable endpoint: server "bastion": <各次失败拼起来的内容>` |

| 类别 | 例子 | 是否故障转移 |
|---|---|---|
| 终止类 | 状态码为 **400、401、403 或 422** 的 `*api.AuthError` | **不转移。** 服务端看到了凭据并拒绝了，换个地址同样会被拒。 |
| 终止类 | 没提供 MFA、缺少会话 cookie、上下文被取消、deadline 到期 | **不转移。** 要么端点已经应答，要么是你让它停的。 |
| 网络类 | 传输失败、状态码为 `0` 的 `api.APIError`（TCP 通，但后面的服务坏了） | 转移。 |
| 网络类 | 5xx 响应 | 转移。 |

### 只重连一次

连接类失败说明会话或记住的地址可能已经过期。此时 MCP 服务端会**重新选择一次、重试一次**：重读状态文件、重排候选顺序、重新登录。重试只在确实有东西变了的时候发生（换了地址或换了后端），并且**只在失败那次一个字都没输出时**发生。已经输出过内容的命令绝不会再发一遍 —— 远端已经看到它了，而 `rm`/`reboot` 不是幂等的。被取消的调用、命令超时、凭据被拒，都不重试。

连接池在 CLI 上遵守同一条安全规则：没有产生任何输出的失败会在新建的连接上重放一次；已经产生输出之后才失败的，不重放。CLI 在这次重放时不会重新跑一遍端点选择 —— 地址仍是会话所绑定的那个。

### pin 的语义

`pin` 是运维人员的一句承诺："我知道自己在哪个网络里"。

只试一个候选，不探测另一个地址；被 pin 的地址登录失败时原样报错，没有第二个地址可试。pin 到服务器没有的那种地址时，候选集为空：`no reachable endpoint: server "bastion" has no address configured`。

静默回退会掩盖一条坏掉的内网路由时，用 `pin`；只想换个先后顺序时，用 `prefer`。

## 代理策略

**客户端默认直连，并且不读 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`、`NO_PROXY`。**

这是有意为之。开发机上这些变量常常被透明代理工具全局导出，而工具自己的 `DIRECT` 规则又常常命中堡垒机地址。结果是请求通过一个没人要求的代理离开了进程，代理自己的错误（典型是空 body 的 502）冒充了真实网络结果。更糟的是，TCP 探测和登录这时量的是两条不同的路径。本次工作已验证：把 `HTTP_PROXY`、`https_proxy`、`ALL_PROXY`、`NO_PROXY` 全设成无效值，请求依然直连到达目标。

| 取值 | 结果 |
|---|---|
| 不填或空串 | 直连。 |
| `direct`、`none`、`off` | 直连。大小写不敏感。 |
| `http://…` | HTTP CONNECT 代理。 |
| `https://…` | HTTP CONNECT 代理，与代理之间走 TLS。 |
| `socks5://…` | SOCKS5 代理。主机名以域名形式交给代理解析。 |
| `socks5h://…` | SOCKS5 代理。作为另一种写法被接受，但在这里与 `socks5` 行为完全相同。 |

其他写法一律**报错**，绝不静默忽略：

```
proxy "ftp://127.0.0.1:21" has unsupported scheme "ftp" (use http, https or socks5)
proxy "example.com" is missing a scheme (use http:// or socks5://)
proxy "http://" is missing a host
```

全局 `proxy` 写在 `config.toml` 根上，单服务器 `proxy` 写在 `[servers.<alias>]` 里。优先级：服务器自己的值非空时用它，否则用全局值；最终为空就是直连。因此单服务器的 `proxy = "direct"` 会**覆盖**全局代理 —— "继承全局默认"和"强制直连"是两种不同意图，不能坍缩成同一种写法。

策略按服务器解析一次并记在会话上，因此从该会话派生出的每条连接都走同一条路由：REST API、TCP 探测、KoKo WebSocket、KoKo SSH 连接、SFTP 连接。两个例外：

- **`jms ssh-pipe` 不使用配置的代理。** 它的桥接登录与探测都走直连策略（读源码得出，未在运行时复现）。它随后建立的数据连接因此也是直连的。
- **`jms sftp` 使用代理**，SFTP 连接继承会话上的策略。

即使配了代理，`NO_PROXY` 也不会被查。配了代理就适用于所有目标地址。

## 后端

地址选定之后，传输后端决定命令怎么到达资产。

| 方面 | `ws`（WebSocket） | `ssh`（KoKo SSH） |
|---|---|---|
| 路径 | `/koko/ws/terminal/` | TCP 连 KoKo 的 SSH 端口，默认 `2222` |
| 协议 | `http://` 用 `ws`，`https://` 用 `wss` | — |
| 认证 | `jms_sessionid` cookie + `JMS-KOKO` 子协议 | 连接令牌：用户名 `JMS-{token_id}`，用令牌的 `value` 作凭据 —— 不是 password |
| 命令通道 | 一条共享 PTY，由连接池的每终端锁串行化 | 每条命令一个 exec 通道 |
| 退出码 | 从 `__JMSRC:N__` 标记里解析 | **原生**远端退出码 |
| 连接超时 | 15 秒 | TCP 连接 15 秒，SSH banner/握手 2 秒 |
| 保活 | 文本帧 `PING`，每 30 秒一次 | OpenSSH keepalive，每 30 秒一次，连续 3 次丢包才判定断开 |
| 令牌重试 | 2 次，每次换新令牌 | 1 次 |

WebSocket 后端用 `__JMSDONE_<unix_nanos>__` 标记划分输出：**最后两次**出现之间的文本才是命令输出。它还在 PTY 上合并 stdout 与 stderr，然后去掉 ANSI 序列、把 CRLF 折成 `\n`。

`--timeout 0`（默认）时，WebSocket 后端自己加一道 **30 秒**上限，超时报错：

```
websocket: command did not complete within 30s — either it is still running, or it ends in '#' or '\' (or has unbalanced quotes) and swallowed the status marker: command timeout
```

SSH 后端自己没有计时器，只有命令上下文能停它。需要更长的上限就传 `-t`（`jms exec -t 600 web-01 "./build.sh"`）。MCP 的 `jms_exec` 工具默认是 **5 分钟**（`JMS_MCP_EXEC_TIMEOUT` 可覆盖）。

`auto` 先试 **ssh，再试 ws**，谁先成功用谁。每次尝试都会新建一个连接令牌。SSH 排在前面，是因为外网端点常常只放行 WebSocket 路径，而 KoKo 的 SSH 端口被防火墙挡着；2 秒的 banner 超时正是为了让回退到 WebSocket 足够快。两者都失败时报 `no backend connected (<各次失败用 "; " 拼起来>)`。

上次用成功的后端会按地址类型记在 `state.json` 的 `servers.<alias>.backend.<kind>` 下。

| 使用面 | 后端选择 |
|---|---|
| `jms exec` | `ws`。`--backend` 可覆盖。 |
| `jms login` | `ws`。`--backend` 可覆盖；选到 `auto` 时换成所选端点记住的后端。 |
| MCP `jms_exec` | `JMS_MCP_BACKEND`，然后是记住的后端，最后是 `ws`。 |
| 库默认 | 什么都没设时是 `auto`。 |

显式设置的 `JMS_MCP_BACKEND` 不会被质疑。当记住的后端在本次调用里已经失败时，会换成另一个具体后端；`auto` 原样返回，因为它本来就意味着"两个都试"。池化的终端保持它建立时的后端：后端不在池的 key 里。

`jms sftp` 跑在 KoKo 的 SSH 端口上（`ssh_port`，默认 `2222`），这个端口必须可达。连接令牌必须用 `protocol=sftp` 与 `connect_method=web_sftp` 创建；用 `ssh` 或 `web_cli` 的令牌会落到 KoKo 的虚拟根目录下，所有路径都报 "please select one of the assets"。md5 校验是在远端通过 SSH exec 终端跑 `md5sum`，同样依赖这个端口。传输本身按 1 MiB 分块、4 路并发。

## 判定表

| 配置 | `--endpoint` | 实际尝试的候选 | 是否故障转移 |
|---|---|---|---|
| `pin = "internal"` | 不设 | `internal` | 否 |
| `pin = "internal"` | `external` | `internal`（pin 赢） | 否 |
| `pin = "external"` | 不设或 `internal` | `external` | 否 |
| `pin` 指向服务器没有的类型 | 任意 | 无 → `no reachable endpoint` | 不适用 |
| 无 pin，`prefer = "internal"` 或不填 | 不设 | last-good（若新鲜）→ `internal` → `external` | 是 |
| 无 pin，`prefer = "external"` | 不设 | last-good（若新鲜）→ `external` → `internal` | 是 |
| 无 pin | `internal` | `internal` | 否 |
| 无 pin | `external` | `external` | 否 |

`jms login` 在没给 `--endpoint` 时强制 `external`；只有内网地址的堡垒机要加 `--endpoint internal`。`jms exec`、`jms ls`、`jms api get`、`jms asset *`、`jms sftp` 都不强制，由故障转移策略决定。`--endpoint` 存在于 `ls`、`exec`、`login`、`sftp`、`api get` 以及每个 `asset` 子命令上；`attach`、`tail`、`log show`、`config test`、`mcp` 上**没有**。

选中的地址打在 **stderr** 上，形如 `Using <kind> endpoint <url>`，所以 stdout 保持干净表格。

## 配置示例

两个地址、外网优先，其中一台服务器退出全局代理，另一台被 pin 住：

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

看客户端眼里的逐地址结果：

```console
$ jms config test
Server: bastion (default)

Endpoint  URL                              Reachable  Probe  Login
internal  http://192.168.1.10:2280/        yes        0ms    ok
external  https://jumpserver.example.com/  yes        7ms    auth failed

Credentials: keyring
```

`jms config test` 会用 1.5 秒超时探测**每一个**候选，并且给**每一个**可达的地址一次登录尝试，不只是第一个。`Login` 列的取值是 `ok`、`auth failed`、`unreachable`、`failed` 或 `skipped`。

帮助文本写的是"the first reachable one gets a real login attempt"，但实现会给每个可达地址都尝试登录 —— 只看探测结果无法判断哪个地址真的可用。

## 常见故障

| 报错 | 原因 | 处理 |
|---|---|---|
| `no reachable endpoint: server "bastion": internal: probe failed; …` | 没有地址能应答 TCP 连接。 | 查地址、端口和 VPN。`jms config test` 能逐地址拆解。 |
| `server "bastion" has no external address configured; refusing to fall back to the other address` | 在只有内网地址的服务器上用了 `--endpoint external`（或 `jms login` 没带该参数）。 | 加 `--endpoint internal`，或者补上外网地址。 |
| `no reachable endpoint: server "bastion" has no address configured` | `pin` 指向服务器没有的地址类型。 | 改 `pin`，或者补上地址。 |
| `authentication error (HTTP 401): …` | 凭据被拒。这是设计如此，不做故障转移。 | 修凭据。换另一个地址也会同样失败。 |
| `websocket: command did not complete within 30s` | ws 后端自己的上限，或者命令吞掉了状态标记。 | 传 `-t`。把以 `#` 或 `\` 结尾、引号不闭合的命令改掉。 |
| `no backend connected (ssh: …; ws: …)` | 该地址上两个后端都失败了。 | 查 KoKo 的 SSH 端口是否开着，或 WebSocket 路径是否放行。 |
| `handshake: <url> returned HTTP 302` | 地址用重定向应答了升级请求，而不是给出终端。 | 这个地址多半不适合这条路径，换另一个试。 |
| 请求走了你从没配过的代理 | 配置里有 `proxy` 值，或者网络层面存在透明劫持。 | 删掉配置项，或设 `proxy = "direct"`。 |
| `exec` 正常但 SFTP 或 SSH 失败 | KoKo 的 SSH 端口被挡。ws 后端不需要它。 | 放开端口，或改用 ws 后端。 |

## 已知限制

- **SSH 后端没有在真实 JumpServer 上跑过。** 源码自己记着这件事：目前接触过的部署都禁用了 KoKo 的 SSH 入口。相关代码路径只有单元测试覆盖。
- **`jms ssh-pipe` 会忽略配置的代理。** 这是读源码得出的，未在运行时确认。
- **`jms config test` 无法指定某个地址。** 它没有 `--endpoint` 参数，永远按天然的候选顺序走一遍。
- **`state.json` 的确切磁盘内容没有观察到**；字段名来自 struct tag。
- **即使配了代理也不认 `NO_PROXY`。** 没有绕过列表。
- **代理实现是手写的**（HTTP CONNECT 与 SOCKS5，RFC 1928/1929），不引入第三方依赖。
