# 使用指南

当前产品链路为 Agent → stdio MCP v2 → Go Session API → 上游 sandboxd/gVisor。生命周期、执行、文件和管理员记录清理已实现；批任务接口仍为计划。旧 Gateway、Daemon 及旧 MCP 工具已移出活动代码，历史复现见[归档](archive/legacy-runtime.md)。

<a id="session-api"></a>
## Go 会话 API v2

当前入口为 `cmd/agentbox-api`，提供创建、查询、关闭，以及显式开启后的执行/文件接口。实例固定一个管理员维护的 namespace/Pool，调用方只传创建标识和硬期限，不能提交镜像或 PodSpec。

| 请求 | 行为 |
| --- | --- |
| `POST /v2/sessions` | 异步创建；首次返回 202，相同有效请求重放返回 200 和同一对象 |
| `GET /v2/sessions/{session_id}` | 查询上游状态及已持久化的身份绑定 |
| `DELETE /v2/sessions/{session_id}` | 提交关闭；确认回收返回 200，否则返回 202/closing |
| `POST /v2/sessions/{session_id}/execute` | 在原会话中运行一次 Python，返回 stdout/stderr/exit_code |
| `POST /v2/sessions/{session_id}/files/write`、`files/read` | 工作目录内有界二进制文件写入/读取 |
| `GET /healthz`、`GET /readyz` | 进程存活、配置 Pool 的 API 可达性；不承诺可领取容量 |

请求使用配置文件中的 Bearer token。token 对应服务端配置的 principal；相同 principal 可配置多个 token 用于轮换，归属不绑定 token 字节。所有错误返回稳定的 `error` 代码，不返回集群地址、凭据或原生错误正文。

```http
POST /v2/sessions
Authorization: Bearer <caller-token>
Content-Type: application/json

{"request_id":"analysis-001","ttl_seconds":600}
```

创建 body 最多 4KiB，拒绝未知/重复字段和尾随 JSON。request_id 为 1～128 个受限 ASCII 字符；TTL 默认 600 秒，默认可选 60～3600 秒，由管理员配置上限。服务端将创建标识、归属和实例关联到同一命名 Claim，并绑定有效 TTL/Pool 摘要。参数冲突为 409/request_conflict；重试不延长期限。创建响应丢失时可用相同参数重放创建请求，服务仅查询/创建同名对象，每次调用只发起一次同名创建动作，不换新标识补建；底层 client-go 的传输重试仍使用同一资源名称。

返回字段为 `session_id`、`state`、`reason`、`expires_at`、`identity_revision`、`cleanup_complete`、`operation_active`，以及首次确认 API 对象清理后出现的 `closed_at`。session_id 是不应解析的标识，包含 Claim 实例身份；旧标识不能操作管理员删除后重建的同名 Claim。首次 Ready 时绑定 Sandbox/Pod/Service UID 并存入 Claim 注解，服务重启不丢失此绑定。Pod UID 改变或原资源消失后标记 invalidated，绑定不覆盖；新环境应使用新的 request_id 申请。

状态包括 creating、ready、unavailable、invalidated、closing、closed、failed。ready 仅反映查询时的控制面状态，不保证未来执行一定成功。跨 principal 的读取/关闭统一返回 404；身份、绑定或参数冲突为 409；配额/权限/策略明确拒绝创建为 503/admission_rejected；传输结果不确定且查不到同一对象为 503/create_outcome_unknown。HTTP 请求超时不代表上游资源已停止。

关闭通过原生 `ShutdownPolicy=Retain` 和终止时间驱动。Claim 留作幂等及终止记录；只有上游确认已观察本代到期，关联 Sandbox/Pod/Service 均消失后才返回 closed/cleanup_complete。仅查无对象、仅到达时钟期限或仅收到关闭请求均不算回收完成。产品字段证明 Kubernetes 对象回收；真实运行时/进程的回收由平台验收独立核对，API 本身无 CRI 权限。

停止 Go API 进程不自动删除会话，上游硬期限仍生效。首次确认 closed/cleanup_complete 时，服务以 UID/resourceVersion CAS 将观察时间写入 Claim 的 `agentbox.dev/api-closed-at`，后续查询/重启不延长该时间；这不是实际进程退出时间，也不从创建时间或 TTL 倒推。旧记录首次被新服务或管理员工具确认关闭后，才开始完整保留窗口。

独立管理员工具 `agentbox-cleanup` 默认只预览，保留期默认 24h，按 namespace/Pool/instance/principal 限定范围。未有 closed_at 的旧记录在 `--apply` 时仅补充关闭确认，本次不删除；已确认关闭且保留期届满的记录才可删除，操作与权限见[记录清理](operations.md#claim-retention)。部署方应发布固定保留期，缩短保留期需要明确管理变更。超过保留期不自动删除，未删除前仍保持创建幂等；实际删除后同一个 request_id 可产生新 Claim UID，旧 session_id 不能操作新对象，因此调用方应持续使用唯一创建标识。记录计入 namespace 配额；清理记录不等于回收运行资源。

`/healthz` 表示进程存活；`/readyz` 读取后台检查快照，不随探针请求增加 Kubernetes 查询。每实例一个检查循环，每 2 秒刷新，单次预算不超过 2 秒；启动尚未完成检查、最近检查失败、快照达到 5 秒或服务停止检查时返回 503。readiness 不表示会话容量或现有命令健康。

### 执行与文件调用

先创建并查询至 ready，再携带该会话的 `identity_revision` 调用；每次只传本次代码，执行结束后可等待模型下一次 tool call。文件跨调用保留，每次启动新的 `python3 -I -B -u -c` 进程，Python 变量不保留。业务方通过文件导出产物后再关闭会话。

```json
{"identity_revision":"<ready response revision>","code":"import csv\nprint(list(csv.DictReader(open('input.csv'))))","timeout_seconds":10}
```

代码最多 64KiB，命令期限默认 20 秒，可选 1～60 秒；从连接运行时开始计入期限。正常返回 HTTP 200 和 `stdout`、`stderr`、`exit_code`，非零退出码是程序结果，仍可继续使用会话。stdout/stderr 合计最多 64KiB；超限为 413/execution_output_limit，并请求关闭整个会话，不返回伪造的完整结果。

文件写入示例：`{"identity_revision":"<revision>","path":"input.csv","content_base64":"dmFsdWUKMgo="}`，其中 content_base64 须替换为实际文件的标准 Base64。读取只传 revision/path，返回 `content_base64` 和 `size_bytes`。单文件最多 256KiB，允许嵌套相对路径；绝对路径、`..`、非规范路径拒绝，符号链接的工作目录边界由上游文件协议再次检查。文件请求预算 10 秒；不存在为 404/file_not_found，越界链接为 403/file_access_denied，下载超限为 413/file_too_large。

执行、文件读取和写入共用 Claim 上的 CAS 操作标记，同一会话最多一个在途数据操作，冲突立即返回 409/session_busy，无等待队列；查询和关闭使用独立 HTTP 名额。此标记跨 API 副本和进程重启保留，不自动租约到期释放。API 在操作中崩溃时，调用方应关闭会话并新建；不能借重启重放命令或绕过仍可能运行的进程。

每次连接使用绑定 Pod 的 IP，TCP 建连前后复核 UID/IP、会话状态和操作标记，随后固定该连接，禁止 Service 重解析、自动重连、HTTP 重定向和命令重试；结果返回前再验证原身份。各校验点分别读取新的完整资源快照，复用当轮已验证 Pod 的 UID/IP；快照不跨校验点或请求缓存，观察关联资源后仍重新读取 Claim 检查关闭、失效和操作标记。首次 Ready 绑定仅在 UID/resourceVersion CAS 成功后发布该轮状态，不重复扫描同一批资源。它依赖可信 Kubernetes/CNI 的 Pod 网络身份，不提供运行时远端证明，也不承诺无竞态的跨系统事务。身份变化拒绝继续，不将调用迁到替换 Pod。

执行超时为 504/execution_timeout；执行或写入结果不确定、客户端断线等返回 503/operation_outcome_unknown（断线时可能收不到响应），保留操作标记并在独立 5 秒预算内请求上游关闭。释放操作标记的补丁响应丢失时，只读回原 Claim UID 确认标记已清除；无法确认则返回 unknown 并尝试关闭。关闭请求失败时仍不得重放或继续，调用方必须重试关闭/查询，硬 TTL 是最终兜底。错误本身不等于清理完成，仍需 GET 确认 closed/cleanup_complete。成功执行响应丢失也没有命令结果查询或去重账本，调用方不能自动重试代码；创建幂等不代表执行 exactly-once。

<a id="mcp"></a>
## stdio MCP v2

`agentbox-mcp` 只访问配置的 Session API，不读取 Kubernetes 凭据或连接宿主运行时。配置 `AGENTBOX_API_TOKEN`，通过 `--api-url` 指定 API；可选 `--api-ca-file` 信任专用 CA。远端必须 HTTPS，HTTP 仅允许显式 loopback IP。客户端不使用环境代理，不跟随重定向；数据操作没有重放请求体或自动重试。stdout 只输出 MCP 协议，日志写 stderr。

| 工具 | 必填输入 | 含义 |
| --- | --- | --- |
| `session_create_v2` | request_id | 异步创建；可选 ttl_seconds，恢复仅重用同标识/同参数 |
| `session_get_v2` | session_id | 查询状态、身份修订与回收确认 |
| `session_execute_v2` | session_id、identity_revision、code | 单条 Python 命令；可选 timeout_seconds，默认 20s，最大 60s |
| `session_write_file_v2` | session_id、identity_revision、path、content_base64 | 写入不超过 256KiB 的文件，支持空文件 |
| `session_read_file_v2` | session_id、identity_revision、path | 下载文件字节，不超过 256KiB |
| `session_close_v2` | session_id | 请求关闭；随后查询确认 cleanup_complete |

工具结果使用 `http_status` 与原 API JSON `response` 信封。程序非零退出会设置 MCP isError，但仍是可解释的 exit_code/stdout/stderr 结果；会话可继续用于修正后的命令。执行/写入超时、输出超限、响应丢失或结果未知时停止继续调用并关闭会话，不能把重启 MCP 当作重放许可。输入拒绝重复、未知、null 字段和越界路径；工具不提供 stdin/env、镜像、资源策略或宿主选择。

旧 `execute_python`/`get_status` 不再注册，旧 `--gateway-url`/`AGENTBOX_GATEWAY_TOKEN` 不再作为入口。新调用必须采用 v2 工具及会话身份，不提供请求级旧后端回落。

<a id="agent"></a>
## Agent 会话调用

Agent 自动创建一个会话并等待 Ready，将 session_id/identity_revision 提供给模型。模型只看到查询、执行、文件读写四个工具；创建/关闭由 Runner 管理。每批工具调用在任何数据操作发出前完整验证，包括绑定身份、重复调用 ID 和预算。执行顺序串行，工作目录跨调用保留，Python 内存变量不保留。模型失败、预算耗尽或会话取消后，也在独立预算内尝试关闭并查询原会话。

```bash
make build
# 在当前 shell 配置 Session API 与模型提供方凭据；不要写进命令参数或代码。
bin/agentbox-agent --api-url https://api.example.test \
  --api-ca-file /path/to/api-ca.pem \
  --prompt 'Create a small CSV, calculate its total, and download a JSON report.' --json
```

`AGENTBOX_API_TOKEN` 用于 API；`DEEPSEEK_API_KEY` 仅用于模型提供方。可通过 `--deepseek-base-url`/`--model` 或相应 DEEPSEEK 环境配置提供方。普通回归使用确定性替身，运行上述真实模型命令可能调用计费服务，需由操作方显式选择。子 MCP 进程仅获得固定 PATH/LANG 与 API token，不继承模型 key、KUBECONFIG、用户 shell 环境或旧 Daemon 凭据。MCP 可执行文件默认为 Agent 同目录下的 agentbox-mcp，也可显式传 `--mcp-command` 指定可信程序。

| Agent 参数 | 默认 | 上限/范围 |
| --- | --- | --- |
| timeout | 3m | 10m；会话创建、就绪与模型/数据调用的总预算 |
| ready-timeout | 1m | 3m；也受总预算限制 |
| cleanup-timeout | 20s | 2m；使用不继承对话取消的独立预算 |
| session-ttl-seconds | 600 | 60～3600；上游硬期限 |
| max-turns | 6 | 1～20 次模型请求 |
| max-tool-calls | 8 | 1～32 次模型工具调用；生命周期另行有界 |

输出 JSON 包含 answer、turns、tool_calls、创建 request_id/ttl_seconds、session_id/identity_revision、outcome_unknown、cleanup_complete 和 cleanup_error。显式读取的文件保留在 artifacts 中，总下载预算 1MiB；模型视图中的 stdout/stderr/base64 各最多 2048 字节，整体最多 32KiB，截断会标记 model_output_truncated。完整 artifact 不会由模型视图的截断内容替代。对话历史上限 512KiB。

创建响应丢失时，Runner 在同一 15s 预算内最多尝试两次相同 request_id 与 TTL 的创建恢复；不会生成新标识补建。若仍无法取得 session_id，输出创建标识/TTL 和未知结果供管理员恢复，上游硬 TTL 最终兜底。已有 session_id 时，退出前只针对该原 ID 请求关闭并等待确认；关闭/查询失败会保留 cleanup_error，不能宣称已回收。成功 API 对象回收不等于独立 CRI 进程证明，真实验收由宿主另行核对。

<a id="deadline"></a>
## 取消与未知结果

MCP 请求取消向 HTTP API 传播，API 对可能仍运行的操作保留互斥标记并请求全会话关闭。连接断开不等于执行停止；结果未知时 Agent 终止当前批次，不再调用后续工具或模型。关闭是幂等控制操作，可在独立预算内重试并查询；执行/写入不能因此重放。硬 TTL、命令期限、对话等待和清理预算分别解释。

<a id="target"></a>
## 会话与后续批任务边界

Go 产品层适配上游 Agent Sandbox 与 Kubernetes Job，不调用 agentboxd。会话 API/MCP 已实现，P3 再增加批任务工具；以下批任务一列仍为计划。

| 能力 | 会话 Session | 批任务 BatchTask |
| --- | --- | --- |
| 生命周期 | 创建/领取、查询、执行、文件访问、关闭/到期 | 提交、查询、取消、读取结果/产物 |
| 执行单元 | 上游 Sandbox/Claim 对应的会话 Pod，可执行多次命令 | 有限输入或分片对应 Job，计算实际运行在其 Pod 内 |
| 状态保留 | 首版保留工作目录；新 Python 进程不继承上次变量，不承诺内存恢复 | 输入 manifest 与按 Job/Pod UID、attempt 隔离的输出 |
| 并发与期限 | 每会话最多一个命令，冲突拒绝；命令超时、会话期限与清理预算分开 | 有限执行/重试预算，队列等待和运行时间分别记录 |
| 断线与未知结果 | 断线不代表终止；按稳定身份查询，不能确认时返回 unknown 并关闭会话收敛，不重放命令 | 提交成功后断线继续；查询同一个任务，不补建另一份 Job |
| 清理 | 关闭后拒绝命令；使用过的环境不归还干净池，确认 Pod 与临时目录回收 | 显式取消等待 Pod 收敛；先保存结果摘要再 TTL 删除，产物有独立保留期 |

模板由管理员控制；调用方不能任意设置镜像、宿主挂载、资源或 PodSpec。鉴权同时检查会话/任务归属，文件访问限制在工作目录；模型 key 和集群权限不进入执行环境。池耗尽必须有有限等待或明确失败，API readiness 不表示一定能立即领取。

创建/提交标识绑定模板和输入摘要；同标识不同内容拒绝。产品标识关联上游 UID，重启后查询同对象；查询能力不足的命令结果明确 unknown，不声称 exactly-once。超时取消无法确认执行终止时关闭整个会话，确认收敛前不继续命令。客户端等待预算不代替服务端执行预算。

会话中产生的输入或产物可以经可信产品层关联到允许的批模板；首版不把任意生成脚本自动转换成可信批程序。Job Complete、程序退出码、产物验证与资源清理分别表达，不能用一个成功字段代替。


旧运行路径的源码、部署方式和实验仅在[历史版本](archive/legacy-runtime.md)中复现。
