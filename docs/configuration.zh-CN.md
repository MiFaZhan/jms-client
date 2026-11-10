# 配置

配置文件、凭据、端点记忆和审计日志分别放在哪里、各自怎么被读取。
[README](../README.zh-CN.md) 里有简版。
相关页面：[命令参考](command-reference.zh-CN.md) · [MCP 服务](mcp.zh-CN.md) · [地址与代理](networking.zh-CN.md) · [故障排查](troubleshooting.zh-CN.md) · [English](configuration.md)。

## 文件都在哪里

配置目录里放四样东西：

```
<config dir>/
  config.toml                  0600  只有元数据，不含凭据
  state.json                   0600  最近可用的地址和记住的后端
  audit/                       0700
    audit-YYYYMMDD-<pid>.jsonl 0600  每个进程一个文件，JSONL
    .prune-stamp               0600  RFC3339 UTC 时间戳，限制自动清理频率
  jms-<hash>.sock              0600  `jms attach` 用的 Unix IPC socket
```

`<config dir>` 是放着 `config.toml`、`state.json` 和 `audit/` 的目录。设置了 `JMS_CONFIG` 时它是 `dirname(JMS_CONFIG)`；否则是平台配置根目录加 `jms`。

| 操作系统 | 平台配置根目录 | 配置目录示例 | 文件示例 |
|---|---|---|---|
| Linux / 其他 Unix | `$XDG_CONFIG_HOME`，未设置则 `$HOME/.config` | `~/.config/jms` | `~/.config/jms/config.toml` |
| macOS | `$HOME/Library/Application Support` | `~/Library/Application Support/jms` | `~/Library/Application Support/jms/config.toml` |
| Windows | `%AppData%`（Roaming） | `C:\Users\<u>\AppData\Roaming\jms` | `C:\Users\<u>\AppData\Roaming\jms\config.toml` |

### 读的是哪个文件，会带走什么

| 优先级 | 来源 | 说明 |
|---|---|---|
| 1 | `--config <path>` | 根命令的持久化标志，所有子命令都有 |
| 2 | `JMS_CONFIG=<path>` | 任意非空值，原样使用 |
| 3 | 平台默认路径 | `<平台配置根目录>/jms/config.toml` |

由嵌入方以编程方式设置的路径（命令行无法触达）优先级高于以上三者。和 `JMS_CONFIG` 一样，它会搬走整个目录；和 `--config` 不同，它连 `state.json` 和 `audit/` 一起搬。

`JMS_CONFIG` 会搬走整个目录：`config.toml`、`state.json`、`audit/` 都跟着走。

`--config` **只**改变 `config.toml` 的位置。`state.json` 和 `audit/` 是按配置目录解析的，不看这个标志。用 `--config` 指向某个临时目录、然后在它旁边找审计日志的人会找不到 —— 日志不在那里。

```console
$ JMS_CONFIG=/tmp/jms/config.toml jms config list
$ jms --config /tmp/jms/config.toml config list    # 读的是同一个文件
```

MCP 服务端认 `JMS_CONFIG`，不认 `--config`：`jms --config other.toml mcp` 依然加载默认配置，因为这个标志不会传进 MCP 服务端的配置路径。

### 文件与目录权限

上面每个文件都是 `0600`，所在目录是 `0700`。目录不存在时按 `0700` 创建；审计目录每次写入都会额外 `chmod` 成 `0700`，所以早先遗留的宽松目录会被收紧。文件是整体替换而不是原地改写，所以早先留下的全局可读 `config.toml` 在下一次保存后会变成 `0600`。配置文件原子写入：同目录临时文件 → `fsync` → `rename`。

在 Windows 上这些 mode 不起作用，由 ACL 决定。

## 配置文件

文件是 TOML，schema 版本为 2。

### 结构

顶层键：

| 键 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `version` | float | `2` | 以整数字面量写出。`0` 或缺省读作 `2` |
| `default_server` | string | `""` | 默认服务器的别名，必须指向已存在的服务器 |
| `proxy` | string | `""` | 全局代理，空值表示直连 |
| `audit_keep_days` | int | 缺省 | `0` 或缺省表示"永不清理"。取值范围 `0`–`3650` |
| `[servers.<alias>]` | table | — | 至少要有一个服务器 |

`[servers.<alias>]` 内的键：

| 键 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `internal` | string | `""` | 带主机名的绝对 `http://` 或 `https://` URL |
| `external` | string | `""` | 同上 |
| `username` | string | — | 必填，去掉空白后不能为空 |
| `prefer` | string | `""` | `internal` 或 `external`；空值表示 internal |
| `pin` | string | `""` | `internal` 或 `external`；把服务器锁定在一个地址上，不做故障切换 |
| `proxy` | string | `""` | 单服务器覆盖项。`direct`、`none`、`off` 强制直连 |
| `ssh_port` | int | `0` | KoKo SSH 端口。`0` 即 `2222` |

`internal` 与 `external` 至少要有一个。

### 带注释的示例

```toml
version = 2                      # 总是写在第一行
default_server = "bastion"        # 目标未指定服务器时使用的别名
proxy = "http://proxy:3128"       # 全局代理；空值表示直连
audit_keep_days = 90              # 清理 90 天以前的审计文件

[servers.bastion]
internal = "https://bastion.internal.example.com"   # 办公网内可达
external = "https://bastion.example.com"            # 任何网络可达
username = "operator"
prefer = "external"               # 先试 external；空值表示 internal
pin = ""                          # 空值：两个都探测，可故障切换
proxy = ""                        # 空值：继承全局 proxy
ssh_port = 2222                   # 写 0 也是 2222
```

服务器按别名排序写出，文件 diff 干净。`default_server` 与 `username` 总是写出，其余键为空时省略。

### 校验规则

下面每条规则在加载时校验一次，保存前再校验一次。被拒绝的条目不会截断已有文件。

| 规则 | 错误信息 |
|---|---|
| 别名非空 | `server alias must not be empty` |
| 有 username | `server %q: username is required` |
| internal/external 至少一个 | `server %q: at least one of internal/external URL is required` |
| 绝对 http(s) URL 且带主机名 | `internal URL %q must use http:// or https://` / `internal URL %q is missing a host` |
| `prefer` 取值 | `server %q: prefer must be "internal" or "external", got %q` |
| `pin` 取值 | `server %q: pin must be "internal" or "external", got %q` |
| `ssh_port` 范围 | `server %q: ssh_port must be between 1 and 65535, got %d` |
| `proxy` 语法 | `server %q: proxy %q ...` |
| `audit_keep_days` 范围 | `invalid config %s: audit_keep_days must be between 0 and 3650, got %d` |
| 至少一个服务器 | `no servers configured: <path> contains no servers — run \`jms config add <alias>\`` |
| default 存在 | `invalid config %s: default_server %q does not exist (available: %s)` |
| 文件非空 | `config file is empty: <path>` |
| TOML 合法 | `parse config %s: ...` |

proxy 语法接受空值、`direct` / `none` / `off`（不区分大小写），以及带主机名的 `http`、`https`、`socks5`、`socks5h`。没有 scheme 的值报 `proxy %q is missing a scheme (use http:// or socks5://)`；scheme 不认识报 `proxy %q has unsupported scheme %q (use http, https or socks5)`。裸的 `host:port` 会被当成 scheme，所以报的是"不支持"而不是"缺 scheme"。

`HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY` 一律不读。要走代理必须显式配置。

加载器和凭据库会抛出的哨兵错误：`ErrNotFound`（`credential not found`）、`ErrReadOnly`（`credential store is read-only`）、`ErrConfigNotFound`（`config file not found`）、`ErrNoServers`（`no servers configured`）、`ErrServerNotFound`（`server not found`）、`ErrLegacyConfig`（`detected legacy jms-cli config`）、`ErrCredentialStoreLocked`（`credential store is locked`）。

### 版本处理

`version` 按写入的值原样保存。加载器不按数字分支：`version = 1`、`2`、`3`、`0` 都能加载，写着 `version = 1` 的文件存回去还是 `version = 1`。只有 `0` 和缺省会被规范成 `2`。**这份代码里没有 v1 到 v2 的 schema 迁移。**

唯一被拒绝的"迁移"是旧版 Python `jms-cli` 的 `config.yaml`。当 `config.toml` 缺失、但旁边有一个 `config.yaml` 时，加载失败并报 `detected legacy jms-cli config — unsupported by design; re-add with jms config add (found <path>)`。名为 `config.yaml` 的*目录*不会触发。

### 会写文件的命令

| 命令 | 对文件的作用 |
|---|---|
| `jms config add <alias>` | 依次询问两个地址、用户名、Password（隐藏输入，需确认）和可选的 TOTP 密钥。先拿可达地址验证凭据，再写进操作系统凭据库，最后写元数据文件；写失败会回滚凭据。`--set-default` 同时切换默认服务器 |
| `jms config remove <alias>` | 先写元数据文件，再删凭据。删掉最后一个服务器会直接删除整个文件，因为 `Save` 拒绝写出没有服务器的配置 |
| `jms config set-default <alias>` | 切换默认服务器 |
| `jms config list` | 只读。`Creds` 列说明凭据来自哪里：`env`、`keyring`、`missing` 或 `locked` |

凭据不会进入 `config.toml`。

## 凭据

### 存在哪里

| 顺序 | 后端 | 读 | 写 |
|---|---|---|---|
| 1 | 环境变量 | 可以 | 不行 —— 返回 `ErrReadOnly` |
| 2 | 操作系统凭据库 | 可以 | 可以 |

**环境变量优先于操作系统凭据库。** 即使密钥环里也有条目，只要变量被设置，`jms config list` 就显示 `env`。

各平台的系统凭据库：Linux 是 Secret Service（D-Bus 上的 `org.freedesktop.secrets`），macOS 是 Keychain，Windows 是 Windows Credential Manager。

服务名：`jms-client`。账户名：登录密钥用 `<alias>/password`，TOTP 密钥用 `<alias>/otp_secret`。凭据类型是 `password` 和 `otp`；未知类型会被拒绝，拼错不会覆盖登录密钥那一条。

### 环境变量命名规则

`JMS_PASSWORD_<ALIAS>` 放登录密钥，`JMS_OTP_<ALIAS>` 放 TOTP 密钥。别名转大写，每个 `-` 变成 `_`，其他字符一律不改。所以 `bastion` 读 `JMS_PASSWORD_BASTION`，`my-wan` 读 `JMS_PASSWORD_MY_WAN`，`web-01` 读 `JMS_PASSWORD_WEB_01`。

变量已导出但取值为空、或只有空白字符时，算作**不存在**，而不是一个空凭据 —— 把空凭据发给服务端比报告"没有凭据"更糟。

### 凭据库被锁住

凭据库被锁与凭据不存在是两回事：条目在，但无人值守读不出来。Linux 上会先探测锁状态再读，因此无桌面会话的机器是毫秒级失败，而不是卡在一个没人回答的提示框上。对凭据库的每次调用都有时间上限 —— Linux 上每次 D-Bus 往返 5 秒，其他平台整个 go-keyring 调用 10 秒。

读的时候报 `the credential store is locked — unlock the keyring, or set JMS_PASSWORD_<ALIAS>`。删除的时候结尾是 `unlock the keyring before removing credentials`。Linux 上写入的时候结尾是 `unlock the keyring before storing credentials`；有桌面会话时改成 `unlock the keyring from the desktop session first`。

Linux 上是否存在桌面会话由 `DISPLAY`、`WAYLAND_DISPLAY` 或 `XDG_SESSION_TYPE` 决定。它只影响消息措辞，从不影响是否去尝试读凭据库。

### 永不落盘、永不记录的内容

- 配置文件里没有凭据字段：schema 里根本没有放的地方，写出来的文件也就带不了。包里有一个守卫，任何声明了凭据字段或带有加密凭据标记的渲染结果都会被它拒绝。
- 错误信息和提示只给出环境变量名，不给值。
- 审计日志的查询参数会被掩码：参数名里含 `password`、`passwd`、`secret`、`token`、`key`、`passphrase` 或 `otp` 的，值替换成 `***`；其他值截断到 128 字节。

## `state.json`

位置：`<config dir>/state.json`。它跟着 `JMS_CONFIG` 走，**不受** `--config` 影响。

```json
{
  "servers": {
    "bastion": {
      "last_good": "internal",
      "at": "2026-11-10T14:47:06.00579509+08:00",
      "backend": { "internal": "ssh", "external": "ws" }
    }
  }
}
```

| 字段 | 含义 | 有效期 |
|---|---|---|
| `last_good` | 上次成功的地址类型，下次运行优先试它 | 10 分钟。过期后由配置里的 `prefer` 决定 |
| `at` | 那次选择发生的时间 | 同上 |
| `backend` | 各地址类型上生效过的传输后端 | 无 TTL。一直保留直到被覆盖；空值会被忽略，所以后一次运行抹不掉前一次学到的结果 |

文件缺失或损坏不算错误，只是多付一次探测。文件在每次端点选择成功后写入，写入失败绝不会把一次成功的登录变成失败的命令。MCP 宿主每次工具调用都会重读它，因此别的进程记录的后端无需重启就能被采纳。

## 审计日志

### 目录与文件命名

| 对象 | 取值 |
|---|---|
| 默认目录 | `<config dir>/audit` |
| 改位置 | `JMS_AUDIT_DIR` 优先于推导出的目录，写入方和所有读取方都认它 |
| 文件名 | `audit-YYYYMMDD-<pid>.jsonl`，日期取写入进程当时的本地日期 |
| 每进程一个文件 | 多个 MCP 客户端同时写入不会争抢同一个文件 |

CLI 在第一个事件到来时才打开文件，所以 `jms version` 这类只读命令不会留下空文件。`jms mcp` 在启动时就建好审计目录，因此位置有问题会在任何工具调用之前报出来。

`JMS_AUDIT=off`（不区分大小写）停用写入端。事件总线仍然运行，所以 `jms attach` 和 `--last` 回放照常工作。关闭审计不算错误，也不会让命令失败。

### 记录格式

每行一个 JSON 对象：

| 键 | 类型 | 含义 |
|---|---|---|
| `ts` | RFC3339 | 事件时间 |
| `pid` | int | 进程号，由写入方标记，保证与文件名一致 |
| `actor` | string | `cli`、`mcp`，或 `JMS_ACTOR` 覆盖值 |
| `kind` | string | 事件类型，见下表 |
| `server` | string | 服务器别名 |
| `asset` | string | 资产名 |
| `backend` | string | `ssh` 或 `ws` |
| `endpoint` | string | `internal` 或 `external` |
| `command` | string | 远端命令 |
| `exit_code` | int | 远端退出码，仅在已知时出现 |
| `duration_ms` | int64 | 墙钟耗时 |
| `output_bytes` | int | 输出大小，不含预览 |
| `preview` | string | 截断后的输出预览 |
| `pool` | string | `cold`、`hit`、`evict` 或 `reap` |
| `idle_ms` | int64 | 终端的空闲时长 |
| `reason` | string | 简短原因 |
| `error` | string | 错误文本 |
| `path` | string | 管理 API 请求路径，不含查询串 |
| `params` | string | 查询串，疑似凭据的值已掩码 |
| `status` | int | 管理调用的 HTTP 状态码 |

只有 `ts`、`pid`、`kind` 必然出现。

```json
{"ts":"2026-11-10T15:44:32.947322402+08:00","pid":2885952,"actor":"cli","kind":"api.denied","reason":"path is not allowed for read-only access: the path must start with /api/v1/","path":"../etc/passwd"}
```

### 事件类型

| 类型 | 产生方 |
|---|---|
| `exec.start` | 仅 MCP |
| `exec.end` | 仅 MCP |
| `pool.cold`、`pool.hit`、`pool.evict`、`pool.reap` | CLI 与 MCP |
| `session.login`、`session.relogin` | CLI 与 MCP |
| `error` | MCP，以及 CLI 的失败管理调用 |
| `api.read` | `jms api get` 和 MCP 管理只读工具 |
| `api.write` | MCP 写工具 |
| `api.denied` | 请求发出前就被本地拒绝的调用 |
| `endpoint.select` | 任何非测试代码都不会发布；常量存在，但没有代码路径发出它 |

CLI 会发布连接池与会话事件，但不会发布 `exec.start` / `exec.end` —— 这两种类型只来自 MCP 侧。

### 预览截断

预览按 4096 字节截断，写入端的截断不会从 UTF-8 字符中间切开：它会回退到最近的字符边界再截。

| 变量 | 效果 |
|---|---|
| `JMS_AUDIT_FULL=1` | 写入方不截断预览 |
| `JMS_AUDIT_PREVIEW_BYTES` | 调整上限。不是正整数的取值会被忽略并保留默认值，审计照常继续 |

`JMS_AUDIT_FULL=1` 也救不回 MCP 层已经丢掉的文本：MCP 路径在事件进入总线之前就把预览截到 4096 字节，而且那里是按字节切的，所以来自 MCP 侧的预览可能停在字符中间。CLI 根本不发布预览。

### 保留期

| 来源 | 作用 |
|---|---|
| 配置文件里的 `audit_keep_days` | 保留天数。`0` 或缺省表示永不清理 |
| `JMS_AUDIT_KEEP_DAYS` | 覆盖配置文件的值。`0` 表示永不清理 |
| `jms log prune --keep <days>` | 两者都覆盖，仅对本次运行有效。`--keep 0` 不是"永不清理"，它会退回使用配置的窗口 |

窗口的解析顺序是 `--keep` → `JMS_AUDIT_KEEP_DAYS` → `audit_keep_days`。三者都没有时，`jms log prune` 直接拒绝执行：

```
no retention window configured: pass --keep <days>, set JMS_AUDIT_KEEP_DAYS, or add audit_keep_days to the configuration file
```

清理时读的是**文件名**里的日期，不是修改时间，所以复制过来的目录不会悄悄把文件永久留下。截止点是今天减窗口天数的本地零点；日期不早于截止点的文件保留。文件名里没有八位日期的文件跳过，不删。目录不存在不算错误。

清理也会在启动时自动跑一次，挂在根命令的 pre-run 钩子上，因此一次性的 `jms exec` 和常驻的 `jms mcp` 执行的是同一个窗口。频率由 `.prune-stamp` 文件限制为每 24 小时一次。`JMS_AUDIT_KEEP_DAYS` 格式非法时在 stderr 上报错并按"关闭"处理：

```
jms: JMS_AUDIT_KEEP_DAYS="abc" ignored: keep days "abc" is not a number
```

清理失败只报在 stderr 上，绝不让命令失败。

### 读日志

`jms tail` 跟踪这条流，`jms log show` 把它当文件查询。

| 命令 | 参数 | 效果 |
|---|---|---|
| `jms tail` | `-f`/`--follow`、`--json`、`--last N` | 合并目录下所有 `audit-*.jsonl` 并按时间戳排序。`--follow` 每 250 毫秒轮询一次，按文件跟踪字节偏移；末尾不完整的一行先不消费。Ctrl-C 可干净退出 |
| `jms log show` | `--since`、`--asset`、`--failed`、`--export md` | 读取日志并立即返回。`--since` 只接受 Go duration（`1h`、`30m`）；裸数字会被拒绝，报 `--since "2" is not a duration: use a form like 1h or 30m`。`--export md` 输出 Markdown 表格，列为 `Time`、`Server`、`Asset`、`Command`、`Exit`、`Duration`、`Actor` |
| `jms log prune` | `--keep`、`--dry-run`、`--dir` | 删除超出窗口的文件。`--dry-run` 只报告不删除 |

对 `jms tail` 来说，审计目录缺失不算错误，只表示流是空的。`jms log show` 没有匹配项时打印 `No audit events matched.`。

## 环境变量

所有被读取的变量，一次列全。

| 变量 | 可接受的值 | 默认 | 读取方 | 作用 |
|---|---|---|---|---|
| `JMS_CONFIG` | 任意非空路径 | 平台配置目录 | `internal/config` | 配置文件路径。同时决定 `state.json` 和 `audit/` 的位置 |
| `JMS_PASSWORD_<ALIAS>` | 登录密钥 | 未设置 → 查操作系统凭据库 | `internal/config` | 某个别名的凭据。只读：`jms config add` 写不了它 |
| `JMS_OTP_<ALIAS>` | base32 TOTP 密钥 | 未设置 → 运行时提示输入 MFA | `internal/config` | 某个别名的 TOTP 密钥。只读 |
| `JMS_AUDIT` | `off` | 审计开启 | `internal/obs` | 停用审计写入端，总线仍在 |
| `JMS_AUDIT_DIR` | 目录路径 | `<config dir>/audit` | `internal/obs`、`internal/cli` | 改变审计目录，写入方和读取方一起变 |
| `JMS_AUDIT_FULL` | 只能是 `1` | 关闭 | `internal/obs` | 预览不截断 |
| `JMS_AUDIT_PREVIEW_BYTES` | 正整数 | `4096` | `internal/obs` | 预览上限。格式非法则保留默认值 |
| `JMS_AUDIT_KEEP_DAYS` | 非负整数，`0` 表示永不清理 | 取配置文件的值 | `internal/cli` | 保留天数 |
| `JMS_ACTOR` | 任意非空字符串 | `cli` / `mcp` | `internal/cli` | 打在审计事件上的 actor 名 |
| `JMS_MCP_TOOLSET` | `core` | 管理类工具注册 | `internal/mcpserver` | `core` 只注册 MIFA-73 之前的那批工具 |
| `JMS_MCP_WRITE` | `limited` | 不注册写工具 | `internal/mcpserver` | `limited` 注册写工具 |
| `JMS_MCP_WRITE_DOMAINS` | 逗号分隔的 `node`、`account` | 全部已注册域 | `internal/mcpserver` | 收窄 `JMS_MCP_WRITE=limited` |
| `JMS_MCP_DEBUG` | `1`、`true`、`yes`、`on` | 关闭 | `internal/mcpserver` | 启用仅调试用的 `jms_pool_status` 工具 |
| `JMS_MCP_BACKEND` | `ssh`、`ws`、`auto` | 不覆盖 | `internal/mcpserver` | 强制本进程使用某个传输后端 |
| `JMS_MCP_ENDPOINT` | `internal`、`external` | 不覆盖 | `internal/mcpserver` | 强制本进程使用某个地址类型 |
| `JMS_MCP_EXEC_TIMEOUT` | Go duration（`90s`、`10m`）或正整数秒 | `5m` | `internal/mcpserver` | `jms_exec` 未指定超时时的上限 |
| `JMS_WS_DUMP` | 任意非空值 | 关闭 | `internal/transport` | 把每个 WebSocket 帧 dump 到 stderr。仅调试用，会打印负载内容 |

非 `JMS_` 前缀但同样相关的变量：

| 变量 | 作用 |
|---|---|
| `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY` | 一律不读 |
| `DISPLAY`、`WAYLAND_DISPLAY`、`XDG_SESSION_TYPE` | 仅 Linux。决定"凭据库被锁"消息的措辞 |
| `XDG_CONFIG_HOME`、`HOME`、`AppData` | 定位默认配置目录 |

## 安全边界

- **配置文件里永远没有密钥。** 登录密钥、TOTP 密钥、加密过的密钥都没有。schema 里根本没有这样的字段。
- **操作系统凭据库按别名和类型各存一条**，服务名是 `jms-client`。删掉别名会连带删掉这两条。
- **环境变量兜底是只读的。** 往它写会失败并报 `credential store is read-only`，所以 `jms config add` 会落到操作系统凭据库。
- **错误信息只给变量名，不给值。** 审计日志在写入时就把疑似凭据的查询参数掩码掉，而不是在读取时补救。
- **审计文件只有属主可读**（`0700` 目录里的 `0600` 文件），且每进程一个文件，被轮转或复制的日志不会变成全局可读。
- **`JMS_AUDIT=off` 是明确的退出开关。** 它停的是文件，不是事件总线：`jms attach` 依然能观察宿主。
- **`JMS_WS_DUMP` 会把协议负载打到 stderr。** 它只是调试开关。

## 已知限制

- **没有 v1 到 v2 的迁移。** 加载器接受任意 `version` 值，也不会转换旧 schema。唯一被拒绝的格式是旧版 Python `jms-cli` 的 `config.yaml`。
- **`--config` 不会搬走 `state.json` 或 `audit/`。** 只有 `JMS_CONFIG` 会搬走整个目录。
- **`--config` 传不到 MCP 服务端。** `jms mcp` 从 `JMS_CONFIG` 或平台默认路径加载配置。
- **凭据库的锁只能被检测，不能被解开。** 客户端会快速失败并指出该设哪个环境变量，它自己解不开锁。
- **`JMS_AUDIT_FULL` 救不回 MCP 的输出。** MCP 层在事件进入总线之前就截断了预览。
- **`endpoint.select` 永远不会出现。** 这个类型在枚举里存在，但没有任何代码路径发布它。
- **一个审计目录只有一套保留窗口**，不管记录是哪个别名产生的。
- **审计日志只在本机。** 不会转发到集中式采集器。
