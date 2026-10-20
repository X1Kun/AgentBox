# Gateway 有限验收与开销记录

日期：2026-10-20。目标是验证真实取消、入口过载、完整输出和转发代价，给继续实现提供依据。完整配对 systemd 验收在解除 inotify 环境额度限制后通过。

本轮 `make check build` 已通过，涵盖全包 race、部署与报告汇总回归、vet、格式/脚本语法和构建；integration 标签下的新增用例也通过编译检查。真实容器用例的通过范围与环境问题处理记录如下。

原始延迟样本、摘要和故障记录保存在 [gateway-validation.json](data/gateway-validation.json)。本机完整日志保存在被 Git 忽略的 `artifacts/gateway-*.log`，JSON 中记录源日志 SHA-256。

## 已验证的行为

普通用户 Gateway 为实际编译的二进制，以 UID/GID 1000 运行；后端是 root Engine、真实 runsc 和 Unix HTTP。执行模式 warm，执行并发 1，库存目标 2，总实例上限 4。故障用例将 Gateway 在途上限设为 1，确定性地触发入口拒绝。

| 用例 | 本次观测 | 支持的结论 |
| --- | --- | --- |
| 执行任务时增加请求 | 0.764ms 返回 `503 gateway_capacity` | 入口达到配置容量后直接拒绝，没有新增等待任务 |
| 执行名额占满时探测 | readyz 0.461ms、status 2.227ms，均 HTTP 200 | 控制接口具有独立名额，本次未被执行请求占满 |
| 客户端取消长任务 | 50.194ms 内确认清理及库存恢复 | Unix HTTP 取消能到达真实运行时，任务 bundle/cgroup 删除，后续请求成功 |
| SIGTERM 停止 Gateway | 13.214ms 内确认清理及库存恢复 | Gateway 停止后 daemon 继续完成回收；重启 Gateway 后请求成功 |
| 1MiB NUL 输出 | JSON 响应 6,292,835 字节，完整恢复 1MiB stdout | 输出经过 JSON 最坏转义后仍在 8MiB 响应边界内，执行成功、未截断、清理完成 |

取消前通过 `task.pid` 确认 runsc 已创建任务，再保留 50ms 让长任务开始。取消后的结果由测试控制器在 daemon 执行返回时单独观察，不依赖已经断开的客户端。恢复耗时包含等待清理及补足库存，不是纯取消传播时延；每种故障只做一次确定性验收，不代表恢复延迟分位数或 SLO。最终关闭 Engine 后检查无任务 bundle/cgroup 残留。

大输出用例开始时使用单次写入，没有正确处理短写，实际产生的输出不足 1MiB。改为按 `os.write` 实际返回字节数循环写完后通过。该问题发生在测试数据生成，不能据此判定 Gateway 丢失输出；最终用例严格核对恢复后的 stdout 长度。

## 小样本开销对照

同一个 daemon，交替经过两条路径：

1. 测试客户端 → Unix HTTP daemon。
2. 测试客户端 → loopback HTTP Gateway → Unix HTTP daemon。

每路先预热 3 次，再采集 30 次；每对请求轮换先后顺序。任务均为 `print(1)`，每次发送前等待两个 READY 实例、无创建/销毁任务，减少库存供给对入口开销判断的干扰。两路各 30 次均成功且确认清理，没有从成功分位数中隐藏失败请求。

容器运行器以 `CGO_ENABLED=0` 编译测试入口和 Gateway；本轮数据对应这组构建设置。

| 路径 | 客户端耗时中位数 | 客户端耗时 P90 | Engine 之外耗时中位数 | Engine 之外耗时 P90 |
| --- | ---: | ---: | ---: | ---: |
| Unix 直连 | 170.859ms | 200.545ms | 0.230ms | 0.310ms |
| Gateway | 177.594ms | 195.549ms | 0.500ms | 0.595ms |

中位数采用中间两个样本的平均，P90 采用 nearest-rank。`Engine 之外耗时 = 客户端收到完整响应的耗时 − 该请求返回的 timings.total_ms`，包含客户端、HTTP 服务、编码/解码、转发和响应传输，不能当成纯 Gateway CPU 耗时。

逐对计算两路的 Engine 之外耗时差，中位数为 **0.257ms**。这是本轮轻载、小输出条件下增加入口路径的开销估计。端到端两路中位数相差约 6.736ms，但其内部执行时延本身波动较大，且 P90 的方向相反；不能把这个差值全部归因于 Gateway，也不能宣称 Gateway 提升了尾延迟。

本轮使用 systrap、6 CPU/5GiB 的禁网私有容器、tmpfs 依赖和固定 digest 的 Python 镜像 rootfs。结果不外推到 KVM、普通磁盘、并发吞吐、大请求或长期负载。每路 30 个样本不报告 P99，也未建立统计显著性或生产时延保证。延迟测试在首次组合运行中单独通过；同次运行的大输出数据生成失败已在后续独立故障验收中修正并通过，未因此重复采样或筛选延迟样本。

## 内存与配置判断

小输出对照结束时 Gateway 的 VmRSS/VmHWM 为 7,900KiB，约 7.71MiB。另一个故障验收进程在大输出前为 7,424KiB，大输出后 VmHWM 为 21,344KiB，约 20.84MiB。该指标是进程 RSS 高水位，不是 Go 活跃堆、整个服务 cgroup 内存或稳态内存。

这支持保留完整响应缓冲机制：本次能在返回客户端之前确认大 JSON 完整，且单请求内存没有接近 unit 的 512MiB 限制。它不能证明 64 个并发大响应也安全。仅按配置上限计算，64 × 8MiB 响应预算就已达到 512MiB，还未计入请求、临时分配和运行时；不能把允许配置的上界当成默认 unit 下已经验收的容量。

当前继续使用默认 `max-inflight=8`。增加上限时需同步调整 MemoryMax，并针对预期请求/输出大小验证。按字节限制在途内存、或校验并发与部署内存预算的关系，是比进一步压缩约 0.26ms 入口开销更有价值的后续改进；本轮没有凭单请求 RSS 简单线性外推并发峰值，也没有据此引入未经验证的缓冲优化。

## systemd 配对验收：已通过

用户将 `fs.inotify.max_user_instances` 从 128 临时提高到 1024 后，重跑同一个套件成功，进程退出码为 0，摘要为 `gateway_systemd=passed`。验收环境为一次性、禁网、私有 PID/cgroup 的 systemd 252.22 / systrap 容器，安装后的依赖位于容器 `/opt` tmpfs。

已通过的检查：

- 仅启动 Gateway 即通过 Requires/After 启动 daemon，并成功完成 Python 执行和清理。
- Gateway 使用 UID 999 的专用普通用户；运行目录为 root:专用组、0750，socket 为 root:专用组、0660。
- 不相关的 `nobody` 用户连接 Unix socket 被权限拒绝；Gateway 用户无法直接读取三份 root 私有凭据文件；内部 token 不能用于外部状态接口。
- daemon 重启后 Gateway 自动恢复。摘要中的 `gateway_state_after_daemon_restart=activating` 是重启刚返回时的瞬时状态；随后就绪检查与实际执行均通过。
- 停止 daemon 会停止依赖的 Gateway；重新启动配对入口后执行成功。
- Gateway 更新保留凭据；移除组件后原 daemon 独立 TCP 模式执行成功；重新配对保留原 Gateway 凭据并执行成功。
- base 配置与状态所有权记录保持一致；显式 purge 后受管理配置及任务状态清除。

共完成 5 次配对执行和 1 次恢复独立模式后的执行，均检查成功及任务清理。最终删除了临时容器；没有安装或切换宿主 AgentBox 服务。通过范围为上述 systemd 252 / systrap 容器，宿主配对部署及其他 systemd 版本仍需各自验收。

复现入口：

```bash
make build
bash scripts/integration-systemd-container.sh systrap cold gateway
```

### 首次阻塞及处理记录

首次运行在安装脚本执行前失败，systemd 252 的 PID 1 以 255 退出，日志为：

```text
Failed to create control group inotify object: Too many open files
Failed to allocate manager object: Too many open files
```

独立临时容器的 root 诊断进程只有 4 个描述符，RLIMIT_NOFILE 为 1,048,576，`inotify_init1` 仍返回 errno 24；当时 `fs.inotify.max_user_instances=128`。用户临时提高到 1024 后，同一配对套件通过，支持此前阻塞来自 inotify 实例额度的判断。未定位具体占用最多的进程，也未据此声称存在泄漏。此次没有写入持久 sysctl 配置。

测试运行器补充了控制台日志、失败容器状态和 PID 1 提前退出检测，便于区分环境初始化失败与部署失败。首次阻塞日志保留为 `artifacts/gateway-systemd-validation.log`，本次通过日志为 `artifacts/gateway-systemd-after-inotify.log`；JSON 已更新为通过结果并记录新日志哈希。

## 后续实现决策

1. 保留普通用户 Gateway、独立控制名额、取消传播、完整响应缓冲和禁止执行重放。真实故障及轻载数据支持继续使用这些机制。
2. 隔离 systemd 配对验收已补齐，可以继续 MCP 工具接入。宿主配对安装属于单独的部署动作，本轮没有执行。
3. 后续优先增加入口在途数、拒绝原因、转发耗时及响应字节观测，并约束并发与内存预算。当前证据不支持为了轻载性能引入流式执行结果或放宽并发。
4. 结果查询/去重在 MCP 需要处理不确定结果时再实现。取消后的清理由 daemon 保证；客户端收到断线或超时仍不等于已确认清理，不能自动重放。

复现真实验证和小样本采集：

```bash
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  AGENTBOX_GATEWAY_MEASURE=1 \
  bash scripts/qos-container.sh systrap '^TestGateway(Validation|Latency)Runtime$'
```

汇总工具 `scripts/summarize-gateway.py` 保留全部 60 个样本，拒绝缺失/重复配对和失败执行；systemd 默认要求通过，只有显式 `--allow-blocked-systemd` 才能将上述特定初始化错误保留为 blocked，绝不会转成 passed。
