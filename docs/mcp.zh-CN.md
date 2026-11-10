# MCP 服务器

`jms mcp` 通过 stdio 运行一个 Model Context Protocol 服务器，让 AI 客户端可以列出资产、执行命令、传输文件，且复用你的 shell 所使用的同一批连接池。
[README](../README.md) 里有更简洁的版本。
相关文档：[English](mcp.md) · [命令参考](command-reference.md) · [配置](configuration.md) · [地址与代理](networking.md) · [故障排查](troubleshooting.md)。

## `jms mcp` 是什么

一个长驻进程，承担三项职责：

| 职责 | 说明 |
|---|---|
| 通过 stdio 提供 MCP 服务 | stdout 是 JSON-RPC 通道。所有诊断信息都走 stderr。在 stdout 上输出任何一行日志都会破坏数据流。 |
| 承载连接池 | 所有工具调用共享同一个连接池，因此针对同一资产的第二次调用会复用已建立的会话，而不会重新登录。 |
| 承载观测通道 | `jms attach` 客户端可以监视同一事件流，并在同一批池化连接上执行命令。IPC socket 就嵌在本进程内。 |

该进程**没有按调用生效的标志**。MCP 客户端只能启动它一次，无法为每次调用传入参数，因此端点、传输后端和执行上限都改用环境变量来控制。

```console
$ jms mcp --help
Usage:
  jms mcp [flags]

Flags:
  -h, --help           help for mcp
      --print-config   Print client configuration snippets (contains no credential)

Global Flags:
      --config string   Configuration file (overrides JMS_CONFIG; overridden by Deps.ConfigPath)
```

`jms mcp` 不接受位置参数（`cobra.NoArgs`）。

### 退出码

| 场景 | 退出码 |
|---|---|
| 客户端断开，或 stdin 到达 EOF | 0 |
| `--print-config` | 0 |
| 协议层失败，例如 stdin 上的 JSON 格式错误 | 1，stderr 上输出 `Error: <message>` |
| 出现非预期参数，例如 `jms mcp bogusarg` | 1，输出 `Error: unknown command "bogusarg" for "jms mcp"` |

有两条警告会写入 stderr，且**不会**改变退出码：`Warning: audit log unavailable: …`（审计目录不可写）和 `Warning: observation channel unavailable: ipc: … is already in use`（socket 已被另一个 `jms mcp` 或 `jms attach` 宿主占用；MCP 服务器仍会继续提供服务）。

## 配置

### 客户端入口项

每个 MCP 客户端都需要读取一份服务器配置项。`jms mcp --print-config` 会把最简单的那种打印到 stdout 并以 0 退出：

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

其中**不含任何凭据**：凭据保存在操作系统的凭据存储中，所以这份配置项可以安全地通过客户端管理器同步。

若要控制该进程，添加一个 `env` 块，并使用 `command` 的绝对路径：GUI 客户端启动服务器时不会经过你的登录 shell，因此 `PATH` 里可能没有 `~/.local/bin`。用 `command -v jms` 确认它的实际位置。

```json
{
  "mcpServers": {
    "jms": {
      "command": "/home/ubuntu/.local/bin/jms",
      "args": ["mcp"],
      "env": {
        "JMS_MCP_ENDPOINT": "external",
        "JMS_MCP_BACKEND": "ws",
        "JMS_MCP_EXEC_TIMEOUT": "10m"
      }
    }
  }
}
```

### Claude Code

Claude Code 把 MCP 服务器保存在 `~/.claude.json` 的 `mcpServers` 下，每个配置项都接受 `command`、`args` 和 `env`。它自己的 CLI 写入的也是同一个文件：

```console
$ claude mcp add jms -- /home/ubuntu/.local/bin/jms mcp
$ claude mcp add --transport stdio jms -e JMS_MCP_BACKEND=ws -- /home/ubuntu/.local/bin/jms mcp
$ claude mcp add-json jms '{"command":"/home/ubuntu/.local/bin/jms","args":["mcp"]}'
$ claude mcp list
```

`claude mcp add` 默认写入 `local` 作用域；`--scope user` 会把它挪到 `~/.claude.json`，从而对所有项目生效。

### Codex

Codex 把 MCP 服务器保存在 `~/.codex/config.toml` 的 `[mcp_servers.<name>]` 下：

```toml
[mcp_servers.jms]
command = "/home/ubuntu/.local/bin/jms"
args = ["mcp"]

[mcp_servers.jms.env]
JMS_MCP_BACKEND = "ws"
```

它的 CLI 写入的也是同一个文件：

```console
$ codex mcp add jms --env JMS_MCP_BACKEND=ws -- /home/ubuntu/.local/bin/jms mcp
$ codex mcp list
```

### Claude Desktop、Cursor 及其他客户端

Claude Desktop 和 Cursor 读取的都是上面展示的那段 `mcpServers` JSON，把它粘贴到客户端各自的 MCP 服务器配置里即可。键名是 `mcpServers`，配置项是 `command` + `args`；至于每个客户端读取哪个文件，那是客户端侧的约定，彼此不同。这里同样请保留 command 的绝对路径，原因还是上面提到的 `PATH` 问题。

协议本身是标准的：在 stdio 上通过 JSON-RPC 交互 `initialize`、`tools/list`、`tools/call`。任何支持 stdio MCP 的客户端都可以驱动该服务器。

### MCPB 打包

发布包中附带一个 MCPB 包（`mcpb/manifest.json`）。它是同一个服务器的便携式封装：它的 `mcp_config` 指向一个启动脚本而不是裸二进制，因为一个包要覆盖六个平台。

```json
"mcp_config": {
  "command": "${__dirname}/launcher.sh",
  "args": ["mcp"],
  "env": {},
  "platform_overrides": {
    "win32": { "command": "${__dirname}/launcher.cmd", "args": ["mcp"] }
  }
}
```

`launcher.sh` 会探测操作系统和 CPU，然后 `exec` 对应的二进制，因此中间经过一层 shell 也不会破坏 JSON-RPC 通道。

## 控制该进程

### 环境变量

| 变量 | 可接受的值 | 作用 | 默认值 |
|---|---|---|---|
| `JMS_MCP_ENDPOINT` | `internal`、`external` | 强制整个进程使用指定的地址类型。 | 已配置的偏好项，加上 `state.json` 中最后一次成功的地址 |
| `JMS_MCP_BACKEND` | `ssh`、`ws`、`auto` | 强制整个进程使用指定的传输后端。 | `state.json` 中为当前所选端点记住的后端，否则为 `ws` |
| `JMS_MCP_EXEC_TIMEOUT` | Go duration（`90s`、`10m`、`1h30m`）或正整数秒 | 未指定 `timeout` 的 `jms_exec` 调用的上限。 | `5m` |
| `JMS_CONFIG` | 文件路径 | 工具读取的配置文件。 | `<UserConfigDir>/jms/config.toml` |
| `JMS_ACTOR` | 任意非空字符串 | 打在审计事件上的操作者名称。 | `mcp` |
| `JMS_PASSWORD_<ALIAS>` | 登录口令 | 某个服务器别名的凭据。别名转为大写，每个 `-` 变为 `_`：`JMS_PASSWORD_BASTION`。 | 未设置 → 改为读取操作系统凭据存储 |
| `JMS_OTP_<ALIAS>` | base32 TOTP 密钥 | 某个别名的 MFA 密钥，别名规则同上。 | 未设置 → 需要 MFA 的登录会被服务器拒绝 |

在表格注明的地方，值会被去除首尾空白且大小写不敏感。无效的 `JMS_MCP_ENDPOINT` 或 `JMS_MCP_EXEC_TIMEOUT` 会被**报告**为工具错误，而不是被悄悄忽略：

```text
ERROR: JMS_MCP_ENDPOINT="bogus": must be internal or external
ERROR: JMS_MCP_EXEC_TIMEOUT="bogus": must be a duration ("90s", "10m") or a positive number of seconds
```

有一个例外：无效的 `JMS_MCP_BACKEND` 会被**静默忽略**，改为使用记住的后端。源码注释声称该错误会被报告，但代码把它丢弃了。不要指望这个变量里的拼写错误能被捕获。

### 工具读取哪个配置文件

| 来源 | 优先级 |
|---|---|
| 工具自身的 `config_path` 参数 | 最高 |
| `JMS_CONFIG` | 其次 |
| 平台默认值 | 最低 |

`jms mcp --config <path>` **传不到** MCP 服务器。这个标志在一条与服务器所获路径不同的路径上解析，因此服务器仍然加载默认配置。请改用 `JMS_CONFIG` 或每个工具自己的 `config_path` 参数来控制。

## 工具清单

工具清单在服务器启动时就确定了。这些开关在注册时读取，所以改动之后需要**重启客户端**。取消设置某个变量即可恢复之前的工具清单，不会对磁盘上的任何内容产生影响。

### 数量

| 开关组合 | 注册的工具数 | 存在的写工具 |
|---|---|---|
| 默认（未设置任何变量） | **29** | 无 |
| `JMS_MCP_TOOLSET=core` | **15** | 无 |
| `JMS_MCP_WRITE=limited` | **31** | 两者都有 |
| `JMS_MCP_WRITE=limited` + `JMS_MCP_WRITE_DOMAINS=node` | **30** | `jms_node_write` |
| `JMS_MCP_WRITE=limited` + `JMS_MCP_WRITE_DOMAINS=account` | **30** | `jms_account_secret` |
| `JMS_MCP_WRITE=limited` + `JMS_MCP_WRITE_DOMAINS=bogus` | **29** | 无 —— 未知域不匹配任何内容 |
| `JMS_MCP_DEBUG=1` | **30** | 无 —— 增加 `jms_pool_status` |
| `JMS_MCP_WRITE=limited` + `JMS_MCP_DEBUG=1` | **32** | 两者都有 —— 工具面最大 |
| `JMS_MCP_TOOLSET=core` + `JMS_MCP_WRITE=limited` | **15** | 无 —— **`core` 优先于 `limited`** |
| `JMS_MCP_WRITE=full`（或任何非 `limited` 的值） | **29** | 无 —— 只有字面量 `limited` 才会启用写操作 |

算式：15 个核心工具 + 14 个管理类读取工具 + 0/1/2 个写工具 + 0/1 个调试工具。

### 开关速查

| 变量 | 可接受的值 | 作用 |
|---|---|---|
| `JMS_MCP_TOOLSET` | `core`（去除首尾空白，大小写不敏感） | 只注册 15 个核心工具。其他任何值（包括空值）都会保留管理工具块。 |
| `JMS_MCP_WRITE` | `limited`（去除首尾空白，大小写不敏感） | 是否注册写工具。`full`、`true`、`1` **不会**注册。 |
| `JMS_MCP_WRITE_DOMAINS` | 以逗号分隔的 `node`、`account` 列表 | 收窄写工具的范围。留空表示两者都允许。出现未知值时两个都不注册。每次调用时都会**重新校验**，不只是在启动时校验一次。 |
| `JMS_MCP_DEBUG` | `1`、`true`、`yes`、`on`（去除首尾空白，大小写不敏感） | 注册 `jms_pool_status`。`0`、`off` 和空值不会注册。 |

有两条规则值得记住：管理类读取工具**默认开启**，写工具**默认关闭**。`core` 胜过 `limited` —— 即使同时设置了 `JMS_MCP_WRITE=limited`，`core` 工具面也没有写工具。

## 15 个核心工具

每个工具除自身参数外，还会接受两个可选参数：

| 参数 | 类型 | 含义 |
|---|---|---|
| `server` | string | 服务器别名。省略时使用默认服务器。 |
| `config_path` | string | 配置文件路径覆盖值。 |

| 工具 | 必填 | 可选 | 作用 |
|---|---|---|---|
| `jms_ls` | — | `keyword` | 列出你可以连接的资产。每个资产一行：`name  address  platform`。列表为空时输出 `No assets found.`。 |
| `jms_resolve_asset` | `asset` | — | 把资产解析为 `name:`、`address:`、`account:`、`protocol:` —— 共四行。 |
| `jms_exec` | `asset`、`command` | `timeout`、`account`、`protocol` | 在资产上运行一条 shell 命令并返回其输出。 |
| `jms_sftp_upload` | `src`、`asset`、`dst` | — | 上传本地文件。结果：`OK: uploaded <src> -> <asset>:<dst> (<bytes> bytes)`。 |
| `jms_sftp_download` | `asset`、`src`、`dst` | — | 下载文件。结果：`OK: downloaded <asset>:<src> -> <dst> (<bytes> bytes)`。 |
| `jms_sftp_relay` | `src_spec`、`dst_spec` | — | 在两个资产之间流式传输文件，不落本地磁盘。结果：`OK: relayed <src_spec> -> <dst_spec> (<bytes> bytes)`。 |
| `jms_config_list` | — | — | 列出已配置的服务器，用 `*` 标记默认项。它只读取文件，从不触碰连接池，因此在任何服务器都还连不上时也能工作。输出中不会出现凭据。 |
| `jms_asset_list` | — | — | 列出服务器上的**全部**资产，而不只是你可以连接的那些。 |
| `jms_asset_show` | `target` | — | 单个资产的详细信息：`ID:`、`Name:`、`Address:`、`Platform:`、`Comment:`、`Nodes:`、`Accounts:`。空值显示为 `-`。 |
| `jms_asset_create` | `name`、`address` | `account`、`username`、`password`、`comment`、`node_id`、`platform_id` | 创建主机类资产。`platform_id` 为 0 表示 Linux。`username` 默认取 `account`。结果：先输出 `Created <name> (id <id>, address <address>).`，随后输出 `The asset is not connectable until it is granted to your account.` |
| `jms_asset_update` | `target` | `new_name`、`comment`、`address` | 更新你设置的字段。只有非空字段会被发送。结果：`Updated <name> (id <id>).` |
| `jms_asset_delete` | `target` | — | 删除资产。**无法撤销，也没有 `confirm` 开关。**结果：`Deleted <name> (id <id>).` |
| `jms_asset_grant` | `target` | `name`、`users`、`user_groups`、`accounts`、`protocols`、`actions`、`comment` | 创建一条 `AssetPermission`。默认值：`name` = `grant-<asset>`，`accounts` = `["@ALL"]`，`protocols` = `["all"]`，`actions` = `["connect"]`。当 `users` 和 `user_groups` 都没给时，认证用户即为主体。结果：`Granted <asset> to <n> user(s) and <m> user group(s) via permission <name> (id <id>).` |
| `jms_asset_permissions` | — | `target` | 列出权限，可以只列出某个资产的。没有权限时输出 `No permissions found.`。 |
| `jms_asset_revoke` | `target` | — | 移除一条权限，资产本身保留。`target` 可以匹配权限的**名称或 id**。结果：`Revoked <name> (id <id>).` |

会影响你做法的几点提示：

- **新建的资产在被授权之前无法访问。**`jms_asset_list` 能看到它，`jms_ls` 看不到。
- `jms_sftp_relay` 接受 `<asset>[@<server>]:<path>` 形式的规格，而不是分开的参数。**第一个**冒号用于分隔主机与路径，因此路径内部的冒号会被保留；主机内部**最后**一个 `@` 用于分隔服务器别名。
- `jms_exec` 的 `timeout` 单位是**秒**。省略或填 `0` 表示使用 `JMS_MCP_EXEC_TIMEOUT`，否则为 5 分钟。
- 当没有任何精确匹配时，`jms_asset_revoke` 会失败并输出 `ERROR: no permission matches "<target>"`。

## 14 个管理类读取工具

它们都同样接受 `server` 和 `config_path`，返回的 JSON 分页结构也完全一致。

| 工具 | 必填 | 可选 | 读取内容 |
|---|---|---|---|
| `jms_audit_list` | — | `kind`、`since`、`until`、`user`、`asset`、`action`、`limit`、`offset` | JumpServer 审计日志。`kind` 可取 `login`（默认）、`operate`、`password`、`ftp` 或 `job`。`since`/`until` 由服务器应用；`user`/`asset`/`action` 则在取回的当前页上匹配。 |
| `jms_session_list` | — | `since`、`until`、`user`、`asset`、`active_only`、`limit`、`offset` | 终端会话：谁在何时连接了哪个资产，以及会话是否仍然打开。 |
| `jms_session_show` | `session_id` | — | 单个会话：参与者、时间信息、是否可回放、能否被终止。 |
| `jms_command_list` | — | `since`、`until`、`user`、`asset`、`input`、`limit`、`offset` | 从会话命令日志中记录的已执行命令。 |
| `jms_node_tree` | — | — | 资产节点树。没有 `offset`/`limit`。 |
| `jms_platform_list` | — | `keyword`、`limit` | 平台模板，以及创建资产时使用的 `platform_id`。没有 `offset`。 |
| `jms_account_list` | — | `asset`、`username`、`privileged`、`limit`、`offset` | 资产账号，**绝不包含其密钥**。提示语始终是 `secret values are never returned`。 |
| `jms_identity_list` | — | `kind`、`org`、`limit`、`offset` | `kind` 可取 `users`（默认）、`groups`、`roles`、`orgs` 或 `org_members`。`org` 会把用户和组成员限定到某一个组织。 |
| `jms_identity_show` | `id_or_name` | `kind` | 按 id 或精确名称查询单个用户、用户组、角色或组织。`kind` 可取 `users`、`groups`、`roles` 或 `orgs`。 |
| `jms_job_list` | — | `keyword`、`since`、`until`、`limit`、`offset` | 作业定义。返回 403 通常意味着该实例关闭了作业执行，而不是路径写错了 —— 用 `jms_audit_list` 配合 `kind=job` 依然能拿到结果。 |
| `jms_job_execution_list` | — | `keyword`、`since`、`until`、`limit`、`offset` | 作业执行记录：运行了什么、何时运行、是否成功。 |
| `jms_ticket_list` | — | `status`、`type`、`applicant`、`limit`、`offset` | 工单（ticket）及其状态和申请人。 |
| `jms_ticket_show` | `ticket` | — | 按 id 查询单个工单。 |
| `jms_local_audit` | — | `since`、`kind`、`asset`、`failed`、`limit` | 本机上的**本地**审计流 —— 与 `jms log show` 打印的是同一批记录。无需服务器权限。`since` 是 Go duration（`24h`、`30m`）；`kind` 是事件类型，例如 `exec.end`、`api.read`、`error`。 |

其中两个工具在缺少参数时以一条简短消息失败，而不是返回分页结果：`ERROR: session_id is required`、`ERROR: ticket is required`、`ERROR: id_or_name is required`。

`jms_local_audit` 在两个方面与其他工具不同：它的 `limit` 默认为 **50**，上限为 **500**，而且超大的 limit 会回落到 50，而不是 500。它的 `note` 是 `newest last` —— 最新的事件位于**末尾**。

## 2 个写工具

默认关闭。只有在 `JMS_MCP_WRITE=limited` 且该域被允许时才会注册。

| 工具 | 域 | 必填 | 可选 | 作用 |
|---|---|---|---|---|
| `jms_node_write` | `node` | `op` | `name`、`parent_id`、`node`、`confirm` | `op` 可取 `create`、`update`（别名 `rename`）或 `delete`。`create` 需要 `name`；`parent_id` 留空表示根节点。`update` 需要 `name`。`delete` 需要 `node` 以及一个 `confirm`，其内容须重复该节点当前的名字或 id。 |
| `jms_account_secret` | `account` | `username`、`confirm` | `asset` | 轮换某个账号的密钥。`confirm` 必须恰好等于 `"<asset>/<username>"`。新密钥在服务端生成，既不会被传入，也不会被返回。 |

两者都会返回一行消息加上美化后的 JSON 正文：

```text
renamed node "old-name" to "new-name"
{
  "id": "…"
}
```

在开关校验失败时，两者都会在发出任何请求**之前**被拒绝，该拒绝会记录为 `api.denied`：

```text
ERROR: jms_node_write: write domain "node" is not enabled
ERROR: jms_node_write: confirm does not match: confirm must repeat the node name "web-01"
ERROR: jms_account_secret: confirm must be "web-01/ops"
ERROR: jms_node_write: unknown op "bogus": use create | update | delete
```

写操作受到三道彼此独立的开关保护：注册（`JMS_MCP_WRITE=limited`）、域列表（`JMS_MCP_WRITE_DOMAINS`）以及 `confirm` 参数。域列表在每次调用时都会重新校验，因此即便某个宿主是以更宽的列表启动的，它仍会拒绝被更窄配置排除的调用。

## `jms_pool_status`（仅调试）

只有在 `JMS_MCP_DEBUG` 为 `1`、`true`、`yes` 或 `on` 时才会注册。它不接受任何参数。

```text
terminals: 2
hits: 7
cold: 1
```

`hits` 和 `cold` 是连接池整个生命周期内的累计值。

## 结果语义

### 成功与失败

| 情形 | 结构 |
|---|---|
| 成功 | 一个文本内容块：`{"content":[{"type":"text","text":"…"}]}` |
| 工具失败 | 结构相同，但带 `isError: true`，文本为 `ERROR: <message>`。**不会**抛出协议层错误。 |
| 未知的工具名 | 一个 JSON-RPC 协议错误：`-32602 unknown tool "jms_x"`。不是工具结果。 |
| 参数格式错误 | `ERROR: invalid arguments: <json error>` |
| 缺少必填参数 | 在处理函数运行之前就被 SDK 拒绝，因为 schema 中声明了 `required`。 |

`ERROR: ` 前缀是刻意设计的，与早先的 Python 实现保持一致，这样已有的提示词仍然可用。

### 32 KiB 结果上限

`maxResultBytes = 32 * 1024` = **32768 字节**。它只作用于 **14 个管理类读取工具和 `jms_local_audit`**，因为只有这些工具会经过分页渲染器。15 个核心工具返回纯文本，不受该上限约束。

被截断的回答会恰好以这段文字结尾：

```text
… truncated at 32768 bytes; narrow the filters or lower limit
```

截断是在序列化后的 JSON 上进行的，因此截断后的正文**不是**合法 JSON。请收窄过滤条件或调低 `limit`，而不要去解析它。

### 分页结构

```json
{
  "count": 30,
  "returned": 1,
  "items": [ … ],
  "note": "…"
}
```

`count` 是服务器针对该查询的总数，`returned` 是本次回答中的记录数，`items` 是当前页，`note` 是可选的 —— 它用于解释被收窄的回答，为空时会被省略。

可能出现的提示语：

| 工具 | 提示语 |
|---|---|
| `jms_audit_list`、`jms_session_list`、`jms_command_list`、`jms_account_list`、`jms_ticket_list`、`jms_platform_list` | `filter applied to the 100 most recent records only, not to the whole collection` |
| `jms_command_list`（当服务器报告没有内容时） | 另外还有 `the server reports no command records; command logging may be off on this instance` |
| `jms_account_list` | 另外还有 `secret values are never returned` |
| `jms_local_audit` | `newest last` |

之所以有这个窗口提示，是因为 `user`、`asset`、`action` 和 `input` 是在**取回的当前页上本地匹配**的，而不是在服务器端匹配。出现这类过滤条件时，工具会把请求扩大到 100 条记录再逐条过滤，因此过滤后的结果是一个被过滤的*窗口*，而不是被过滤的集合。

### 分页

默认每页 20 条，最大 100 条。`limit` 小于等于 0 时回落到 20；`limit` 超过 100 时被截到 100。`jms_local_audit` 默认为 **50**，上限为 **500** —— 但超大的 limit 会回落到 50，而不是 500。用 `offset` 翻页；`jms_node_tree`、`jms_platform_list` 和 `jms_local_audit` 没有 `offset`。

### 远程命令的退出码不会被透出

`jms_exec` 只返回命令的**输出文本**。远程退出码不属于工具结果的一部分；它只会被记录在审计事件里。远程主机上的非零退出码在传输层同样不算错误。

如果你需要退出码，就在命令本身里索取：

```sh
command; echo "exit=$?"
```

### 只重试一次，绝不重放

有两层相互独立的机制共同保证命令不会被发送两次。

| 层次 | 规则 |
|---|---|
| MCP | `jms_exec` 总共最多执行**两次**：第一次尝试，加上通过重新登录进行的**一次**恢复尝试。仅当第一次尝试失败、失败可恢复、第一次尝试**没有产生任何输出**，且重新选定的会话或后端确实**不同于**失败的那一个时，才会重试。 |
| 连接池 | 终端池只会淘汰并重建一个终端并**重放一次**命令，且仅当失败发生在任何输出到达之前。 |

以下情况不会重试，因为命令已经到达远程主机，而 `rm` 或 `reboot` 并不幂等：客户端取消、context 超时、不支持的后端、命令超时，以及任何认证拒绝（HTTP 400、401、403、422）。*连接*超时仍可重试 —— 它从未送达任何内容。

文件传输遵循同样的规则：只有在**没有任何字节被传输**且校验未失败时才会重试。中继会同时刷新两端，因为调用方无法判断是哪一端过期了。

## 实例演示

下面的示例是真正跨越 stdio 的 JSON-RPC。`tools/call` 接收工具名及其参数。

### 列出你可以访问的资产

```json
{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"jms_ls","arguments":{}}}
```

```text
web-01  10.20.0.44  Linux
web-02  10.20.0.45  Linux
```

加上 `keyword` 即可改为搜索：`{"name":"jms_ls","arguments":{"keyword":"web"}}`。

### 在资产上运行命令

```json
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"jms_exec","arguments":{"asset":"web-01","command":"uptime"}}}
```

```text
 14:22:31 up 12 days,  3:07,  2 users,  load average: 0.21, 0.18, 0.15
```

在别名中嵌入服务器即可指向另一台服务器，并用 `timeout`（秒）给调用设限：`{"asset":"web-01@bastion","command":"df -h /","timeout":30}`。

### 上传文件

```json
{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"jms_sftp_upload","arguments":{"src":"/home/ubuntu/app.log","asset":"web-01","dst":"/var/log/app.log"}}}
```

```text
OK: uploaded /home/ubuntu/app.log -> web-01:/var/log/app.log (2048 bytes)
```

上传和下载工具始终会做 md5 比对。

### 授予用户组访问权限

```json
{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"jms_asset_grant","arguments":{"target":"web-01","user_groups":["ops"],"accounts":["ssh"],"protocols":["ssh"],"actions":["connect"]}}}
```

```text
Granted web-01 to 0 user(s) and 1 user group(s) via permission grant-web-01 (id 8c1d0f22-…)
```

省略 `users` 和 `user_groups` 时，认证用户即成为主体。

### 查看会话

先列出，再按 id 取回：用 `jms_session_list` 配合 `{"active_only":true,"limit":10}`，然后用 `jms_session_show` 配合 `{"session_id":"4d0e7a2c-…"}`。

```json
{
  "count": 1,
  "returned": 1,
  "items": [
    { "id": "4d0e7a2c-…", "user": "alice", "asset": "web-01", "date_start": "2026-11-10 14:02:11" }
  ]
}
```

### 轮换账号密钥（需启用写工具）

```json
{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"jms_account_secret","arguments":{"asset":"web-01","username":"ops","confirm":"web-01/ops"}}}
```

```text
requested a secret rotation for ops on web-01
{
  "account_id": "…",
  "asset": "web-01",
  "username": "ops"
}
```

新密钥绝不会出现在结果中。

## 安全说明

### 没有通用的路径类工具

这里刻意**没有**任何接受 `path`、`url` 或 `raw_body` 参数的管理工具，连只读的也没有。MCP 工具的调用方是一个模型，一旦提供 `path` 参数，"读取令牌能看到的任何内容"就只差一句话；而真正重要的边界 —— 哪些字段属于密钥、这份会话日志属于谁 —— 无法用路径前缀来表达。因此每个管理工具都是一个结构固定的动词。有一条测试在守护这一点：它会扫描每个已注册的管理工具 schema，查找这四个名字。

### 写开关

三道开关，依次是：注册、域列表、`confirm`。未注册的工具根本不会出现在 `tools/list` 里，因此无法诱导模型去调用它。`JMS_MCP_WRITE` 必须恰好是 `limited`；`JMS_MCP_WRITE_DOMAINS` 进一步收窄范围；破坏性操作需要一个重复目标名称的 `confirm`。

### `jms_asset_delete` 没有 confirm 开关

`jms_asset_delete` 属于破坏性且不可逆的操作，却不要求 `confirm` 参数。它位于核心工具块中，因此在 `core` 工具面上同样会被注册。如果你不想这样，就别让 MCP 工具面接触到能够无人值守执行操作的客户端。

### 哪些行为会被审计

每次调用都会写入本地 JSONL 审计流，与 `jms log show` 打印的是同一份。

| 事件类型 | 产生方 |
|---|---|
| `exec.start` | 每一个开始执行的 exec、SFTP、资产和授权调用 |
| `exec.end` | 对应的结束事件，包含 `duration_ms`、`output_bytes` 以及截断后的 `preview` |
| `error` | 调用失败，包括从未开始过的失败 |
| `api.read` | 14 个管理类读取工具 |
| `api.write` | 2 个写工具，以服务端的结果为准 |
| `api.denied` | 在本地被拒绝的写调用：域不对、`confirm` 不对 |

审计预览上限为 **4096** 字节。该上限与 32 KiB 结果上限彼此独立，`JMS_AUDIT_FULL=1` 也无法找回已被 MCP 层丢弃的文本。

在会话建立之前发生的失败同样会被记录，因此运维人员可以区分出配置有误和服务器宕机。原因字符串绝不会包含凭据值 —— 只有变量名。

审计操作者默认为 `mcp`；`JMS_ACTOR` 可以覆盖它，这样同一台机器上的多个客户端在共享审计文件中仍能区分开。

### 凭据处理

- 凭据绝不进入配置文件。它保存在操作系统的凭据存储中（Windows Credential Manager、macOS Keychain、Linux Secret Service），位于账号名 `<alias>/password` 之下，或者保存在 `JMS_PASSWORD_<ALIAS>` 中。
- 环境变量优先于凭据存储。这是在无头机器、CI 运行器或 WSL 上运行的正确方式。
- `jms_config_list` 只渲染地址和用户名。凭据绝不会出现在其中。
- `jms_account_list` 绝不返回密钥值，并在提示语中说明了这一点。
- `jms_account_secret` 生成的新密钥在服务端完成生成。它既不会被传入，也不会被返回。
- 错误消息给出的是别名和需要设置的环境变量，而绝不是凭据值。
- MCP 服务器没有终端，因此缺少 TOTP 密钥时会以一条可操作的错误消息失败，而不是弹出交互式提示。

## 已知限制

- 真实客户端如何渲染带 `isError: true` 的工具结果，在这里**没有**被实际验证过：本环境中没有可用的 MCP 客户端。线路上的结构已确认；但客户端是显示这段文本还是中断当前轮次，尚属未知。
- 当 JSON 中含多字节 UTF-8 时，32 KiB 边界处的确切截断行为**没有**在真实服务器上测量过。渲染器是在 32768 字节处切断字节切片，这可能劈开一个 UTF-8 序列。
- MCPB 包的启动脚本只被阅读过，没有被执行。仅验证了 manifest 中的 `mcp_config` 块。
- 写工具和破坏性的 `jms_asset_delete` **没有**在真实的 JumpServer 上验证过。它们的注册、schema、开关和拒绝路径都验证过；没有向任何服务器发送过写操作。
- Claude Desktop 和 Cursor 读取的文件名与路径，在这个 worktree 中**没有**核实过，因为这里没有安装这两个客户端。`mcpServers` 的 JSON 结构就是 `jms mcp --print-config` 打印出来的那个。
- 无效的 `JMS_MCP_BACKEND` 会被静默忽略，尽管源码注释声称它会被报告。
- `jms mcp --config <path>` 传不到 MCP 服务器。请改用 `JMS_CONFIG` 或每个工具自己的 `config_path` 参数。
- `jms_asset_grant` 的描述写的是 "Give at least one of users or user_groups"，但当两者都省略时，该工具会把权限授予认证用户。实际行为以代码为准。
- 工具数量只有结合产生它的开关组合才有意义：默认为 29，`core` 工具面上为 15，最大值为 32。
