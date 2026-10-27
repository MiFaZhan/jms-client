# 命令参考

每个子命令的完整说明、参数与行为。
[README](../README.zh-CN.md) 里有简版。


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
