# jms-client

[![Release](https://img.shields.io/github/v/release/MiFaZhan/jms-client)](https://github.com/MiFaZhan/jms-client/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](go.mod)

**[JumpServer v4](https://docs.jumpserver.org/zh/v4/) 堡垒机的命令行客户端与 MCP 服务器。**
一个静态可执行文件，像 Web 终端一样访问堡垒机背后的资产 —— 不需要 VPN 客户端，不需要 SSH 配置，也不需要为每台主机单独保存凭据。

```console
$ jms exec web-01 "systemctl is-active nginx"
active
```

[English](README.md) · **简体中文** · **文档：** [命令参考](docs/command-reference.zh-CN.md) · [MCP](docs/mcp.zh-CN.md) · [配置](docs/configuration.zh-CN.md) · [网络](docs/networking.zh-CN.md) · [故障排查](docs/troubleshooting.zh-CN.md) · [设计](DESIGN.md)

## 为什么需要它

访问堡垒机通常意味着开一个浏览器标签页，或者自己维护一份 `~/.ssh/config`。两者都很难和命令行管道、CI 任务或 AI 智能体组合。`jms-client` 调用的是 Web 控制台用的同一套 JumpServer API（Core 的 REST API 与 KoKo 终端网关）：

- **不用 VPN，不用 SSH 配置，不用逐主机凭据。** 配置一个服务器别名，客户端完成登录后即可访问其背后的资产。
- **一次登录，服务多条命令。** 登录按服务器池化，终端按 `server/asset/account/protocol` 池化，两者都会复用。
- **同一份配置在办公网内外都能用。** 给服务器配上内网与外网两个地址，客户端依次探测、切换到能连上的那个，并记住最近一次可用的地址。
- **所有操作本地留痕，AI 助手复用同一批连接。** `jms mcp` 以 stdio 方式提供 Model Context Protocol 服务，Claude Desktop、Claude Code、Codex、Cursor 等客户端使用的是同一批池化且可审计的连接。

## 环境要求

- **JumpServer v4**，且 **KoKo** 组件能从你的机器访问。JumpServer v3 及更早版本不受支持。
- 一个已获得目标资产授权的 JumpServer 账号。
- WebSocket 终端后端只要求能访问一个 HTTP(S) 地址。SFTP 与 SSH 后端还需要 KoKo 的 SSH 端口（默认 `2222`）在网络路径上可达 —— 很多部署只开放 Web 端口。

## 安装

预编译压缩包发布在 [**Releases**](https://github.com/MiFaZhan/jms-client/releases/latest)。把 `jms`（Windows 上是 `jms.exe`）放进 `PATH` 即可。

| 平台 | 压缩包 |
|---|---|
| Linux x86_64 | `jms-client_<version>_linux_amd64.tar.gz` |
| Linux arm64 | `jms-client_<version>_linux_arm64.tar.gz` |
| macOS Intel | `jms-client_<version>_darwin_amd64.tar.gz` |
| macOS Apple Silicon | `jms-client_<version>_darwin_arm64.tar.gz` |
| Windows x86_64 | `jms-client_<version>_windows_amd64.zip` |
| Windows arm64 | `jms-client_<version>_windows_arm64.zip` |

每个压缩包内还包含 `LICENSE` 与 `README.md`。用同一发布中的 `checksums.txt` 校验下载：Linux 用 `sha256sum -c --ignore-missing checksums.txt`，macOS 用 `shasum -a 256 -c --ignore-missing checksums.txt`。

**Scoop（Windows）** —— 清单位于 `MiFaZhan/scoop-bucket` 仓库，应用名为 `jms-client`：

```powershell
scoop bucket add mifazhan https://github.com/MiFaZhan/scoop-bucket
scoop install jms-client
```

**Go**（需 1.25 或更高版本）—— `go install github.com/MiFaZhan/jms-client/cmd/jms@latest`。**源码编译** —— `git clone https://github.com/MiFaZhan/jms-client && cd jms-client && go build -o jms ./cmd/jms`

**MCPB 打包** —— 从 [Releases](https://github.com/MiFaZhan/jms-client/releases/latest) 下载 `jms-client.mcpb`，用支持 MCPB 的客户端打开。它内置了上表六种平台/架构组合的二进制文件和一个自动选择程序的启动器，不需要安装 Go 工具链。

## 快速开始

凭据会先在可达地址上验证通过，之后才落盘，因此输错的值不会留下任何痕迹；凭据进入操作系统凭据存储，不会写进 `config.toml`。内网与外网地址可以留空其一，但至少要填一个；添加的第一个服务器会成为默认服务器。目标写作 `<asset>` 或 `<asset>@<server>`，不带 `@<server>` 时使用默认服务器。

```console
$ jms config add bastion
Internal URL (http(s)://..., empty to skip): https://jumpserver.example.com/
External URL (http(s)://..., empty to skip):
Username: alice
Password:
Confirm Password:
TOTP secret (base32, empty to enter the code each time):
Server "bastion" saved to /home/alice/.config/jms/config.toml
Server "bastion" is now the default.
$ jms config test
Server: bastion (default)

Endpoint  URL                                Reachable  Probe  Login
external  https://jumpserver.example.com/    yes        12ms   ok

Credentials: keyring
$ jms ls
Using external endpoint https://jumpserver.example.com/
Name     Address     Platform  Type
web-01   10.20.0.11  Linux     Host
db-01    10.20.0.32  Linux     Host

Total: 2 asset(s)
$ jms exec web-01 "uptime"
 14:22:31 up 12 days,  3:07,  2 users,  load average: 0.21, 0.18, 0.15
$ jms login web-01
Connected to web-01 (ws backend). Press Ctrl+] to exit.
$ jms sftp web-01:/var/log/app.log .
Transferred 2048 bytes in 120ms
MD5 verified: 5d41402abc4b2a76b9719d911017c592
```

## 接入 AI 助手（MCP）

`jms mcp` 通过 stdio 运行一个 [Model Context Protocol](https://modelcontextprotocol.io) 服务器。任何支持 MCP 的客户端都可以借助与命令行相同的池化连接来列出资产、执行命令、传输文件。

```json
{
  "mcpServers": {
    "jms": { "command": "jms", "args": ["mcp"], "env": { "JMS_MCP_ENDPOINT": "external", "JMS_MCP_BACKEND": "ws", "JMS_MCP_EXEC_TIMEOUT": "10m" } }
  }
}
```

`jms mcp --print-config` 会打印同样的配置片段（不含 `env` 一段），其中不含任何凭据。MCP 进程没有按调用传参的开关 —— 客户端只会启动一个长期运行的进程 —— 因此地址类型（`internal`|`external`）、传输后端（`ssh`|`ws`|`auto`）与命令超时都通过环境变量控制。注意 `jms mcp --config <path>` **不会**影响 MCP 服务器，请改用 `JMS_CONFIG` 或工具参数 `config_path`。

| 分组 | 工具 | 数量 |
|---|---|---|
| 核心 | `jms_ls`、`jms_resolve_asset`、`jms_exec`、`jms_sftp_upload`、`jms_sftp_download`、`jms_sftp_relay`、`jms_config_list`、`jms_asset_list`、`jms_asset_show`、`jms_asset_create`、`jms_asset_update`、`jms_asset_delete`、`jms_asset_grant`、`jms_asset_permissions`、`jms_asset_revoke` | 15 |
| 管理类只读 | `jms_audit_list`、`jms_session_list`、`jms_session_show`、`jms_command_list`、`jms_node_tree`、`jms_platform_list`、`jms_account_list`、`jms_identity_list`、`jms_identity_show`、`jms_job_list`、`jms_job_execution_list`、`jms_ticket_list`、`jms_ticket_show`、`jms_local_audit` | 14 |
| 写操作（默认关闭） | `jms_node_write`、`jms_account_secret` | +2 |
| 调试（默认关闭） | `jms_pool_status` | +1 |

实际注册的工具数：默认 **29** 个（15 个核心 + 14 个管理类只读）；`JMS_MCP_TOOLSET=core` 时 **15** 个；`JMS_MCP_WRITE=limited` 时 **31** 个；`JMS_MCP_WRITE=limited` 与 `JMS_MCP_DEBUG=1` 同时设置时为上限 **32** 个。写工具**默认关闭**：`JMS_MCP_WRITE` 必须正好是 `limited`，`JMS_MCP_WRITE_DOMAINS` 可将范围收窄到 `node`、`account` 或两者，破坏性操作还需要 `confirm` 参数。这些开关在服务器启动时读取，修改后需要重启客户端。

**详情：** [docs/mcp.zh-CN.md](docs/mcp.zh-CN.md) · [English](docs/mcp.md)

## 开通资产与权限

JumpServer 把“资产存在”和“用户可以访问”分成两件事：只有 `AssetPermission` 记录才能把资产暴露给用户。因此新建的资产会出现在 `jms asset list`（管理员视图）中，却**不会**出现在 `jms ls`（当前用户视图）中，直到授权为止。`--grant-me` 把这两步合成一条命令：

```console
$ jms asset create "web-02" 10.20.0.44 --account ops --username ops --password '…' --grant-me
Created web-02 (id 3f9c2a1e-8d44-4c0a-9d0e-1b7c5e2a4411)
Granted to the current user via permission grant-web-02
$ jms asset grant web-02 --user alice --group ops
Granted web-02 to 1 user(s) and 1 user group(s) via permission grant-web-02 (id 8c1d0f22-...)
$ jms asset permissions web-02
Name          Users   Groups  Assets  Nodes  Protocols  Actions
grant-web-02  alice   ops     web-02  -      all        connect

Total: 1 permission(s)
$ jms asset revoke grant-web-02 --yes
Revoked grant-web-02 (id 8c1d0f22-...)
```

不带 `--grant-me` 时，创建命令会在 stderr 打印提示，说明资产暂时还不可连接，退出码仍为 0。`--user` 接受用户 ID 或精确的用户名，`--group` 接受用户组 ID 或精确的组名，两者都可以重复传入或用逗号分隔。既不给 `--user` 也不给 `--group` 时，默认授权给当前登录用户；`--self` 是叠加语义。服务端默认值为：账号 `@ALL`、协议 `all`、动作 `connect`。`jms asset permissions` 会列出用户、用户组、资产与节点，因此只有组或只有节点的规则同样可见。删除资产不会连带删除引用它的权限规则。

**详情：** [docs/command-reference.zh-CN.md](docs/command-reference.zh-CN.md) · [English](docs/command-reference.md)

## 命令一览

所有顶层命令及其子命令。`--config <path>` 是唯一的全局参数；完整参数表见 [docs/command-reference.zh-CN.md](docs/command-reference.zh-CN.md)。

| 命令 | 子命令 | 作用 |
|---|---|---|
| `jms api` | `get <path> [server]` | 按路径读取管理 API（只读，受路径白名单限制） |
| `jms asset` | `list`、`show`、`create`、`update`、`delete`、`grant`、`permissions`、`revoke` | 管理资产以及谁可以访问它们 |
| `jms attach` | —— | 观察运行中主机的事件流，可选执行命令 |
| `jms completion` | `bash`、`fish`、`powershell`、`zsh` | 生成 shell 补全脚本 |
| `jms config` | `add`、`list`、`remove`、`set-default`、`test` | 管理服务器配置及其凭据 |
| `jms exec` | —— | 在资产上执行一条命令 |
| `jms login` | —— | 打开资产的交互式 shell |
| `jms ls` | —— | 列出或搜索你有权访问的资产 |
| `jms log` | `show`、`prune` | 查询与清理本地审计日志 |
| `jms mcp` | —— | 运行 stdio MCP 服务器 |
| `jms sftp` | —— | 上传或下载文件 |
| `jms ssh-pipe` | —— | 把 stdio 桥接到资产（供 `rsync -e` 使用） |
| `jms tail` | —— | 跟踪本地审计流 |
| `jms version` | —— | 打印版本信息 |

## 配置

| 平台 | 路径 |
|---|---|
| Linux | `$XDG_CONFIG_HOME/jms/config.toml`，未设置时为 `~/.config/jms/config.toml` |
| macOS | `~/Library/Application Support/jms/config.toml` |
| Windows | `%AppData%\jms\config.toml` |

`--config` 优先于 `JMS_CONFIG`，后者优先于平台默认路径。根级别只有五个键 —— `version`、`default_server`、`proxy`、`audit_keep_days` 与 `[servers.<alias>]` —— 就是全部配置结构。每个服务器至少要配置 `internal` / `external` 之一；`prefer` 决定优先尝试哪个地址，`pin` 把服务器锁定在单个地址上（不再探测、不做故障切换），`ssh_port` 覆盖 KoKo 的 SSH 端口。

```toml
version = 2
default_server = "bastion"
# proxy = "socks5://127.0.0.1:1080"     # 全局代理；不写表示直连
# audit_keep_days = 90                  # 保留天数；0 表示永不清理
[servers.bastion]
internal = "http://192.168.1.10:2280/"       # 可选
external = "https://jumpserver.example.com/" # 可选
username = "alice"
```

| 环境变量 | 作用 |
|---|---|
| `JMS_CONFIG` | 配置文件路径 |
| `JMS_PASSWORD_<ALIAS>` | 该别名的凭据（别名转为大写，`-` 换成 `_`） |
| `JMS_OTP_<ALIAS>` | 该别名的 TOTP 密钥 |
| `JMS_ACTOR` | 写入审计事件的操作者名称 |
| `JMS_AUDIT`、`JMS_AUDIT_DIR`、`JMS_AUDIT_KEEP_DAYS` | 关闭审计日志、迁移审计目录、设置保留天数 |
| `JMS_MCP_ENDPOINT`、`JMS_MCP_BACKEND`、`JMS_MCP_EXEC_TIMEOUT` | 仅 MCP：地址类型、传输后端、命令超时 |

**详情：** [docs/configuration.zh-CN.md](docs/configuration.zh-CN.md) · [English](docs/configuration.md) · [docs/networking.zh-CN.md](docs/networking.zh-CN.md)

## 安全与审计

- 凭据与 TOTP 密钥保存在操作系统凭据存储中（Windows 凭据管理器、macOS 钥匙串、Linux Secret Service），服务名为 `jms-client`。配置文件以 `0600` 权限原子写入，永远不包含凭据。
- 环境变量优先于凭据存储，这是无桌面环境（SSH、CI、容器）下的推荐用法。导出但取值空的变量视为未设置。
- 每条命令都会写入本地 JSONL 审计流，位置为 `<config dir>/audit/audit-YYYYMMDD-<pid>.jsonl`，每个进程一个文件，权限 `0600`。命令行会记录连接池与会话事件；MCP 侧会为每次工具调用记录 `exec.start` / `exec.end`。输出预览截断到 4096 字节，疑似凭据的查询参数会被掩码。
- `jms log show` 查询这条审计流，`jms tail` 跟踪它，`jms attach` 观察运行中主机的实时事件。

## 已知限制

- **仅支持 JumpServer v4。** v3 的认证与终端 API 不同。
- **不支持跳板链。** 没有实现经由另一台资产代理或跳转。
- **`jms sftp` 不支持递归复制，也不支持资产到资产的中转。** MCP 工具 `jms_sftp_relay` 可以中转，命令行不行。
- **不构建 32 位版本。** 只有 x86_64 与 arm64。
- **审计日志只在本机。** 不会转发到集中式采集器，需要汇总请自行搬运文件。
- **`jms asset permissions <asset>` 只匹配资产关联关系。** 授权的是节点而非资产本身的规则不会出现在该资产的列表中，而且这条路径上一次读取详情失败会被静默跳过。
- **管理类写操作有开关限制。** MCP 写工具需要 `JMS_MCP_WRITE=limited`；`jms_asset_delete` 与 `jms_asset_revoke` 没有确认参数，而它们的命令行版本会提示确认。
- **SSH 后端尚未在真实堡垒机上验证过**，多个部署本身就关闭了 KoKo 的 SSH 入口。

## 文档

- [docs/command-reference.zh-CN.md](docs/command-reference.zh-CN.md) —— 全部命令、参数、默认值与退出码
- [docs/mcp.zh-CN.md](docs/mcp.zh-CN.md) —— MCP 配置、工具、开关与错误语义
- [docs/configuration.zh-CN.md](docs/configuration.zh-CN.md) —— 配置结构、凭据存储、审计格式
- [docs/networking.zh-CN.md](docs/networking.zh-CN.md) —— 地址、故障切换、代理与后端
- [docs/troubleshooting.zh-CN.md](docs/troubleshooting.zh-CN.md) —— 现象 → 原因 → 处理
- [DESIGN.md](DESIGN.md) —— 架构、协议细节与失败处理规则（中文）

## 开发

构建、测试与格式化：`go build ./... && go test ./... && go vet ./... && gofmt -l .`，其中 `gofmt` 必须没有输出。测试全部基于本地假服务运行，不需要真实堡垒机，因此全新克隆的仓库测试是绿的。发布通过 GoReleaser 完成，`scripts/release.sh` 会更新 MCPB manifest 并发布 Scoop 清单位。

## 许可

MIT © MiFaZhan，详见 [LICENSE](LICENSE)。设计与 KoKo 协议方面的结论来自 [GCS-ZHN/jms-cli](https://github.com/GCS-ZHN/jms-cli)。
