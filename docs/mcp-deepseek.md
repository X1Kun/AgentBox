# MCP 与 DeepSeek Agent

`agentbox-mcp` 把现有 Gateway 协议适配成两个 MCP 工具；`agentbox-agent` 是使用 DeepSeek Tool Calls 的有限轮次示例。协议由固定版本的[官方 Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk)实现，首版采用 stdio，不新增对外 MCP HTTP 服务。

```text
DeepSeek API ←→ agentbox-agent
                    ↓ MCP / stdio
                agentbox-mcp（普通用户）
                    ↓ loopback HTTP
                  Gateway
                    ↓ Unix HTTP
              root daemon → runsc → 清理
```

模型只返回工具名和参数；实际执行、结果回传和调用预算由 Agent 程序管理。DeepSeek API 使用普通非流式、非思考模式，显式发送 `thinking.type=disabled`。模型名和 API 地址可配置，默认值依据[当前官方工具调用示例](https://api-docs.deepseek.com/guides/tool_calls/)。没有启用 Beta strict 模式；MCP 和 Gateway 的服务端校验仍然执行。

## MCP 工具

| 工具 | 输入 | 输出 |
| --- | --- | --- |
| `execute_python` | 必填 code；可选 stdin、timeout_ms、env | 原始 HTTP 状态及完整 Gateway JSON |
| `get_status` | 空对象 | daemon 就绪、容量、库存和 QoS 状态 |

Python 的 runtime 固定，工具参数不能选择宿主机、镜像、rootfs、挂载、cgroup 或资源配额。代码/stdin 各最多 256KiB，env 沿用最多 16 个 AGENT_ 变量等约束；timeout_ms 为 0 或省略时使用 5000ms，最大 30000ms。每次执行生成新的 `mcp-...` request_id，仅用于关联，不提供去重保证。

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

## 运行示例

先按 [Gateway 部署说明](gateway.md)准备可用入口，再构建：

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

## 配置与会话边界

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

## 取消与断线

MCP 调用取消通过 SDK 通知传递到 Gateway HTTP 请求，再沿 Unix HTTP 到 daemon。SIGINT/SIGTERM 会取消 Agent 的会话；stdio 关闭时 SDK 负责回收 MCP 子进程。daemon 保持独立清理职责，客户端无法收到结果时仍按不确定结果处理。

MCP 进程的生命周期取消也显式关联到每个在途 HTTP 请求，确保停止信号能让转发及时退出。SDK 的 ClientSession.Close 是优雅关闭，会等待已有调用；需要取消时应先取消调用上下文。断线验收直接关闭底层 stdio 传输，区分实际断线与优雅等待。

取消不等于同步确认清理。任务已经进入 create/start 时沿用 daemon 的独立事务预算；只有完整结果中的 cleanup_complete，或独立验收对资源的检查，才是清理确认。

## 远端连接与 kind 后续阶段

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

本轮没有第二台宿主机，未进行跨宿主机验收。下一阶段可用独立 kind 多 Worker 集群模拟跨节点接入及节点故障，再逐步增加节点发现和路由；当前仍固定一个 Gateway，不声称已实现多 Worker 调度。kind 节点共享宿主 CPU/内存/PSI，其结果不能替代独立物理机资源隔离或性能证据。

## 验证入口

```bash
make check build
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py mcp --platform systrap
```

第一条覆盖协议映射、服务端校验、取消、禁止重放、模型错误修正、预算和凭据传递边界。第二条使用实际普通用户 MCP/Gateway 子进程与真实 runsc，DeepSeek API 仍由本地确定性模拟服务代替。真实 DeepSeek 联调需要操作人员配置自己的 API key 后运行上述示例，不能将模拟 API 结果当作云端模型验证。

### 已验证的行为

模拟 API 已覆盖成功、代码错误、超时、取消、断线和自修正，实际普通用户 MCP/Gateway 子进程调用真实 runsc，清理和容量恢复均确认。SDK 优雅 Close 与真实传输断开分别测试，MCP 进程生命周期显式取消转发 HTTP。

真实 deepseek-flash 联调以 3 次模型请求、2 次执行完成 ZeroDivisionError 后修正为 42；控制器独立核对任务状态、stdout 和 bundle/cgroup 删除，没有只依据模型最终回答判定。API key 未进入 MCP/任务环境，也未出现在日志与待提交文件中。

完整范围、环境与已知问题见 [统一验收](validation.md#mcp)，[脱敏结果](data/deepseek-live-validation.json)保留实际计数与日志哈希。不将单次流程耗时当作性能基准；SSH 跨机和多 Worker 尚未验收。

显式复现真实 API 联调（普通 make check 不调用）：

```bash
AGENTBOX_LIVE_DEEPSEEK=1 \
  AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py live --platform systrap
```

默认读取 `.local/deepseek.env`，或由 AGENTBOX_DEEPSEEK_CONFIG 指定私有文件，只解析允许的 KEY=VALUE，不执行 shell 内容。上限为 4 次模型请求、3 次工具调用、100 秒对话；只有该套件的控制容器使用 bridge 出站访问 API，runsc 任务仍禁网。普通容器验证均保持禁网。
