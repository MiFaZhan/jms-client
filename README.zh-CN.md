# jms-client

[![Release](https://img.shields.io/github/v/release/MiFaZhan/jms-client)](https://github.com/MiFaZhan/jms-client/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](go.mod)

**面向 [JumpServer v4](https://docs.jumpserver.org/zh/v4/) 堡垒机的命令行客户端与 MCP 服务器。**
单个独立二进制，用与网页终端相同的方式访问堡垒机后面的资产 —— 不需要 VPN 客户端，不需要维护 SSH 配置，不需要为每台主机单独管理凭据。

你可以单独用它列资产、跑命令、开交互 shell、传文件；也可以跑 `jms mcp`，让 AI 助手（Claude Desktop、Claude Code、Codex、Cursor、pi 等）通过同一套带连接池、带审计的连接来操作这些资产。

```console
$ jms exec web-01 "systemctl is-active nginx"
active
```

[English](README.md) · **简体中文**

---

## 为什么做这个

要访问一台在堡垒机后面的机器，通常只有两条路：开一个浏览器标签页，或者手工维护一份 `~/.ssh/config`。两条路都没法接进 shell 管道、构建脚本，也没法给 AI agent 用。

`jms-client` 直接对接网页控制台使用的同一套 JumpServer 接口（Core 的 REST API 与 KoKo 终端网关），因此：

- **凭据不落在配置文件里。** 密码与 TOTP secret 存进操作系统凭据库，配置文件只存地址和用户名 —— 所以它可以放心同步、备份，甚至提交进版本库。
- **一次登录服务多条命令。** 连接按 `服务器/资产/账号/协议` 复用，而不是每条命令重新登录一遍。
- **同一份配置在公司内外都能用。** 给一台服务器同时配内网与外网地址，客户端自动挑能通的那个。
- **所有操作用本地日志留痕。** AI agent 执行的每条命令都写进 JSONL 审计流，你可以回看、实时跟随，或直接接进去手动操作。

## 特性

| | |
|---|---|
| **单个静态二进制** | 无运行时依赖。支持 Linux / macOS / Windows 的 x86_64 与 arm64。 |
| **双地址故障转移** | 自动探测并选择内网/外网地址，并记住上次成功的地址以加速冷启动。 |
| **连接池** | 登录态与终端连接按 `服务器/资产/账号/协议` 复用；空闲回收，损坏重建。 |
| **凭据存 OS 凭据库** | Windows Credential Manager、macOS Keychain、Linux Secret Service；容器与 CI 可用环境变量回退。 |
| **MCP 服务器** | 7 个工具，走 stdio；与 CLI 共用同一个带池的长驻进程。 |
| **本地审计流** | 每进程一份结构化 JSONL；`jms tail` 实时跟随，`jms attach` 观察并接入。 |
| **两种终端后端** | WebSocket（默认，与网页终端同源）与 SSH，可自动选择。 |

## 环境要求

- **JumpServer v4**，且 **KoKo** 组件从你的机器可达。不支持 JumpServer v3 及更早版本。
- 在堡垒机上有一个账号，且目标资产已在你的授权范围内。
- WebSocket 后端只要求 HTTP(S) 地址可达。**SFTP 与 SSH 功能额外要求 KoKo 的 SSH 端口（默认 `2222`）在网络路径上开放** —— 不少部署只对外暴露 Web 端口，依赖文件传输前请先确认。

## 安装

### 预编译二进制（推荐）

从 [**Releases**](https://github.com/MiFaZhan/jms-client/releases/latest) 下载对应平台的压缩包，把 `jms`（Windows 为 `jms.exe`）放进 `PATH`。

| 平台 | 文件 |
|---|---|
| Windows x86_64 | `jms-client_<版本>_windows_amd64.zip` |
| Windows arm64 | `jms-client_<版本>_windows_arm64.zip` |
| macOS Apple Silicon | `jms-client_<版本>_darwin_arm64.tar.gz` |
| macOS Intel | `jms-client_<版本>_darwin_amd64.tar.gz` |
| Linux x86_64 | `jms-client_<版本>_linux_amd64.tar.gz` |
| Linux arm64 | `jms-client_<版本>_linux_arm64.tar.gz` |

同一 Release 里的 `checksums.txt` 可用于校验下载：

```sh
sha256sum -c --ignore-missing checksums.txt     # Linux
shasum -a 256 -c --ignore-missing checksums.txt # macOS
```

### Scoop（Windows）

```powershell
scoop bucket add mifazhan https://github.com/MiFaZhan/scoop-bucket
scoop install jms-client
```

后续升级用 `scoop update jms-client`。

### Go 安装

需要 Go 1.25 或更新版本：

```sh
go install github.com/MiFaZhan/jms-client/cmd/jms@latest
```

### 从源码构建

```sh
git clone https://github.com/MiFaZhan/jms-client
cd jms-client
go build -o jms ./cmd/jms
```

### 只用 MCP 集成

如果你只想要 AI 集成，不想自己管理二进制，直接从 [Releases](https://github.com/MiFaZhan/jms-client/releases/latest) 下载 **`jms-client.mcpb`**，用你的 MCP 客户端打开即可（Claude Desktop 等支持 MCPB 的客户端支持双击安装）。这一个文件包含全部 6 种平台/架构组合的二进制，启动时由内置 launcher 自动选择当前平台。

## 快速开始

### 1. 录入服务器

```console
$ jms config add bastion
Internal URL (http(s)://..., empty to skip): https://jumpserver.corp.example.com/
External URL (http(s)://..., empty to skip):
Username: alice
Password:
Confirm password:
TOTP secret (base32, empty to enter the code each time):
Server "bastion" saved to /home/alice/.config/jms/config.toml
Server "bastion" is now the default.
```

两个地址都可以留空，但至少要填一个。凭据会**先**对着一个可达地址验证，验证通过才写盘 —— 密码打错了不会在磁盘上留下任何痕迹。第一个添加的服务器自动成为默认服务器。

### 2. 验证

```console
$ jms config test
Server: bastion (default)

Endpoint  URL                                     Reachable  Probe  Login
external  https://jumpserver.corp.example.com/     yes        12ms   ok

Credentials: keyring
```

`jms config test` 会探测每个已配置地址，并对每个可达地址真的尝试一次登录，所以网络问题与凭据被拒会被分开报告，你也能看清到底哪个地址可用。注意 TCP 探测通过不等于登录能成 —— VPN 或代理可能接住了连接，但后面并没有服务。

### 3. 开始使用

```console
$ jms ls                                   # 列出你有权限访问的资产
$ jms ls -q web                            # 关键词搜索
$ jms exec web-01 "uptime"                 # 单次命令，远端退出码透传
$ jms login web-01                         # 交互式 shell（Ctrl+] 退出）
$ jms sftp web-01:/var/log/app.log .       # 下载
$ jms sftp ./config.yml web-01:/etc/app/   # 上传
```

## 接入 AI 助手（MCP）

`jms mcp` 启动一个基于 stdio 的 [Model Context Protocol](https://modelcontextprotocol.io) 服务器。任何支持 MCP 的客户端都能借此列资产、跑命令、传文件，且共用你 shell 使用的那套连接池。

```json
{
  "mcpServers": {
    "jms": {
      "command": "jms",
      "args": ["mcp"]
    }
  }
}
```

随时用下面这条命令打印这段配置：

```sh
jms mcp --print-config
```

由于 MCP 进程没有命令行参数（客户端只拉起一个长驻进程，无法按调用追加参数），端点、后端与命令超时都通过该客户端条目里的环境变量控制：

```json
{
  "mcpServers": {
    "jms": {
      "command": "jms",
      "args": ["mcp"],
      "env": {
        "JMS_MCP_BACKEND": "ws",
        "JMS_MCP_EXEC_TIMEOUT": "10m"
      }
    }
  }
}
```

### 工具列表

| 工具 | 作用 | 必填参数 |
|---|---|---|
| `jms_ls` | 列出或搜索授权资产 | — |
| `jms_resolve_asset` | 解析资产的地址、账号与协议 | `asset` |
| `jms_exec` | 执行一条命令并返回输出 | `asset`、`command` |
| `jms_sftp_upload` | 上传本地文件到资产 | `src`、`asset`、`dst` |
| `jms_sftp_download` | 从资产下载文件 | `asset`、`src`、`dst` |
| `jms_sftp_relay` | 在两个资产之间直接流式中转文件 | `src_spec`、`dst_spec` |
| `jms_config_list` | 列出已配置服务器及其凭据来源 | — |

设 `JMS_MCP_DEBUG=1` 时会额外注册第 8 个工具 `jms_pool_status`，用于报告连接池的当前形态 —— 这是排查服务端问题的工具，不是日常用法。

多数工具还接受可选的 `server`、`config_path`、`account`、`protocol`、`timeout` 参数。远端路径语法为 `<资产>[@<服务器>]:<路径>`。

写 prompt 时有两个行为值得注意：

- **非零退出码不会被当成失败。** `jms_exec` 返回的是命令输出；远端退出状态会记进审计日志，但不会出现在工具结果里。如果 agent 需要据此分支，就让命令自己说出来（`… && echo OK || echo FAIL`）。
- **命令绝不重放。** 如果失败发生在命令已经投递到远端之后，客户端只上抛错误而不重试 —— `rm`、`reboot` 这类操作不是幂等的。连接断开会触发一次通过重新登录的恢复尝试，但仅限「完全没有产生任何输出」的命令。

## 命令参考

```
jms config  add | list | remove | set-default | test    服务器元数据与凭据
jms ls      [server] [-q keyword] [-n limit] [--endpoint]   列出/搜索资产
jms exec    <target> <command...> [flags]               执行单条命令
jms login   <target> [flags]                            交互式 shell
jms sftp    <src> <dst> [flags]                         上传或下载文件
jms ssh-pipe <target> [command...]                      stdio 桥（供 rsync -e 使用）
jms tail    [-f] [--json] [--last N]                    跟随本地审计流
jms attach  [--attach] [--asset] [--last]               观察运行中的宿主，可选接入
jms log     show [--since] [--asset] [--failed] [--export md]   以文件方式查询审计日志
jms mcp     [--print-config]                            启动 stdio MCP 服务器
jms version                                             打印版本信息
```

**target 语法。** `<资产>@<服务器>` 指定某台服务器上的资产；省略 `@<服务器>` 则用默认服务器。分隔符取**最后一个** `@`，因此资产名本身可以含 `@`。

### `jms config`

| 命令 | 作用 |
|---|---|
| `add <alias>` | 交互录入地址、用户名、密码与可选 TOTP secret；验证后落盘。`--set-default` 同时设为默认。 |
| `list` | 每台服务器一行：地址、用户名、凭据来源，以及哪台是默认。凭据缺失的服务器会在 stderr 上告警。 |
| `remove <alias>` | 删除服务器及其已存凭据。`-y` 跳过确认。 |
| `set-default <alias>` | 切换默认服务器。 |
| `test [alias]` | 探测每个已配置地址，并对可达地址尝试登录。 |

配置文件里一个服务器都没有的状态是写不出去的 —— 否则 `jms config add` 之后读不回自己的文件。因此删除**最后一台**服务器会直接删掉配置文件，回到 `add` 之前的状态，并提示 `no servers remain`。

### `jms ls`

| 参数 | 含义 |
|---|---|
| `-q`, `--search` | 搜索关键词；省略则全量列出。 |
| `-n`, `--limit` | 最大结果数（默认 50）。 |
| `--endpoint` | 强制 `internal` 或 `external`。 |

选中的地址打印到 **stderr**，因此 stdout 是一张干净的、可直接管道的表。

### `jms exec`

| 参数 | 含义 |
|---|---|
| `-t`, `--timeout` | 超时（单位：**秒**）。注意下面那条说明 —— 默认值不是「无限」。 |
| `--backend` | `ws`、`ssh` 或 `auto`（默认 `ws`）。 |
| `--endpoint` | 强制 `internal` 或 `external`。 |
| `--account`、`--protocol` | 覆盖访问该资产所用的账号或协议。 |

命令作为**一个**参数透传，引号由你负责：

```sh
jms exec web-01 'df -h /'                 # 正确：单个参数
jms exec web-01 df -h /                   # 错误：-h 被 jms 解析了，不是远端
jms exec web-01 -- nginx -t               # 远端命令以短横线开头时，用 -- 隔开
```

远端退出码会成为 `jms` 的退出码，所以这样写是有效的：

```sh
jms exec web-01 "test -f /etc/app.conf" && echo "present"
```

诊断信息（`Using external endpoint …`、`Command exited with status N`）都走 stderr，保证 stdout 只有命令输出，方便 `$(…)` 捕获。

> **默认超时取决于后端。** 在 WebSocket 后端（默认）下，不带 `-t` 的 `jms exec` 会被**硬性限制在 30 秒**，更慢的命令会以「did not complete within 30s」失败。凡是耗时可能超过 30 秒的，请显式指定 `-t`：
>
> ```sh
> jms exec -t 600 web-01 "./build.sh"
> ```
>
> MCP 的 `jms_exec` 工具默认是 **5 分钟**，可用 `JMS_MCP_EXEC_TIMEOUT` 调整。另外，以 `#` 或 `\` 结尾、或引号不闭合的命令会吞掉状态标记，同样会撞上超时 —— 错误信息里会提示这种可能，因为从客户端看，它和「命令太慢」长得完全一样。

### `jms login`

打开交互式 shell 并接管本地终端。会话属于该进程，不进连接池。`Ctrl+]` 退出。`--timing` 会打印各阶段耗时，是排查连接慢时第一个该用的东西。

> **`jms login` 默认强制走 external 地址。** 它不会退回 internal，在没有外网地址的服务器上会直接失败：`server "…" has no external address configured`。如果你的堡垒机只有内网地址，请加 `--endpoint internal`。其他命令（`ls`、`exec`、`sftp`）走的是正常的故障转移策略。

### `jms sftp`

必须恰好有一侧是远端写法（`<资产>[@<服务器>]:<路径>`），方向由哪一侧是远端推断。两侧都是本地路径会报错 —— 那种情况请用 `cp`。

| 参数 | 含义 |
|---|---|
| `--verify` / `--no-verify` | MD5 校验。**默认开启**；`--verify` 只是为了对称而存在。 |
| `--chunk` | 分块大小（默认 1 MiB）。 |
| `--parallel` | 并发分块流数量（默认 4）。 |
| `--account`、`--endpoint` | 覆盖账号或强制地址。 |

```sh
jms sftp web-01:/var/log/app.log ./app.log     # 下载
jms sftp ./app.yml web-01:/etc/app/app.yml     # 上传
```

**CLI 目前尚不支持：** 递归目录传输（`-R`）与资产间直连中转。帮助文本里提到了这两个参数，但实际执行会返回错误，而不是悄悄经本地临时文件中转。资产间传输请用 MCP 工具 `jms_sftp_relay`，或在本地机器上分两次 `jms sftp` 中转。这两项都依赖 KoKo 的 SSH 端口（默认 2222）可达。

### `jms ssh-pipe`

把 stdin/stdout 桥接到资产，stdout 上跑的是协议字节。它存在的意义是让 `rsync` 能穿过堡垒机：

```sh
rsync -av -e "jms ssh-pipe" ./dist/ web-01:/opt/app/dist/
```

stdout 上除了协议字节不会有任何东西，所有诊断信息都走 stderr。

### 观察执行记录

| 命令 | 用途 |
|---|---|
| `jms tail -f` | 实时跟随审计流。`--json` 输出原始 JSONL，`--last N` 先回放历史。 |
| `jms attach` | 观察运行中的 `jms mcp` 宿主的事件流。默认只观察。 |
| `jms attach --attach` | 同时启用操作员提示符，可在宿主的连接池上执行命令。 |
| `jms log show` | 以文件方式读取日志：立即返回。支持 `--since 1h`、`--asset`、`--failed`、`--export md`。 |

`jms attach` 在你不加 `--attach` 时**故意不接受任何输入**，这样开着的窗口不会因为一次误触就在生产机器上执行命令。操作员命令与 AI 命令共用同一个连接池，在同一条连接上严格串行，而不是互相抢。

## 配置

配置文件位置：

| 平台 | 路径 |
|---|---|
| Windows | `%AppData%\jms\config.toml` |
| Linux / macOS | `~/.config/jms/config.toml`（或 `$XDG_CONFIG_HOME/jms/config.toml`） |

用 `--config` 参数或 `JMS_CONFIG` 环境变量覆盖。配置文件搬家时审计目录会跟着搬，因为两者同级。

```toml
version = 2
default_server = "bastion"

# 可选：全局代理策略。省略即直连。
# proxy = "socks5://127.0.0.1:1080"

[servers.bastion]
internal = "http://192.168.1.10:2280/"          # 内网地址（可选）
external = "https://bastion.example.com/"       # 外网地址（可选）
username = "alice"

# prefer = "external"       # 优先尝试该地址；默认 internal
# pin = "internal"          # 只用该地址：不探测、不故障转移
# ssh_port = 2222           # KoKo SSH 端口（非默认时填）
# proxy = "direct"          # 本服务器强制直连，覆盖全局 proxy

[servers.staging]
external = "https://jumpserver.staging.example.com/"
username = "alice"
prefer = "external"
```

除用户名外每个字段都可选，但至少要有一个地址。

### 地址是怎么选出来的

当一台服务器同时配了两个地址时，客户端会：

1. 把记住的上次成功地址排在最前（`state.json` 中保留 10 分钟）。
2. 其余按 `prefer` 排序（默认内网优先）。
3. 对每个候选做 1.5 秒 TCP 连接探测。
4. 对第一个能应答的地址登录。

一个地址不通不会导致命令失败 —— 会自动移向下一个。有两个刻意的例外：

- **认证类错误（401/403）绝不触发故障转移。** 密码错的话，换个地址同样会被拒。
- **`pin` 完全关闭故障转移。** 当你明确知道自己在哪个网络里，宁愿看到诚实的报错，也不想被静默回退到另一个地址时使用。

连接失败时客户端会重读状态文件、并通过一次全新登录重试一次，但仅限该命令尚未产生任何输出的情况。

### 凭据

| 数据 | 存放位置 |
|---|---|
| 别名、地址、用户名、偏好 | `config.toml`，明文（0600） |
| **password**、**TOTP secret** | OS 凭据库，service 名为 `jms-client` |

配置文件是原子写入的，且从不含凭据 —— 这正是它能在机器之间放心同步的原因。也要清楚这条边界：它防的是文件外泄和配置被拷到别的机器，防不住以同一用户身份运行的恶意进程。

无桌面环境（容器、CI、没有 Secret Service 的 WSL）可用环境变量回退。**环境变量优先于凭据库。**

| 变量 | 含义 |
|---|---|
| `JMS_PASSWORD_<ALIAS>` | 该服务器别名的密码。 |
| `JMS_OTP_<ALIAS>` | 该服务器别名的 TOTP secret。 |

别名会转成大写并把 `-` 换成 `_`，因此 `bastion-eu` 对应 `JMS_PASSWORD_BASTION_EU`。已导出但值为空的变量按「未设置」处理，而不是当作空密码。两个来源都没有时会明确报错并指出该设置哪个变量 —— 绝不会静默降级为把密码明文落盘。

还有一种情况值得单独说：**凭据库存在，但被锁住了**（GNOME Keyring 在无桌面会话的机器上常是这个状态）。这时命令会在毫秒内失败并说明「凭据库已锁定」，而不是挂住等一个永远不来的解锁提示 —— 错误信息里会直接给出 `JMS_PASSWORD_<ALIAS>` 作为出路。想知道卡在哪一步，加 `--timing`：

### 代理策略

**默认直连，且不读取 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`。**

这是对常见约定的刻意偏离。开发机上这些变量往往被透明代理工具全局导出，而工具自身的 `DIRECT` 规则又常命中堡垒机地址 —— 结果是请求经由一个没人要求的代理离开进程，代理自己的错误（典型是空 body 的 502）被当成了真实网络结果。更麻烦的是，TCP 探测与真实登录会因此走在两条不同的网络路径上。

需要走代理时请显式配置，可全局也可按服务器：

```toml
proxy = "socks5://127.0.0.1:1080"   # 支持 http、https、socks5、socks5h
```

```toml
[servers.bastion]
proxy = "direct"                    # 本服务器强制直连，覆盖全局 proxy
```

已配置的代理优先于环境变量；对它们同样不看 `NO_PROXY`。

### 环境变量

| 变量 | 作用 |
|---|---|
| `JMS_CONFIG` | 配置文件路径。 |
| `JMS_PASSWORD_<ALIAS>`、`JMS_OTP_<ALIAS>` | 凭据回退（见上）。 |
| `JMS_ACTOR` | 审计日志中记录的操作者名；同机多客户端时用它区分。 |
| `JMS_AUDIT` | 设为 `off` 关闭审计日志。 |
| `JMS_AUDIT_DIR` | 更换审计目录。 |
| `JMS_AUDIT_FULL` | 设为 `1` 时记录未截断的输出预览。 |
| `JMS_AUDIT_PREVIEW_BYTES` | 输出预览截断长度（默认 4096）。 |
| `JMS_MCP_ENDPOINT` | 仅 MCP：强制 `internal` 或 `external`。 |
| `JMS_MCP_BACKEND` | 仅 MCP：强制 `ssh`、`ws` 或 `auto`。 |
| `JMS_MCP_EXEC_TIMEOUT` | 仅 MCP：`jms_exec` 默认超时，如 `10m` 或 `600`。 |
| `JMS_MCP_DEBUG` | 仅 MCP：设为 `1` 时注册 `jms_pool_status`。 |
| `JMS_WS_DUMP` | 打印原始 WebSocket 帧，用于协议层排查。 |

## 终端后端

JumpServer 的 KoKo 组件提供两种终端通道，`jms-client` 两种都能说。

| 后端 | 通道 | 说明 |
|---|---|---|
| `ws`（默认） | `/koko/ws/terminal/` | 与网页终端同源。网页终端能用，它就能用。默认命令超时 30s。 |
| `ssh` | KoKo SSH 端口（默认 2222） | 每条命令独立 exec channel，原生退出码，无输出标记解析问题。要求该端口可达；SFTP 也依赖它。 |

用 `--backend ssh` 指定，或用 `--backend auto` 先试 SSH、失败回退 WebSocket。每个地址上次成功的后端会记在 `state.json`，所以冷启动可以直接用它，而不必先在一个被拦的端口上白等一次握手超时。在 MCP 场景下，这个记忆还让进程能从失效连接中自愈而无需重启。

需要 SFTP 的功能 —— `jms sftp`、`jms_sftp_upload`、`jms_sftp_download`、`jms_sftp_relay` —— 都要求 KoKo 的 SSH 端口开放。如果 `exec` 正常但文件传输卡住或失败，第一个该查的就是这个端口。

## 审计日志

每条命令记为一个 JSON 对象、一行一条：

```json
{"ts":"2026-10-05T14:32:01.123+08:00","pid":12345,"actor":"mcp:pi","kind":"exec.end","server":"prod","asset":"web-01","backend":"ssh","endpoint":"external","command":"systemctl status nginx","exit_code":0,"duration_ms":118,"output_bytes":2140,"preview":"● nginx.service ...","pool":"hit","idle_ms":68000}
```

文件写在 `<配置目录>/audit/audit-YYYYMMDD-<pid>.jsonl`，每进程一份，目录与文件权限均为 `0600`。按 pid 分文件既避开了 Windows 上多进程追加同一文件被锁的问题，也让长驻宿主与一次性 CLI 调用能共存。

事件类型：`exec.start`、`exec.end`、`pool.cold`、`pool.hit`、`pool.evict`、`pool.reap`、`session.login`、`session.relogin`、`endpoint.select`、`error`。

输出预览默认截断到 4096 字节（用 `JMS_AUDIT_PREVIEW_BYTES` 调整，或 `JMS_AUDIT_FULL=1` 记全量）。日志中任何地方都不会插入凭据。

## 排障

**`server "…" has no external address configured`**
你对一台只有内网地址的服务器执行了 `jms login`。加上 `--endpoint internal`。

**`command did not complete within 30s`**
你用的是 WebSocket 后端（默认），命令超过了默认超时。用 `-t` 调大。如果命令以 `#` 或 `\` 结尾、或引号不闭合，客户端永远等不到完成标记，报错完全一样 —— 把命令改成能正常结束的形式。

**`no reachable endpoint: … probe failed`**
两个地址都没能应答 TCP 连接。检查配置里的地址是不是你当前网络能到的那个、需要 VPN 是否已连、端口是否正确。跑 `jms config test` 看逐地址的明细。

**请求走了你没配过的代理**
理论上不该发生：客户端不读 `HTTP_PROXY` 这类变量。如果真的看到代理报错，那要么是配置文件中配了 `proxy`，要么是网络层面的透明劫持。删掉配置项，或强制 `proxy = "direct"`。

**`exec` 正常但 SFTP / SSH 失败**
多半是 KoKo 的 SSH 端口（默认 2222）被挡。SFTP 与 `ssh` 后端都跑在它上面，WebSocket 后端不依赖它。

**`jms login` 显示乱码，或退出后终端不正常**
主要见于较老的 Windows 控制台。`jms login` 会在退出时恢复控制台模式；如果是崩溃导致留在 raw mode，Unix 下 `reset`、Windows 下重开终端即可恢复。

**容器里密码提示失败**
容器里没有 OS 凭据库。改用 `JMS_PASSWORD_<ALIAS>`，或配置 Secret Service。`jms config list` 会显示每台服务器的密码从哪来。

**shell 里每条命令都正常，AI 客户端却报错**
再查一遍 `jms config list`：MCP 进程可能由另一个用户或另一套环境启动，因此看不到你的凭据库或 `JMS_CONFIG`。在该 MCP 客户端条目里显式写 `env`。

**连上过，后来就不行了**
空闲连接 10 分钟后会被回收，会话在复用前会重新校验地址。失败时会重读状态文件并重新选择一次地址与后端。如果持续失败，重启 MCP 宿主。

## 限制

- **仅支持 JumpServer v4。** v3 的认证与终端接口不同。
- **`jms sftp` 不支持递归拷贝，也不支持资产间直连中转。** 后者请用 MCP 工具 `jms_sftp_relay`，或经本地机器中转。
- **不构建 32 位平台。** 只有 x86_64 与 arm64。
- **审计日志是本地的。** 不会转发到集中式收集器；需要汇总请自行搬运这些文件。
- **尚未实现跳板机链路（经另一台资产中转）。**

## 开发

```sh
go build ./...     # 编译
go test ./...      # 单元测试与协议回归测试
go vet ./...
gofmt -l .         # 应当无输出
```

集成测试会连真实 JumpServer，且由环境变量门控；不设置则自动 skip，因此新克隆的仓库跑测试是全绿的：

```sh
JMS_TEST_SERVER=bastion go test ./...
```

架构、KoKo 协议逆向结论、连接池设计与失败处理规则见 [DESIGN.md](DESIGN.md)。

发版走 GitHub Actions：

```sh
gh workflow run release --repo MiFaZhan/jms-client -f tag=v0.1.7
```

## 许可

MIT © MiFaZhan。见 [LICENSE](LICENSE)。

设计与 KoKo 协议结论参考了 [GCS-ZHN/jms-cli](https://github.com/GCS-ZHN/jms-cli)。
