# 排障

每条错误信息代表什么、该怎么处理。
[README](../README.zh-CN.md) 里有简版。


**`the credential store is locked`**
操作系统凭据库存在，但无法无人值守地读取。无桌面会话的机器上这通常是 GNOME Keyring 的常态：Secret Service 对解锁请求返回一个提示框，而没有人去输入解锁密码。

命令会在毫秒内失败，并指出该设哪个环境变量。要么在桌面会话里解锁凭据库，要么直接绕开它：

```sh
export JMS_PASSWORD_BASTION='…'
```

想知道卡在哪一步，加 `--timing` —— 凭据阶段在失败路径上也会计时。

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
