# jms-client 架构与设计

面向维护者。用户文档见 [README](README.md) 与 [docs/](docs/)；本文只讲内部结构与取舍。

## 1. 总体架构

`jms` 是一个单二进制程序，两种运行形态共用同一套内核：

| 形态 | 入口 | 生命周期 | 连接池 |
|---|---|---|---|
| CLI | `jms ls/exec/login/sftp/...` | 单次进程，退出即清理 | 进程内，一次性 |
| MCP 宿主 | `jms mcp` | 长驻，由 AI 客户端拉起 | 进程内，含后台回收器与 IPC 宿主 |

两种形态都经由 `internal/cli.Runtime` 构建同一种对象图：配置 → 凭据 → API 客户端 →
端点选择 → 传输层 → 连接池 → 审计总线。CLI 每条命令现建现用；`jms mcp` 只建一次，
之后所有工具调用共享。

`jms mcp` 同时内嵌 IPC 宿主（Unix socket / Windows 命名管道），`jms attach` 连上去
观察同一事件流，`--attach` 时还能在同一批池化连接上执行命令。宿主监听失败只告警，
不阻断 MCP 服务——观测是附加能力。

## 2. 包一览

依赖方向自上而下，`internal/api` 不依赖本模块内任何其他包。

| 包 | 职责 |
|---|---|
| `cmd/jms` | 入口，调用 `cli.Execute()` |
| `internal/cli` | Cobra 命令树、测试缝、每进程 Runtime（池 + 总线 + 审计） |
| `internal/mcpserver` | stdio MCP 服务：默认 29 个工具，写工具开启后 31，调试工具再加 1 |
| `internal/connpool` | `SessionPool`（会话缓存）、`TerminalPool`（活动终端 + 每终端互斥锁） |
| `internal/transport` | KoKo 后端：SSH（`x/crypto/ssh`）与 WebSocket（`gorilla/websocket`），统一 `Terminal` 接口 |
| `internal/transport/console` | 本地终端 raw 模式与窗口尺寸；Unix termios / Windows console API |
| `internal/auth` | 双登录流程（Bearer + 表单 Cookie）、MFA/TOTP |
| `internal/api` | REST 客户端：Cookie jar、重试、错误分类、limit/offset 分页 |
| `internal/assets` | 资产检索与解析（读侧） |
| `internal/assetsmanage` | 资产创建/更新/删除（写侧） |
| `internal/permits` | AssetPermission 的授权、撤销与列表 |
| `internal/mgmt` | 管理面只读 API：审计、会话、命令、作业、工单、身份、节点、平台 |
| `internal/endpoint` | 内部/外部地址排序、TCP 探测、故障转移、last-good 持久化 |
| `internal/netproxy` | 代理策略（direct / http / https / socks5 / socks5h），自行实现，无第三方代理依赖 |
| `internal/config` | TOML v2 元数据存储、凭据存储（环境变量 + 系统钥匙串）、路径解析 |
| `internal/atomicfile` | 临时文件 → fsync → rename，带重命名重试 |
| `internal/obs` | 事件总线（有界环形缓冲、非阻塞发布）、JSONL 审计写入、保留期清理 |
| `internal/ipc` | 本地 IPC 宿主与客户端（Unix socket / Windows 命名管道），JSON-Lines 协议 |
| `internal/xfer` | SFTP 引擎：分块传输、MD5 校验、资产间中继、rsync 桥 |
| `internal/tools` | 仅 `//go:build tools` 的依赖锚点，不编入二进制 |

`AssetCache` 定义在 `internal/connpool`（`ResolveTTL = 5m`）并有单元测试，但生产路径
没有使用它：冷拨号时每次都会重新解析资产。它是预留代码，不是现役缓存。

## 3. 数据流

### 3.1 CLI 执行一条命令

```
解析参数 → 选配置 → 读凭据 → SessionPool 取会话
        → endpoint 选地址 → 登录
        → TerminalPool 取终端（键：server/asset/account/protocol）
        → 创建连接令牌 → transport 执行
        → 审计事件 → 输出
```

### 3.2 `jms login`

交互式登录**不走连接池**：会话属于该进程，终端 raw 模式与窗口尺寸由
`transport/console` 管理，退出即销毁。这是刻意设计——交互会话的输入输出不能与其他
命令共享。

### 3.3 MCP 工具调用

所有 MCP 工具共享 `jms mcp` 进程的同一个池。资产、授权、管理面读工具拿到的是同一份
会话；`jms_exec` 与 `jms_sftp_*` 复用同一批终端。IPC 客户端的操作也落在这个池上，
与 AI 调用串行化在同一终端互斥锁之后，不并发抢占。

## 4. 连接池

### 4.1 对象与键

| 对象 | 键 | 说明 |
|---|---|---|
| `auth.Session` | 服务器别名 | 会话绑定到一个具体地址 |
| `transport.Terminal` | `(server, asset, account, protocol)` | account 与 protocol 进入连接令牌，故必须进键 |

### 4.2 SessionPool

- 命中时校验 `api.NormalizeBaseURL(sess.BaseURL)` 与期望地址一致；不一致则丢弃旧会话、
  重新登录，审计记为 `session.relogin`。
- 同一别名的并发未命中由 `creating` 表合并为一次登录（single-flight）。
- 没有周期探活。调用方遇到 `api.AuthError` 时 `Invalidate(alias)` 并重试一次。

### 4.3 TerminalPool

- **冷路径**：解析资产 → `transport.Connect`（连接令牌在传输层内创建，因为后端不同）
  → 存入 `terminals[key]`，记 `pool.cold`。
- **热路径**：取每终端互斥锁 → 确认仍是池中当前终端（等待期间可能已被回收）→
  `Execute` → 更新 `lastUse`。出错则 `detach + Close`，记 `pool.evict`。
- **回收**：后台循环每 60s 扫描；用 `TryLock` 跳过忙碌终端，只关闭空闲超过
  `IdleTTL = 10m` 的终端。回收器只由 `jms mcp` 启动；一次性 CLI 靠进程退出清理。

### 4.4 锁顺序

`SessionPool.mu` 与 `TerminalPool.mu` 从不跨 I/O 持有；`pooledTerm.mu` 永远第二个获取。
代码中的约束是：“调用方已持有 `t.mu`，池锁只能第二取”。

### 4.5 数值基线

| 参数 | 值 |
|---|---|
| 终端空闲回收 | 10 分钟 |
| 回收扫描间隔 | 60 秒 |
| TCP 拨号超时（SSH/WS/SFTP） | 15 秒 |
| SSH 握手上限 | 2 秒 |
| keepalive | 30 秒，容忍连续 3 次失败 |
| 端点探测 | 1.5 秒 |
| last-good 记忆 | 10 分钟 |
| REST 请求超时 | 15 秒 |
| REST 重试 | 3 次，退避 0.5 / 1 / 2 秒，仅传输错误与 502/503/504 |
| MCP exec 默认超时 | 5 分钟 |
| WS 内层 exec 默认超时 | 30 秒 |
| 审计预览 | 4096 字节 |

## 5. 失败与重放规则

安全核心是“命令最多执行两次，且只在确定没送达时重试”。

`connpool.replayable` 的条件：

- 已有输出 → 不重放；
- 上下文已取消/超时 → 不重放；
- 终端已关闭 → 不重放；
- 阶段错误：只有连接阶段可重放，解析资产失败不重放。

`TerminalPool.Exec` 因此最多执行两次。需要注意两点实现事实：

1. `context.DeadlineExceeded` 不会触发重放；但传输层实际返回的是
   `transport.ErrCommandTimeout`，池层会把它当作可重放。
2. MCP 层在 `recoverable()` 中明确拒绝 `ErrCommandTimeout`，并且要求“没有任何输出”
   且“换一个会话或后端才有意义”才重试一次。因此通过 MCP 调用超时命令不会被重放，
   通过池层直接调用则可能重放一次。这是当前实现，不是文档理想。

传输层对断连的恢复只有一次：重新登录/重选后端后重发；命令一旦开始产生输出，就不再重试。

## 6. KoKo 协议

### 6.1 连接令牌

- 路径常量是 `transport.PathConnectionToken`。本仓库检出中该常量的字面值被脱敏，
  真实路径以部署环境为准。
- 请求体：`{asset, account, protocol, connect_method}`。`account` 必须是账户别名
  （如 `@USER`），不是显示名。
- 缺省：`protocol="ssh"`、`connect_method="web_cli"`。
- SFTP 必须使用 `protocol="sftp"` + `connect_method="web_sftp"`；用 ssh/web_cli 的令牌
  进入 SFTP 子系统会落到 KoKo 虚拟根并报 “please select one of the assets”。
- 令牌每次连接尝试新建，`auto` 后端因此每个后端各拿一个新令牌。

### 6.2 WebSocket 终端

- 路径 `/koko/ws/terminal/`，子协议 `JMS-KOKO`。
- 文本帧承载终端数据；应用层 PING 维持连接，间隔 30 秒。
- 命令结束靠输出标记识别，因此结尾为 `#`、`\` 或引号不配平时可能吞掉标记，表现为超时。

### 6.3 SSH 后端

- 默认端口 2222，原生退出状态，无需输出标记。
- 握手有 2 秒 banner 上限；keepalive 30 秒一次，连续 3 次失败判定断连。
- SFTP 依赖同一端口。

### 6.4 双登录

JumpServer v4 的认证分两步：先取 Bearer token，再用表单登录换 Cookie。两步都由
`internal/auth` 完成；MFA/TOTP 在第二步提交。API 客户端随后同时携带所需凭据。

### 6.5 `auto` 顺序

`auto` 先试 SSH，再退 WebSocket。上次成功的后端记在 `state.json`，冷启动直接使用，
避免在被封端口上白等一次握手。

## 7. 端点故障转移与代理

候选顺序：`pin` > 强制端点 > 新鲜的 last-good > `prefer` > 其余地址（internal 在前）。

- `pin` 只返回一个候选，不探测另一个地址，也不故障转移。
- `--endpoint` / `JMS_MCP_ENDPOINT` 强制失败时报错，不静默回退。
- 401/403 不触发故障转移：密码错误换地址也没用。
- 连接失败后重读状态文件，换地址/后端重试一次，前提是命令尚未产生输出。

代理默认直连，**不读取** `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`。必须显式配置，
全局 `proxy` 或每服务器 `proxy`，后者覆盖前者；`direct` 表示强制直连。支持
`http`、`https`、`socks5`、`socks5h`。探测与实际登录走同一条代理路径。

## 8. 可观测性

### 8.1 审计

每个进程写自己的 `audit-YYYYMMDD-<pid>.jsonl`，目录 0700、文件 0600。一行一个 JSON
对象，字段包括 `ts`、`pid`、`actor`、`kind`、`server`、`asset`、`backend`、`endpoint`、
`command`、`exit_code`、`duration_ms`、`output_bytes`、`preview`、`pool`、`idle_ms`。
事件类型：`exec.start`、`exec.end`、`pool.cold`、`pool.hit`、`pool.evict`、`pool.reap`、
`session.login`、`session.relogin`、`endpoint.select`、`error`。

`obs.Event` 结构里没有凭据字段；错误信息不嵌入请求头；管理面事件对查询参数中的
敏感值做掩码。预览默认截断到 4096 字节，`JMS_AUDIT_FULL=1` 关闭截断。
`JMS_AUDIT=off` 关闭写盘；`JMS_AUDIT_DIR` 改目录；`audit_keep_days` /
`JMS_AUDIT_KEEP_DAYS` 控制保留期，0 表示永不清理，上限 3650 天，由 `jms log prune`
执行。

### 8.2 IPC

`jms mcp` 内嵌宿主，环形缓冲 512 条事件；客户端通过 JSON-Lines 订阅。`jms attach`
默认只读，`--attach` 才开放操作提示符。

## 9. 安全边界

- 凭据只进系统钥匙串或 `JMS_PASSWORD_<ALIAS>` / `JMS_OTP_<ALIAS>` 环境变量；环境变量优先。
  配置文件 schema 没有密码字段，写盘前还有正则守卫拒绝 `password=`、`otp_secret=`、
  `secret=` 与 `enc:v1:` 标记。
- 连接令牌只在内存中，每次连接新建，不落盘。
- Linux 钥匙串读取改用自带 Secret Service 客户端：每次 D-Bus 调用 5 秒超时，先查
  `Locked` 属性，锁定即快速失败，不触发无人应答的解锁提示。写入仍走 go-keyring。
- 文件权限：`config.toml`、`state.json` 0600，目录 0700，审计目录与文件每次启动重新
  chmod。Windows 上 Unix mode 无效，`config.toml` 继承 `%AppData%` 的 ACL；只有 IPC
  管道有显式的仅所有者 SDDL。
- 管理面 MCP 工具不接受通用 `path` 参数，写入需要 `JMS_MCP_WRITE=limited`、域名白名单
  和调用级 `confirm` 三重条件。
- 错误信息只报别名和环境变量名，不报值。

## 10. 测试策略

- 单测与协议回归测试默认全绿，不依赖真实堡垒机。
- 真实环境集成测试由 `JMS_TEST_SERVER=<alias>` 门控，未设置时跳过。
- 传输层用 httptest/假 WebSocket 服务器验证帧序、标记解析与超时行为；MCP 层用内存
  传输验证工具注册表与 schema。
- 池层有跨资产并行性测试和“命令不重复执行”测试；新增重放规则时必须同时补测试。

## 11. 技术栈

| 层 | 选择 |
|---|---|
| 语言 | Go 1.25 |
| CLI | spf13/cobra |
| MCP | modelcontextprotocol/go-sdk |
| SSH | golang.org/x/crypto/ssh |
| WebSocket | gorilla/websocket |
| TOML | BurntSushi/toml |
| 凭据 | zalando/go-keyring + 自带 Linux Secret Service 客户端 |
| 发布 | GoReleaser + GitHub Actions；MCPB 打包；MCP Registry 发布 |
