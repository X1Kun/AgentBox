# 使用指南

状态：agentboxd 已决定退役；当前提供普通 Job、gVisor、Calico 和上游冷会话、固定预热池/Go SDK 验收，没有新会话/批任务产品 API。上游验收命令见[冷会话入口](operations.md#cold-sessions)；该 Go 客户端在集群内逐次调用执行/文件接口，不接入大模型、不要求一次提交整套代码，也不保存 Python 变量。下面先定义拟议契约，再保留仍在源码中的旧 HTTP、MCP 与 DeepSeek 用法。阶段见[路线图](roadmap.md)，旧服务收尾见[运维指南](operations.md#retirement)。

<a id="target"></a>
## 目标接口契约（计划）

Go 产品层适配上游 Agent Sandbox 与 Kubernetes Job，不调用 agentboxd。P2 实现会话工具，P3 增加批任务工具；具体端点、工具名和 schema 在实现时版本化公布，目前以下是能力约定，不是可调用 API。

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

## 旧接口迁移约定

- 旧 POST /v1/execute、execute_python/get_status 与 Daemon 容量字段进入退役范围；不静默赋予跨调用状态，也不暗改为后台 Job。
- 新会话/批任务工具与旧工具版本明确区分，取消、断线和结果语义独立定义，不承诺旧时限或 Warm 延迟等价。
- 旧在途请求先排空或明确取消，未知结果不得自动迁入新会话/Job；切换后旧调用明确停用，新平台失败不自动回落 Daemon。
- 通用模型客户端、输入/输出预算和错误处理可按职责复用；旧 cleanup_complete 及 timings 不直接作为新平台字段解释。

执行 Pod 重建可能保留 Claim/Service 名称，但 Pod UID 和 emptyDir 工作目录已经变化；旧句柄不能据此继续代表原环境。当前 fault 验收器会显式识别这种变化并结束原会话，产品接口中的逐请求身份保护仍属于 P2。

新契约依据见[会话设计](../DESIGN.md#session)、[批任务设计](../DESIGN.md#batch)，切换门槛见[路线图](roadmap.md#retirement)。以下所有 HTTP/MCP 示例仅适用于旧链路。

<a id="http"></a>
## 旧 HTTP 接口

配对模式使用普通用户 Gateway 的 loopback 地址与外部 Bearer token；daemon 的内部 token 不向调用方提供。独立 daemon 也支持 loopback HTTP。参数结构以 [protocol.go](../internal/api/protocol.go) 为准。

| 接口 | 作用 | 鉴权 |
| --- | --- | --- |
| GET /healthz | 当前服务存活 | 无 |
| GET /readyz | 执行器就绪；Gateway 会探测 daemon | 无 |
| GET /v1/status | 容量、库存、观测与 QoS 状态 | Bearer |
| POST /v1/execute | 同步执行 Python，等待清理并返回结果 | Bearer |

| 请求字段 | 限制 |
| --- | --- |
| runtime | 必须为 python |
| code | 非空，最多 256KiB |
| stdin | 最多 256KiB |
| timeout_ms | 省略或 0 使用 5000；有效范围 1～30000，仅限执行阶段 |
| request_id | 最多 128 字节，只关联、不去重 |
| env | 最多 16 个 AGENT_ 变量；键名和内容受服务端校验，单值最多 4096 字节且无 NUL |

请求使用 application/json，整体最多 2MiB，仅允许已知字段；不能指定镜像、宿主路径、挂载、cgroup 或资源配额。

响应包含 execution_id、status、exit_code（可用时）、stdout/stderr、output_truncated、timings、usage、cleanup_phases 和 cleanup_complete。stdout/stderr 合计最多 1MiB；JSON 转义后的响应可能更大。状态有 succeeded、failed、timeout、oom、cancelled、output_limit、infrastructure_error。Usage 是从实例创建到清理前采样的 cgroup 累计量，不是纯 Python 用量或完整生命周期成本。

HTTP 200 不等于代码成功，需要检查 status 与 cleanup_complete。参数/格式错误为 400，鉴权失败为 401，请求体超限 413、类型错误 415、慢 body 可返回 408；执行器容量或健康拒绝为 503，基础设施错误为 500。排队前或排队期间取消可能没有 execution_id，此时没有已领取实例需要清理。

timings 的单位为毫秒：

| 字段 | 统计范围 |
| --- | --- |
| queue_ms | 等待资源预留的时间，不含成功预留后的创建 |
| setup_ms | 按需创建及任务准备；READY 命中时为健康检查和输入准备 |
| execution_ms | 任务启动与等待，受 timeout_ms 约束 |
| cleanup_ms | 实例销毁、最终输出收集与记录删除 |
| total_ms | 本次 Engine 调用的排队、准备、执行和清理；不含提前预热、空闲期、HTTP 解码和传输 |

### Gateway 转发边界

- 就绪不代表当前有执行容量或 QoS 门控开放。路径和方法固定，不转发任意 URL、查询参数、调用方 Host、转发头、幂等头或内部期限头。
- Gateway 与 daemon 复用上述校验，转发原始字节以避免二次编码改变大小预算。
- 默认最多 8 个执行请求在 Gateway 中读取/转发/返回，范围 1～64。满载立即返回 `503 reason=gateway_capacity`，不新增网关等待队列；状态/就绪请求另有 4 个名额与最多 2 秒预算，执行满载不会耗尽其名额。
- daemon 的执行结果、500 基础设施结果和 503 容量/QoS 拒绝保留原始 JSON 与 Retry-After。返回体最多 8MiB，覆盖现有 1MiB 输出的 JSON 转义开销；响应先完整读取再返回，避免把破损结果当成完整执行结果。
- daemon 不可连接、响应中断、内部凭据错误或非法响应映射为 502；Gateway 请求预算耗尽映射为 504。执行转发失败带 `outcome_unknown=true`，不能据此自动重放。同一 request_id 仍不提供去重保证。
- Gateway 不跟随后端重定向。执行转发只调用一次，Body 无 GetBody 且不转发幂等头，禁止传输层重放执行请求。

<a id="deadline"></a>
## 期限、取消与不确定结果

`timeout_ms` 仍只限制任务执行阶段。Gateway 默认另有 50 秒端到端请求预算，客户端更早的取消/期限优先；慢请求体受该预算及 5 秒读取上限约束。读完整请求体后清除读取期限，防止它影响长任务的连接断开监听。

Gateway 覆盖调用方的 `X-AgentBox-Deadline`，将实际截止时间传给 Unix daemon。daemon 只在 Unix 模式启用该内部约束，并保持 50 秒预算上限；Gateway 配置更大的超时不会放宽 daemon 上限。取消通过 Unix HTTP 请求上下文/断连传播，没有新增独立 Cancel/GetExecution RPC。

取消后，已经开始的 create/start 仍在独立预算内完成当前事务，再清理。Gateway 返回超时或连接错误不表示沙箱已经销毁；只有完整执行结果的 cleanup_complete 能表达已确认清理。停止 Gateway 会取消在途转发，daemon 仍负责后续回收。

<a id="mcp"></a>
## 旧 MCP 与模型调用

MCP 使用固定版本官方 Go SDK 的 stdio 传输。模型生成 Tool Calls，由 agentbox-agent 转为 MCP 调用，MCP 再转发到 Gateway；模型 API key 仅由 Agent 持有。当前不提供外网 MCP HTTP 服务。

### 工具与结果

| 工具 | 输入 | 输出 |
| --- | --- | --- |
| `execute_python` | 必填 code；可选 stdin、timeout_ms、env | 原始 HTTP 状态及完整 Gateway JSON |
| `get_status` | 空对象 | daemon 就绪、容量、库存和 QoS 状态 |

工具参数沿用上述 HTTP 输入限制，runtime 固定为 Python，不选择宿主机或资源配置。每次执行生成新的 `mcp-...` request_id，仅用于关联，不提供去重保证。

MCP 结果同时包含 `structuredContent` 和对应的文本 JSON：

```json
{
  "http_status": 200,
  "response": {
    "execution_id": "ab-...",
    "request_id": "mcp-...",
    "status": "succeeded",
    "stdout": "42\n",
    "stderr": "",
    "cleanup_complete": true
  }
}
```

实际 response 保留 Gateway 返回的完整字段，包括 timings、usage、清理阶段、拒绝原因和不确定结果。代码失败、超时、输出超限，以及 HTTP 错误会设置 MCP `isError=true`；它们作为工具结果返回，便于 Agent 读取和修正。未知工具等协议错误由 SDK 处理。

MCP 每个进程最多同时转发 8 个执行调用、4 个状态调用，额满直接返回 `mcp_capacity`，不额外排队。HTTP 客户端限制请求/响应大小、只连接显式 loopback 地址、禁止重定向及执行重放。stdio 是同一用户信任范围内的进程接口；参数的大小校验发生在 SDK 解码后，不应将 stdin 直接接到不可信公共网络。

### 运行示例

仅在已有旧服务或独立历史复现环境中使用；配对方式见[旧 Gateway 部署参考](operations.md#gateway)。构建：

```bash
make build
```

在运行 Agent 的普通用户终端设置凭据，交互输入避免将实际值写进示例或命令参数：

```bash
read -rsp 'DeepSeek API key: ' DEEPSEEK_API_KEY
echo
export DEEPSEEK_API_KEY
read -rsp 'Gateway external token: ' AGENTBOX_GATEWAY_TOKEN
echo
export AGENTBOX_GATEWAY_TOKEN

./bin/agentbox-agent \
  -prompt '请调用 Python 计算 sum(i*i for i in range(1, 101))，依据实际执行结果回答。' \
  -json
```

Agent 默认启动同目录下的 `agentbox-mcp`，通过 tools/list 获取工具定义并转换为 DeepSeek 的 function schema，再用 tools/call 执行。每个返回值通过原始 tool_call_id 与 `role=tool` 消息关联，直到模型给出最终答案或预算耗尽。

可选的自修正演示提示：

```bash
./bin/agentbox-agent \
  -prompt '演示工具错误修正：先用 Python 执行 print(1/0)，读取错误后再执行 print(6*7)，最后根据工具结果回答。'
```

模型调用行为并非确定性的，演示是否遵循提示以真实输出为准。自动回归使用模拟 DeepSeek API，确定性覆盖失败后修正路径，不把模型随机性或付费 API 当作回归依赖。

### 客户端配置与会话预算

| 配置 | 默认值/含义 |
| --- | --- |
| `DEEPSEEK_API_KEY` | 必填，仅由 Agent 持有 |
| `AGENTBOX_GATEWAY_TOKEN` | 必填，Gateway 的外部 token |
| `DEEPSEEK_MODEL` / `-model` | `deepseek-flash`，可按账户可用型号覆盖 |
| `DEEPSEEK_BASE_URL` / `-deepseek-base-url` | `https://api.deepseek.com`；远程 API 要求 HTTPS |
| `-gateway-url` | `http://127.0.0.1:8080`；仅显式 loopback IP/非零端口 |
| `-mcp-command` | 与 Agent 同目录的 `agentbox-mcp`；由操作人员配置 |
| `-max-turns` | 6 次模型请求，允许 1～20 |
| `-max-tool-calls` | 8 次工具调用，允许 1～32；单个返回批次最多 4 个，顺序执行 |
| `-timeout` | 整轮会话 3 分钟，最大 10 分钟 |

Agent 的 MCP 子进程只获得固定 PATH、LANG 和外部 Gateway token，不继承模型 API key、内部 daemon token 或用户完整环境。任务环境仍由 daemon 显式构造。DeepSeek 返回的错误正文不直接打印，API 请求失败不自动重试。

Agent 对 prompt、会话历史、模型响应和发送给模型的工具结果设置独立预算。MCP 返回给普通客户端的完整执行结果保留；发送给模型时 stdout/stderr 各最多 2KiB，并标记 `model_output_truncated`，必要时省略非关键细节。它与 daemon 的 `output_truncated` 含义不同：前者控制模型上下文，后者表示执行阶段输出超限。status、清理确认和不确定结果判断使用裁剪前字段。

代码失败且清理已确认时，模型可以发起修正后的新执行。结果不确定、执行的清理未确认、基础设施/容量错误或鉴权失败时，Agent 停止，包含同批次尚未执行的后续调用。轮次或调用次数不足时也在执行前停止。这里只提供一个任务的一次有限对话，不提供持久会话、跨重启去重或独立执行结果查询。

### MCP 取消与断线

MCP 调用取消通过 SDK 通知传递到 Gateway HTTP 请求，再沿 Unix HTTP 到 daemon。SIGINT/SIGTERM 会取消 Agent 的会话；stdio 关闭时 SDK 负责回收 MCP 子进程。daemon 保持独立清理职责，客户端无法收到结果时仍按不确定结果处理。

MCP 进程的生命周期取消也显式关联到每个在途 HTTP 请求，确保停止信号能让转发及时退出。SDK 的 ClientSession.Close 是优雅关闭，会等待已有调用；需要取消时应先取消调用上下文。断线验收直接关闭底层 stdio 传输，区分实际断线与优雅等待。

取消不等于同步确认清理。任务已经进入 create/start 时沿用 daemon 的独立事务预算；只有完整结果中的 cleanup_complete，或独立验收对资源的检查，才是清理确认。

<a id="remote"></a>
## SSH 隧道

当前客户端支持通过 SSH 隧道调用远端 Gateway，不将 daemon Unix socket 改成外网入口。若后续有宿主机 B，在 A 建立：

```bash
ssh -NT -o ExitOnForwardFailure=yes \
  -L 127.0.0.1:18080:127.0.0.1:8080 user@worker-b
```

另一个终端以 B 的外部 Gateway token 运行：

```bash
./bin/agentbox-agent -gateway-url http://127.0.0.1:18080 \
  -prompt '请调用 Python 计算 6*7 并返回实际结果。'
```

当前仍固定一个 Gateway；上面是 SSH 隧道用法，尚无跨宿主机验收，不表示已提供多 Worker 路由。

旧验证入口与范围见[验证指南](validation.md#mcp)。新平台上线后本页旧示例仅作为版本参考，不要求继续运行 Daemon 来兼容旧工具。
