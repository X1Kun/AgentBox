# 本机 Gateway 与 Unix HTTP

`agentbox-gateway` 是普通用户入口，daemon 继续负责特权沙箱操作。首版固定连接一个本机 daemon，复用 Execute/Status HTTP 协议；默认只绑定 loopback，不包含多节点路由或 MCP。

```text
客户端 --Bearer 外部 token--> Gateway（普通用户）
              --Unix socket / Bearer 内部 token--> daemon（root）
              --> 一次性 runsc 执行 --> 清理 --> 返回结果
```

独立启动 daemon 时仍可使用原有 loopback TCP。配对部署将 daemon 切到 `unix:/run/agentbox/daemon.sock`，Gateway 占用原来的外部端口。

## 协议与入口限制

- `GET /healthz` 表示 Gateway 存活；`GET /readyz` 探测 daemon，就绪不代表当前有执行容量或 QoS 门控开放。
- `POST /v1/execute`、`GET /v1/status` 要求外部 Bearer token。请求路径和方法固定，不转发任意 URL、查询参数、调用方 Host、转发头、幂等头或内部期限头。
- Gateway 与 daemon 复用 JSON 校验：2MiB 请求体、字段允许列表、代码/输入/env 和 timeout_ms 的既有约束。校验后转发原始字节，避免二次 JSON 编码改变请求大小预算。
- 默认最多 8 个执行请求在 Gateway 中读取/转发/返回，范围 1～64。满载立即返回 `503 reason=gateway_capacity`，不新增网关等待队列；状态/就绪请求另有 4 个名额与最多 2 秒预算，执行满载不会耗尽其名额。
- daemon 的执行结果、500 基础设施结果和 503 容量/QoS 拒绝保留原始 JSON 与 Retry-After。返回体最多 8MiB，覆盖现有 1MiB 输出的 JSON 转义开销；响应先完整读取再返回，避免把破损结果当成完整执行结果。
- daemon 不可连接、响应中断、内部凭据错误或非法响应映射为 502；Gateway 请求预算耗尽映射为 504。执行转发失败带 `outcome_unknown=true`，不能据此自动重放。同一 request_id 仍不提供去重保证。
- Gateway 不跟随后端重定向。执行转发只调用一次，Body 无 GetBody 且不转发幂等头，禁止传输层重放执行请求。

## 期限和取消

`timeout_ms` 仍只限制任务执行阶段。Gateway 默认另有 50 秒端到端请求预算，客户端更早的取消/期限优先；慢请求体受该预算及 5 秒读取上限约束。读完整请求体后清除读取期限，防止它影响长任务的连接断开监听。

Gateway 覆盖调用方的 `X-AgentBox-Deadline`，将实际截止时间传给 Unix daemon。daemon 只在 Unix 模式启用该内部约束，并保持 50 秒预算上限；Gateway 配置更大的超时不会放宽 daemon 上限。取消通过 Unix HTTP 请求上下文/断连传播，没有新增独立 Cancel/GetExecution RPC。

取消后，已经开始的 create/start 仍在独立预算内完成当前事务，再清理。Gateway 返回超时或连接错误不表示沙箱已经销毁；只有完整执行结果的 cleanup_complete 能表达已确认清理。停止 Gateway 会取消在途转发，daemon 仍负责后续回收。

## Unix socket 权限与生命周期

daemon 的 `-listen` 接受显式 loopback IP，或 `unix:/绝对路径`。Unix 父目录须已存在、由 daemon 用户所有、权限为 0700 或 0750，且不能通过符号链接访问。socket 固定为 0660，组由 daemon 的有效主组决定。

监听器持有私有旁路锁，拒绝替换活跃 socket、普通文件、符号链接或其他用户的 socket。仅能确认连接被拒绝、归属正确且检查期间 inode 未变化的残留 socket 才回收。关闭时只删除自身 inode；私有锁文件可留在目录中，systemd RuntimeDirectory 会在服务停止后清理运行目录。

配对模式下：

- daemon 仍为 root，主组为 `agentbox-gateway`；运行目录为 root:agentbox-gateway、0750。
- Gateway 使用同名专用非 root 系统用户/组，通过组权限连接 socket，不能在运行目录中替换端点。
- 外部 token 与内部 token 不同。两份凭据文件均 root:root、0600，由 systemd 管理器读取后传入相应进程；Gateway 不需要直接读取 root 的配置文件。
- `/var/lib/agentbox` 保留 0700，Gateway 不访问 bundle、rootfs、cgroup 或 Docker socket。任务环境继续由 daemon 显式构造，不继承两个服务的 token。

## 配对安装

先完成普通构建，以及现有 daemon 的安装/更新。安装工具不编译、不下载依赖，也不自动启用或启动服务。已有部署的示例：

```bash
make build
sudo systemctl stop agentbox.service
sudo python3 scripts/deploy.py base --update
sudo python3 scripts/deploy.py gateway
sudo systemctl start agentbox-gateway.service
sudo python3 scripts/deploy.py smoke
```

首次安装 base daemon 使用 `python3 scripts/deploy.py base`，不传 `--update`。启动 Gateway 会通过 Requires/After 启动并等待 daemon。安装前两个服务必须停止、状态锁释放且无未回收任务记录；首次创建专用系统账号，不删除或修改其他账号。

附加安装文件：

| 路径 | 用途 |
| --- | --- |
| `/opt/agentbox/bin/agentbox-gateway` | 普通用户服务二进制 |
| `/opt/agentbox/GATEWAY_INSTALL.json` | 组件归属和二进制哈希，不含凭据 |
| `/etc/systemd/system/agentbox-gateway.service` | Gateway unit |
| `/etc/systemd/system/agentbox.service.d/gateway.conf` | daemon 组、RuntimeDirectory 和附加环境文件 |
| `/etc/agentbox-gateway.env` | 外部与内部 token、Gateway 参数，0600 |
| `/etc/agentbox-gateway-daemon.env` | daemon Unix 地址与内部 token，0600 |

base 的 `/etc/agentbox.env`、INSTALL.json、依赖和任务状态不变。首次安装默认沿用 base API token 作为外部 token，另生成内部 token，保留现有客户端认证方式。附加环境文件排在 base EnvironmentFile 后覆盖 daemon 地址和 token；不会把内部 token 传给外部客户端。

Gateway 环境配置采用无引号 KEY=VALUE：

| 配置 | 默认值/约束 |
| --- | --- |
| `AGENTBOX_GATEWAY_LISTEN` | base 的 loopback 地址，通常 127.0.0.1:8080；首次也可传 `--listen` |
| `AGENTBOX_GATEWAY_TOKEN` | 外部 token，32～4096 可见 ASCII 字符 |
| `AGENTBOX_DAEMON_TOKEN` | 内部 token，同上，且不能等于外部 token |
| `AGENTBOX_GATEWAY_MAX_INFLIGHT` | 8；1～64，增加时同步考虑 unit 的 MemoryMax |
| `AGENTBOX_GATEWAY_TIMEOUT` | 50s；整数 ms/s，1ms～60s |

unit 默认无 capabilities、NoNewPrivileges、只读系统路径、512MiB MemoryMax 和 64 TasksMax。它仍是本机单 worker 功能部署，不提供外网入口或生产高可用承诺。

## 更新、回退到独立模式及卸载

只更新 Gateway：

```bash
sudo systemctl stop agentbox-gateway.service agentbox.service
sudo python3 scripts/deploy.py gateway --update
sudo systemctl start agentbox-gateway.service
```

凭据保留，文件写入异常会恢复写入前的组件文件。首版不提供独立的 Gateway 二进制历史回退命令。

base 更新/回退/卸载会拒绝仍装有 Gateway 的部署，避免旧 daemon 或目录删除破坏配对关系。先移除组件，保留 Gateway 配置，再更新 base 并重新配对：

```bash
sudo systemctl stop agentbox-gateway.service agentbox.service
sudo python3 scripts/deploy.py gateway --uninstall
sudo python3 scripts/deploy.py base --update
sudo python3 scripts/deploy.py gateway
sudo systemctl start agentbox-gateway.service
```

仅移除组件时执行上面的 stop 和 `--uninstall`，然后 `systemctl start agentbox.service` 即恢复原 base 配置。`--uninstall --purge` 额外删除保留的 Gateway 配置；专用系统账号保留。不会删除 base token、任务状态、runsc 或 rootfs。

`--destdir` 支持在已有暂存 base 安装上生成组件文件，不创建主机账号或操作 systemd。root smoke 在配对模式读取附加凭据，经 Unix socket 直接检查 daemon，生命周期操作不依赖 Gateway 进程存活。

## 验证

`make check` 包含入口协议、鉴权、容量、结果映射、期限/取消、断线不重放、socket 归属与回收、配置及暂存安装/更新/卸载回归。短真实用例以 UID/GID 1000 启动实际 Gateway 二进制，再通过 Unix HTTP 调用 root Engine 和真实 runsc：

```bash
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py gateway --platform systrap
```

该用例使用 tmpfs 测试依赖，验证普通用户权限、未鉴权拒绝、Python 输出、token 隔离与任务清理；不验证宿主配对服务的完整 systemd 生命周期或正常磁盘路径性能。没有要求长时间实验或更新宿主服务。

### 本轮验证记录

2026-10-20：

- `make check build` 通过：全包 race 回归（含 Python 部署测试）、基准摘要回归、vet、格式/差异/脚本语法检查，以及三个二进制构建。
- 真实 TCP 慢请求体用例通过：40ms Gateway 预算下只发送部分 body，返回 408，未调用 daemon；执行超时、客户端取消和断线不重放回归通过。
- 一次真实 systrap 短流程通过：实际 Gateway 以 UID/GID 1000 运行，经 Unix HTTP 调用 root Engine 和 runsc；Python 返回 `gateway-ok`，任务环境不含服务 token，`cleanup_complete=true`，关闭后无任务 bundle/cgroup 残留。
- 完整配对生命周期已在隔离 systemd 252 / systrap 容器通过，覆盖权限、依赖启停/重启、更新、卸载恢复独立模式、重新配对与 purge；未执行宿主安装、账号创建或服务切换。

随后完成的真实取消/过载、大输出和 30 对延迟采样，以及解除 inotify 环境限制后的 systemd 配对验收与改进判断，见[有限验收记录](validation.md#gateway)。其中已验证行为与未验证项分别列出，原始样本可供复核。
