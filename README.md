# jms-client

**JumpServer v4 堡垒机的命令行客户端与 MCP 服务器。** 单二进制，用 Go 编写，面向日常运维与 AI 辅助运维场景。

通过 JumpServer 的 REST API 与 KoKo 终端网关访问资产：列出资产、执行命令、交互式 shell、SFTP 传输；同时提供 stdio MCP 服务器，让 Claude Code、Codex、pi 等 AI 客户端直接操作堡垒机资产。

## 特性

- **单二进制** — 静态编译，Linux / macOS / Windows 交叉编译
- **双地址故障转移** — 内网/外网双地址自动探测选择，last-good 记忆加速冷启动
- **连接池** — MCP 场景下登录态与终端连接按 `服务器/资产/账号/协议` 复用，空闲回收、损坏重建
- **凭据不落盘** — 密码与 TOTP secret 存 OS 凭据库（Windows Credential Manager / macOS Keychain / Linux Secret Service），配置文件只有元数据，可备份可同步
- **MCP 服务器** — 8 个工具，供 AI 客户端列资产、执行命令、传输文件
- **本地审计流** — 结构化 JSONL 落盘，`jms tail` 实时观察，`jms attach` 人工接入同一连接池

## 安装

**预编译二进制**（推荐，无需 Go 工具链）：从 [Releases](https://github.com/MiFaZhan/jms-client/releases/latest) 下载对应平台，解压后把 `jms`（Windows 为 `jms.exe`）放进 `PATH`：

| 平台 | 文件 |
|---|---|
| Windows x86_64 | `jms-client_<版本>_windows_amd64.zip` |
| Windows ARM64 | `jms-client_<版本>_windows_arm64.zip` |
| macOS Apple Silicon | `jms-client_<版本>_darwin_arm64.tar.gz` |
| macOS Intel | `jms-client_<版本>_darwin_amd64.tar.gz` |
| Linux x86_64 | `jms-client_<版本>_linux_amd64.tar.gz` |
| Linux ARM64 | `jms-client_<版本>_linux_arm64.tar.gz` |

每个压缩包同时带 `checksums.txt` 可供校验。

**用 Go 安装**（需 Go 1.25+）：

```sh
go install github.com/MiFaZhan/jms-client/cmd/jms@latest
```

**从源码构建**：

```sh
git clone https://github.com/MiFaZhan/jms-client
cd jms-client
go build -o jms ./cmd/jms
```

### MCP 客户端（Claude Desktop 等）

下载 [Releases](https://github.com/MiFaZhan/jms-client/releases/latest) 里的 `jms-client.mcpb` 双击安装。**这一个文件已包含全部 6 个平台**（macOS/Linux/Windows × x86_64/arm64）的预编译二进制，启动时由内置 launcher 自动选择当前平台，无需按平台挑文件。

也可以手动配置，让 MCP 客户端直接调用已安装的 `jms`：

```json
{
  "mcpServers": {
    "jms": { "command": "jms", "args": ["mcp"] }
  }
}
```

## 快速开始

```sh
jms config add bastion        # 录入双地址、用户名、密码（隐藏输入）
jms config test               # 验证两端可达性与登录
jms ls                        # 列出资产
jms exec web-01 "uptime"      # 单次命令，进程退出码即远端退出码
jms login web-01              # 交互式 shell（Ctrl+] 退出）
jms sftp web-01:/etc/hosts .  # 下载文件
jms mcp                       # 启动 stdio MCP 服务器
```

## 配置

配置文件位于 `%APPDATA%\jms\config.toml`（Windows）或 `~/.config/jms/config.toml`（Unix），可用 `JMS_CONFIG` 环境变量或 `--config` 覆盖。

```toml
version = 2
default_server = "bastion"

[servers.bastion]
internal = "http://192.168.1.10:2280/"        # 内网地址（可缺省）
external = "http://bastion.example.com:2280/" # 外网地址（可缺省）
username = "testuser"
# prefer = "internal"                         # 地址偏好，可设 external
# ssh_port = 2222                             # KoKo SSH 端口
# proxy = "direct"                            # 本服务器强制直连，覆盖全局 proxy
```

### 代理

**默认直连，且不读取 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`。** 堡垒机客户端走本机透明代理几乎总是错的：开发机上这些变量是全局导出的，而代理自身的 `DIRECT` 规则又往往命中堡垒机地址，于是请求经由一个没人要求的代理离开进程，代理自己的错误（典型是空 body 的 502）冒充了真实网络结果。

需要跨代理访问时显式配置（`http` / `https` / `socks5`）：

```toml
version = 2
default_server = "bastion"
proxy = "socks5://127.0.0.1:1080"             # 全局；缺省即直连

[servers.bastion]
internal = "http://192.168.1.10:2280/"
username = "testuser"
proxy = "direct"                              # 本服务器强制直连，覆盖全局
```

配置优先于环境：配了代理就连 `NO_PROXY` 也不看。内网地址与 `proxy = "direct"` 都不需要额外设置。

### 凭据

| 数据 | 存放位置 |
|---|---|
| 别名 / 地址 / 用户名 | `config.toml` |
| password / otp_secret | OS 凭据库（service=`jms-client`） |

Headless 环境可用环境变量 `JMS_PASSWORD_<别名>` / `JMS_OTP_<别名>`（大写，`-`→`_`）回退。两处都没有时明确报错，绝不静默降级为明文落盘。

## 命令

```
jms config add|list|remove|set-default|test    # 服务器配置与凭据
jms ls [server] [-q keyword]                   # 列出/搜索资产
jms exec <target> <command...>                 # 执行命令
jms login <target>                             # 交互式 shell
jms sftp <src> <dst> [-R] [--verify]           # 上传/下载/资产间中转
jms ssh-pipe <target> [command...]             # stdio 桥（rsync -e 可用）
jms tail [-f] / jms attach / jms log show      # 审计流观察与接入
jms mcp [--print-config]                       # stdio MCP 服务器
```

`<target>` 语法为 `asset@server`，省略 `@server` 时使用默认服务器。

## MCP

`jms mcp` 暴露 7 个工具：`jms_ls`、`jms_resolve_asset`、`jms_exec`、`jms_sftp_upload`、`jms_sftp_download`、`jms_sftp_relay`、`jms_config_list`。设 `JMS_MCP_DEBUG=1` 时额外注册第 8 个调试工具 `jms_pool_status`。

客户端配置片段：

```json
{
  "mcpServers": {
    "jms": { "command": "jms", "args": ["mcp"] }
  }
}
```

## 后端与兼容性

JumpServer 的 KoKo 组件提供两种终端通道：

| 后端 | 通道 | 说明 |
|---|---|---|
| WebSocket（默认） | `/koko/ws/terminal/` | 与 Web 终端同源，Web 终端可用即可用 |
| SSH | KoKo SSH 端口（默认 2222） | 需部署开放；代码完整但尚未在开放 2222 的部署上验证 |

所有命令默认走 WebSocket；SSH 后端用 `--backend ssh` 显式开启。SFTP 由 KoKo SSH 通道承载，依赖部署开放 2222。

要求：JumpServer v4（含 KoKo）。诊断：`jms login --timing <资产>` 输出分阶段耗时，`JMS_WS_DUMP=1` 打印 WebSocket 原始帧。

## 开发

```sh
go build ./...
go test ./...
go vet ./...
gofmt -l .     # 应为空
```

架构与协议细节见 [DESIGN.md](DESIGN.md)。集成测试由 `JMS_TEST_SERVER` 门控，未设置时自动 skip。

## 许可

MIT © MiFaZhan。设计与 KoKo 协议结论参考了 [GCS-ZHN/jms-cli](https://github.com/GCS-ZHN/jms-cli)。
