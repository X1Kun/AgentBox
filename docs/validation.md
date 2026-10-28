# 验证指南

本页区分待退役旧实现的历史证据、新平台验证和 Daemon 退出验收。详细旧数据保存在[本地验收归档](archive/local-validation.md)，JSON/图表继续留在 data/assets；它们不代表新平台测试。agentboxd 已决定退役，但新平台和实际移除均未完成，状态见[路线图](roadmap.md)。

## 当前证据摘要

| 范围 | 已有证据支持 | 不能据此推导 |
| --- | --- | --- |
| 旧执行与生命周期 | KVM/systrap 指定环境下的隔离、取消、OOM、回收和重启流程 | 完整安全审计或任意存储环境稳定 |
| 旧 Warm 与库存 | READY 命中和有空闲波次的典型等待下降，库存机制可运行 | 新平台同等启动时延或持续吞吐 |
| 旧 CPU/QoS | 静态预算有局部保护价值，动态机制有真实接线与动作 | AIMD 支配静态配置或保证业务 SLO |
| 旧 Gateway/MCP | 普通用户转发、取消、无自动重放和模型错误修正闭环 | 新会话/批任务接口兼容或相同取消语义 |
| 普通平台基线 | kind 预检与普通 Job 创建/执行/清理，记录见下节 | 会话/预热池、生产就绪或性能改善 |
| gVisor 运行时子步骤 | 新增固定发布包、RuntimeClass 和六项可信生命周期探针，范围见下节 | 完整网络隔离、Agent Sandbox、Kueue 或新产品 API |

<a id="platform-bootstrap"></a>
## kind/containerd 功能基线

入口见[运维指南](operations.md#platform-lab)。tests/scripts/platform_test.py 经 Go 包装纳入 make check，覆盖 context 隔离、归属缺失/变化、部分创建失败、工具校验、镜像 digest、超时、错误输出及清理；普通回归不连接 Docker。

2026-10-26 已完成三次创建/任务/删除闭环，固定结果均为 333833500，任务在专用 worker 执行，Pod 和实验集群最终回收。节点为 Kubernetes v1.36.4、containerd 2.3.4；工具为 kind v0.33.0、kubectl v1.35.8。前两轮属于集成修正过程，该阶段最后一轮创建、smoke、删除均匹配当时脚本哈希（已合入 dff78ed）；当前脚本后续扩展不回写旧记录。make check 及 22 个平台回归用例通过，原有三个 kind 集群列表保持不变。

开发中保留了冷文件页等待、kubeadm 配置版本不匹配、节点回环代理、异步 Pod 回收及 kind 部分删除改写 kubeconfig 等失败记录；一次旧开发状态的恢复也单独留证。脱敏摘要、逐阶段源码/报告哈希及边界见[平台验收 JSON](data/platform-bootstrap-validation.json)，完整日志仍在忽略的 artifacts 目录，不将这些功能结果解读为生产稳定性或性能收益。

此阶段仅可信普通 Python 容器的 Kubernetes/containerd/Job 路径，不证明 gVisor 隔离、会话/预热池、Kueue 准入或批任务性能；它无需构建或启动 agentboxd。

<a id="gvisor-runtime"></a>
## kind/containerd/gVisor 运行时子步骤

入口与安装范围见[运维指南](operations.md#gvisor-runtime)。固定 gVisor release-20261005.0 完整包、systrap、containerd 2.3.4，在本项目拥有的 Worker 中配置官方 shim 和 RuntimeClass；默认普通 runtime 保留。此阶段使用可信探针，尚未开放不可信代码服务。

2026-10-26 已完成新建集群、首次安装、只验证不重启的重复安装、全部六项运行时用例、普通 Job 回归和最终集群删除。六项均确认清理完成；make check、22 个基础平台和 17 个 gVisor 回归通过，原有三个 kind 集群保留。首次安装与最终验收的源码差异在逐阶段哈希中明示，未回写早期报告。

验收用例与证据边界：

| 用例 | 核对内容 |
| --- | --- |
| success | UID 1000、根挂载只读/拒绝写、无 ServiceAccount token、独立可写目录、正确计算结果 |
| failure | 已真实运行的程序退出 23，不能误判为启动失败或 OOM |
| cancel | 在父子进程忽略 TERM 的运行状态取消整个 Job，检查 Pod、CRI sandbox 与匹配运行时进程消失 |
| deadline | Job `DeadlineExceeded`；Pod 被原生控制器删除时不虚构退出码，另验资源回收 |
| oom | 逐页写入的有界内存负载、实际祖先内存/swap 限制、CRI OOM 原因或本 Pod 新增 `oom_kill` 事件 |
| missing | 不存在的 handler 产生明确的创建 sandbox 失败事件，没有运行中的容器，不静默回落 runc |

运行身份通过 Pod UID → CRI sandbox handler/ID → runsc state → Sentry PID/cgroup 联合核对。资源读取覆盖进程 cgroup 和祖先，验证 Pod 的 500m CPU/256MiB 内存约束及有效的零 swap 上限。部分 shim/辅助进程处于 containerd 服务组，独立记录并检查回收，不能把 Pod 用量称为全部运行时成本。

中间失败揭示了以下边界，原始报告保留：kind 节点没有 bzip2；非 root 写入错误与只读挂载应分别核对，`/tmp` 需要显式临时卷；Job deadline 可能删除 Pod；内存限制不包含 swap 上限；OCI base spec 的资源项会被 CRI sandbox 生成覆盖。最终 swap 策略由 systemd 在实验 Worker 的 `kubelet.slice` 设置 `MemorySwapMax=0`，作用于该节点全部 Pod，不改宿主机或其他集群，也不自行写 cgroup。一次中间 Docker 节点删除未收到退出事件，保留身份后重试完成，不计为首次清理成功。

结构化结果与逐阶段源码/报告哈希见[运行时验收 JSON](data/platform-gvisor-validation.json)。创建、首次安装和后续验收的源码版本分别记录；不能把开发中失败轮次改写为最终版本成功。普通 Job 基线的历史 JSON 保持原样。

NetworkPolicy 执行、精确 emptyDir sizeLimit 边界、Agent Sandbox 会话/预热池、Kueue、新 Go API/MCP、生产稳定性和性能 SLO 均不在这份验收结论内。后续完整会话验收仍按下面计划进行。

<a id="network-policy"></a>
## Calico NetworkPolicy 功能验收

入口与范围见[运维指南](operations.md#network-policy)。新建 Calico v3.33.0 / IPv4 VXLAN profile，先进入 network_pending，再安装 CNI 并等待组件、CoreDNS 和节点实际可用；与默认 kindnet 基线分开，不进行原地 CNI 替换。上游 manifest 校验、三个镜像 digest、渲染参数与组件 UID 都记录在报告中。

实际流量覆盖普通与 gVisor 两种 runtime，每种 11 条、共 22 条路径：允许的控制调用、错误入站端口、同 namespace 错误 Pod 标签、错误 namespace；允许的数据目标、错误出站端口、错误目标标签、错误目标 namespace；Kubernetes API Service 的无凭据 TCP 探测；DNS UDP/TCP。调用/数据 Pod 与目标沙箱分别位于控制面与 Worker，均为普通 Pod 网络，不以 hostNetwork、port-forward 或仅检查 YAML 代替。

同一批已就绪 Pod 按下列顺序验证，每阶段要求连续两次完整匹配，保留所有收敛中样本：

| 阶段 | 预期 |
| --- | --- |
| baseline | 全部监听和 DNS 先确认可达；响应匹配目标 Pod UID、run 和端口 |
| deny | 默认拒绝后全部路径按相应超时表现为阻断 |
| allow | 仅每种 runtime 的控制端口、指定数据目标端口和 DNS UDP/TCP 放行，其余保持阻断 |
| revoked | 删除三条允许规则，重新全部阻断 |
| restored | 删除默认拒绝，恢复原可达基线，确认拒绝并非由服务死亡造成 |

连接拒绝、HTTP 读取超时、错误 UID/响应、DNS 错误内容或 Pod 不健康都不算策略拒绝。DNS 使用新的 UDP/TCP 查询，避免把本地解析缓存当网络连通性。策略异步传播使用有界收敛窗口，不能要求 create 返回即全节点同步生效。

2026-10-27 已完成 22 条路径、5 个阶段、每阶段连续两次匹配，共 220 次流量观测；Calico 重复入口未触发 rollout，普通 Job 和 gVisor 取消回归通过。最终 make check 覆盖 22 个基础平台、17 个 gVisor 和 19 个网络回归，实验集群及探针已清理，其他集群保留。

结果与阶段源码/报告哈希见[网络验收 JSON](data/platform-network-validation.json)。失败的开发尝试包括 kubectl 多文档 JSON 输出解析，以及 CoreDNS 在无 CNI 等待期间的历史 rollout 超时；修正后检查实际可用状态，不以重启或放宽期限掩盖失败。默认 profile、gVisor 既有配置和历史 JSON 保持原口径，新结果不回写旧记录。

本轮不证明 ICMP/IPv6、hostNetwork、节点/云元数据、任意公网目的地、恶意管理员或对抗性多租户场景。网络规则不提供执行 API 鉴权、产物权限或跨用户身份保证，Agent Sandbox 会话与 Go 产品层仍需独立实现和验收。

<a id="cold-sessions"></a>
## 上游冷会话行为验收

入口见[运维指南](operations.md#cold-sessions)，固定 Agent Sandbox v1.0.8 的 controller/CRD、sandboxd 和 Go SDK，在 Calico/gVisor profile 中冷创建会话（WarmPool replicas=0）。本阶段只验证行为，不做压测、延迟分位数统计或性能收益实验；日志耗时仅用于诊断。

Go 客户端在独立受限控制 namespace 逐次调用上游接口，实际 Python 在 gVisor 会话 Pod 执行。验收覆盖同一 Claim/Sandbox/Pod/Service 身份下连续命令与文件保留、两会话文件隔离、程序失败后继续执行、新 Python 进程不保留变量、非 root/只读根/无集群凭据、文件 API 越界拒绝、会话间及 Kubernetes API 出口拒绝。host 另从第三个 namespace 验证未经授权的入口无法连接，并核对真实运行时与 500m/512MiB cgroup 约束。

关闭/到期分别检查 Claim、Sandbox、Pod、Service 删除；命令截止后不自动重放，关闭整个会话以收敛未知执行；关闭运行中忽略 TERM 的父子进程后拒绝新调用。host 最后检查对应 CRI sandbox、捕获的 gVisor 进程及本轮 namespace 清理，不能只凭 SDK Close 或客户端 Job Complete 宣告清理成功。

2026-10-27 单轮真实行为验收已通过：Go SDK 的 13 项检查、独立 namespace 的入口拒绝检查，以及 Pod→CRI→runsc/Sentry→cgroup 运行身份核对均通过；四个会话的 API/CRI 清理与三组捕获的运行时进程回收已确认，本轮三个 namespace 和临时实验集群已删除，原有三个 kind 集群保留。`make check` 通过，平台脚本回归共 70 项（22 基础、17 gVisor、19 网络、12 会话）。构建、安装及行为结果见[冷会话验收 JSON](data/platform-session-validation.json)，未生成性能图表或性能结论。

准备阶段首次 Calico rollout 和首次镜像导入超时；原始失败报告保留，同一校验/期限下重试成功，没有放宽工作负载时限。最终会话验收源码与报告分别留哈希，历史三份平台 JSON 不回写。

可信客户端的 namespace Role 不允许读 Secret、创建 Pod 或修改 Template；任务 Pod 没有 SA token。执行服务依赖受控客户端与 NetworkPolicy，尚无产品级用户鉴权。固定非零预热池、池耗尽、控制器重启/Pod 故障恢复、新产品 API/MCP、持久化产物、精确磁盘容量和对外不可信用户服务不在此轮结论内，空闲回收/动态会话容量也未定策。

<a id="warm-pool"></a>
## 固定预热池行为验收

入口见[运维指南](operations.md#warm-pool)。此轮使用同一固定版本的上游组件与执行镜像，库存目标固定为 2，工作 namespace 总 Pod quota 为 3。它验证库存供给与总容量的职责边界，不测量预热带来的时延或吞吐收益。

行为流程：两个 Ready 的预热 Sandbox/Pod → 两个并发 Claim 保持原 UID 且互不重复 → 剩余预算补出新库存并领取第三个会话 → 第四次创建在配额阻断下有界失败并回滚 → 关闭已用会话 → 两个全新库存恢复 → 再次领取的文件环境干净且可执行 → 停止池并回收。

Go 检查覆盖固定目标、领取身份、文件隔离、补充、耗尽等待、关闭后拒绝调用及干净环境恢复。host 额外要求可关联的 Claim/Sandbox UID、当前代的 quota 拒绝状态及已占满的 ResourceQuota，随后检查失败 Open 的资源删除；一般超时不能算容量保护成功。运行时证据仍来自 CRI→runsc/Sentry→cgroup，成功结束要同时确认 WarmPool/Claim/Sandbox/Pod/Service、CRI 与捕获进程的清理。

2026-10-28 重试后，完整 `session-test --session-profile warm` 入口已成功退出：九项 Go 检查、失败 Open 的配额拒绝与回滚证据、六个实际 gVisor Pod 的运行身份/资源证据，以及 WarmPool、会话对象、CRI、捕获进程和 namespace 清理均已通过。结果与逐阶段源码/报告哈希见[验收 JSON](data/platform-warm-pool-validation.json)。不进行压测、性能对照或收益推断。

失败历史保留：首次受原生 controller-manager/scheduler 不健康阻塞，未创建测试 namespace；重试时两轮普通 kind 引导又在管理员绑定阶段失败，节点日志显示原生镜像快照解包/提交被取消。待原生镜像解包完成后，使用原 kind 配置重试 Kubernetes 官方 kubeadm init/join，重新核对归属、版本与两个节点，再继续 Calico/gVisor/会话组件。该恢复不修改健康标准或工作负载时限，不能视为普通冷引导稳定性通过。

首轮实际 warm 流程完成九项 Go 检查后，结束标记的 exec 与客户端退出竞态导致回执 137。原失败报告保持不变；独立核对该 Job 的成功终态、完整成功事件和资源清理后，修正验收器仅对最后标记的 137 等待明确完成证据而不重放，随后用新 namespace 重跑完整入口成功。另补充了导入后 CRI 缓存的有限同步等待，标签与 image ID 仍严格核对。

完整 `make check` 已通过；会话脚本回归现为 29 项，平台脚本合计 87 项，Go warm 身份/就绪保护测试 3 项。测试 namespace 与临时实验集群均已清理，其他集群保留；节点首次删除未收到退出事件，保留归属后重试完成。最终检查结果记录在 JSON。控制面健康检查、归档校验缓存、逐节点镜像身份和未完成 ctr 操作保护继续保留。

固定 replicas 不限制已领取会话总数，池为空时上游可能冷创建；总量边界由另设的 Kubernetes ResourceQuota 执行。该测试采用有限 SDK 等待及失败清理，并未交付产品级 admission/容量拒绝接口。未领取库存归 WarmPool 控制器管理，SDK 关闭已领取会话不会停止整个池；host 按 UID 删除池后再验回收。

此轮不包括控制器重启、Pod 故障恢复、TTL/关闭竞争、动态容量、idle/暂停、生产多租户鉴权或产品 API/MCP。历史冷会话与性能证据保持原样，不把行为通过解释为性能提升或生产就绪。

<a id="runtime"></a>
<a id="warm"></a>
<a id="qos"></a>
<a id="static"></a>
<a id="gateway"></a>
代表性数字保留在证据源：固定波次 Cold/Warm 成功 P50 为 331.64/182.98ms，而 P95 改善很小；共享 2 CPU 的固定场景中，父组 1 CPU＋PSI 仅补池配置两轮 6,480 个 Core 请求均在 50ms 内成功，但门控未触发，不能归因于动态策略。所有配置、任务代价、失败反例和版本条件见[归档](archive/local-validation.md)，不将这些数字转用于 Kubernetes 后端。

## 旧代码检查与真实复现

本节及后续本地/MCP 命令针对当前仍保留的旧源码，仅用于迁移期回归和历史复现，不是目标平台验收。P2c 后需使用归档代码版本；活动 CI 将改为新平台检查。所有命令在对应仓库根目录执行，普通检查不启动付费 API 或特权运行时；真实用例另需 runsc/rootfs、Docker 和可用平台。

```bash
make check
make build
```

make check 包含 Go race、基准摘要、vet、格式/差异检查及部署和实验脚本回归。按改动范围再选择真实用例；不把单元测试或 tmpfs 验收当作普通磁盘验证。

```bash
python3 scripts/container.py runtime --platform kvm
python3 scripts/container.py runtime --platform systrap
python3 scripts/container.py systemd --platform kvm --mode warm
python3 scripts/container.py systemd --platform systrap --mode warm
```

Gateway 和 QoS 短流程：

```bash
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py gateway --platform systrap
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py qos --platform kvm
```

动态库存的单独机制回归：

```bash
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py qos --platform systrap --pattern '^TestDynamicInventoryRuntime$'
```

该用例验证真实库存阻塞、扩缩及清理；不证明策略收益。宿主服务 smoke 与显式启停/SIGKILL 检查见[运维指南](operations.md#smoke)，执行前确认服务作用范围。

<a id="mcp"></a>
## 旧 MCP 与模型验证

```bash
make check build
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py mcp --platform systrap
```

普通 MCP 流程使用确定性模拟模型 API 和真实沙箱。历史真实 deepseek-flash 记录为 3 次模型请求、2 次执行，从除零错误修正为 42，两次确认清理；见[脱敏证据](data/deepseek-live-validation.json)。不把单次演示当作性能或普遍自愈能力。

付费 API 仅通过下列显式入口调用：

```bash
AGENTBOX_LIVE_DEEPSEEK=1 \
  AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py live --platform systrap
```

默认读取 `.local/deepseek.env`，或由 AGENTBOX_DEEPSEEK_CONFIG 指定私有文件，只解析允许的 KEY=VALUE，不执行 shell 内容。上限为 4 次模型请求、3 次工具调用、100 秒对话；只有该套件的控制容器使用 bridge 出站访问 API，runsc 任务仍禁网。普通容器验证均保持禁网。

<a id="local-experiments"></a>
## 本地性能实验入口

实验需选择对应资源和工作负载口径；详细方法、参数、失败条件和逐批结果见[归档](archive/local-validation.md#pool-experiments)。

| 问题 | 现有入口/详细说明 |
| --- | --- |
| Warm 命中、突发、持续、波次 | scripts/container.py benchmark；[供给实验](archive/local-validation.md#pool-experiments) |
| 配额与延迟目标下的容量 | benchmark --scenario capacity；[容量扫描](archive/local-validation.md#容量扫描寻找可用边界) |
| 补池与 Core 争用 | benchmark --scenario refill-core；[补池对照](archive/local-validation.md#补池与-core-争用对照) |
| 固定 CPU 保护与代价 | scripts/container.py static；[静态方法](archive/local-validation.md#static-method) |

### 本地动态 CPU 对照

沿用现有实验入口，不新增脚本：

```bash
make build
AGENTBOX_TEST_IMAGE_ROOTFS=1 python3 scripts/container.py static --platform kvm --comparison dynamic --duration 15 --repetitions 1
```

每组包含三个各 15 秒的阶段，固定 1 CPU、固定 2 CPU 与 AIMD 1～2 CPU 使用同一工作量、5/60/5 req/s 核心到达率、两个沙箱客户端。保持静态库存、关闭准入门控，避免把多个策略收益混在一起。预先设定 50ms 核心 SLO，记录拒绝/丢弃、超标秒数、完成量、配额轨迹和恢复；一轮结果不支持显著性推断。2026-10-21 的未达标结果及限制见[详细记录](archive/local-validation.md)。

<a id="recording"></a>
## 记录与离线分析

```bash
# 启动受控的短观测实验；已有本地依赖后执行。
make build
python3 scripts/container.py observe --platform kvm

# 记录已运行的 observe/enforce 服务；token 从环境读取。
python3 scripts/experiment.py record --output artifacts/observe-manual \
  --duration 30 --rate 20 --sandbox-workers 2

# 读取历史数据，不启动负载；恢复原始数据的方法见 validation.md。
python3 scripts/experiment.py analyze artifacts/static-kvm-XCmQHtIX \
  --output /tmp/agentbox-analysis --no-plots
```

记录器保存逐请求 Core/沙箱 CSV、状态 JSONL、配置和质量摘要。成功、拒绝、客户端丢弃、其他失败分开；enforce 的动作来自 daemon，记录器不修改策略。容器观测为 2 CPU，idle-pool 仍包含库存与观察开销，不能当独占业务基线。

离线分析只写新的输出目录并记录源文件哈希。Core 三种时间口径使用同一窗口、同一批成功请求；沙箱阶段只匹配正式 CSV 中成功且清理完成的 execution_id，排除 warmup。drain 完成请求保留阶段数据，但不计入窗口吞吐。

历史图表和口径核对见[分析归档](archive/local-validation.md#observation-analysis)；不要在缺失旧字段时补写新观测值。

<a id="evidence"></a>
## 历史资料与复现

[原始数据恢复说明](archive/local-validation.md#evidence)保存归档位置、恢复命令和版本边界。[理论分析](archive/performance-theory.md)及[引用索引](archive/performance-references.md)为历史研究资料，不再承担当前开发计划。data/assets 的现有文件未迁移或重算。

<a id="limits"></a>
## 已知限制

普通磁盘 create/delete 超时尚未根治；tmpfs 成功不等于磁盘修复。动态库存独立收益、业务健康借用收益、采集净开销、长期生产运行、多租户公平与物理跨机性能仍缺完整证据。新会话与批任务平台、AdmissionCheck/Koordinator 不以本地证据代验。

<a id="session-validation"></a>
## 新会话功能与启动测量（计划）

P1/P2 独立验证上游 Agent Sandbox＋containerd/gVisor，不复用旧 Warm 数字。没有启动 agentboxd/旧 Gateway 时，分别通过上游客户端和新 Go API/MCP 验证：

| 项目 | 必须取得的证据 |
| --- | --- |
| 运行边界 | 实际 runsc handler、Pod UID、业务与运行时进程资源归属；无普通 runtime 静默回落 |
| 会话行为 | 同会话多条命令可读写工作目录；新进程不宣称继承 Python 变量，跨会话不可读写 |
| 领取与容量 | 冷创建、固定池命中、池耗尽、并发 Claim、配额耗尽；有限等待/失败，无重复分配 |
| 执行结果 | 正常/非零退出、OOM、输出超限、命令截止、API 断线与结果未知分别表达 |
| 身份与权限 | 创建响应丢失后查询同一对象，归属检查、文件越界/符号链接、模型与集群凭据隔离 |
| 终止与回收 | 同会话并发拒绝，关闭/TTL 竞态、Pod 失败、上游控制器重启；无法确认命令终止时关闭会话收敛 |
| 池成员卫生 | 已领取/执行过的环境不回到干净池；替代创建与终止中资源有记录，残留不误报清理成功 |
| 网络 | 实际允许必要控制通道并拒绝其余流量；不能只以存在 NetworkPolicy YAML 作为证据 |

首轮启动测量区分以下时间，分别记录缓存、预热和存储条件：

- 冷创建：请求发起到上游对象及执行接口实际可用，包含调度/运行时/服务初始化。
- 预热领取：请求发起到取得独占会话并能执行，不能只量 CR 创建返回时间。
- 首条命令：另列从会话申请开始的首个正确结果时间，以及在就绪后提交该命令的执行延迟。
- 后续命令：客户端提交到完整结果，记录类型、输入规模、拒绝、超时和未知结果。
- 关闭：关闭请求到资源收敛，包含终止中和失败残留；不混入业务执行时间。

为理解任务粒度，可用相同图片程序分别运行一次性 Job 和会话内命令，注明“一次操作”“一份输入”“一次会话”各自边界。预热构建/空闲成本单列，不将预热领取与冷 VM 启动或纯计算时间直接比较；不预设亚秒目标或复制厂商数字。

<a id="retirement"></a>
## 新平台迁移与 Daemon 退役验收（计划）

P1/P2 新会话链路独立证明安全与正确性即可切换，不要求旧同步 API 或 Warm 行为等价，也不等待批任务策略和 Koordinator。至少核对：

| 验收项 | 必须取得的证据 |
| --- | --- |
| 独立运行 | agentboxd/旧 Gateway 未安装或未运行时，真实会话与新 API/MCP 完成执行、文件访问与关闭 |
| 平台所有权 | 会话 Pod 由上游控制器管理，执行与资源在其 Pod 内，由 kubelet/containerd 承担运行，无旁路宿主沙箱 |
| 新调用契约 | 版本化工具、稳定创建身份、会话与命令期限、并发边界、未知结果不重放和关闭后收敛 |
| 旧入口退出 | 调用方已切换，旧 API/MCP 不误投新会话/任务，也不成为自动回退目标 |
| 旧资源收敛 | 旧请求排空或明确取消，残留处理、服务/进程/监听/凭据清退记录 |
| 活动发布收敛 | 构建、CI、部署和默认文档无 agentboxd 必需依赖；旧版本及证据可复现 |

P3 再独立验收批任务提交/取消、输入冲突、Job attempt 与产物语义，不以会话验收替代。退役步骤见[路线图](roadmap.md#retirement)、[运维指南](operations.md#retirement)。修改文档、安装 containerd 或单个 Job 成功都不能替代验收。

<a id="platform"></a>
## 会话与批任务共享资源实验（计划）

以下用于 P3/P4 及后续阶段，尚无对应新平台成绩。核心问题是固定预热池和共享预算下，批任务健康准入相对静态配额的收益与代价。

### 假设与资源域

| 假设 | 最小比较 | 可以否定该假设的结果 |
| --- | --- | --- |
| 预热改善首次可执行等待 | 同模板冷创建 vs 固定预热领取，另计预热成本 | 收益不足、池耗尽频繁或空闲成本过大 |
| 健康准入有价值 | 调优静态批配额 B vs 同配置＋AdmissionCheck A | 会话无明显改善，或批任务等待/产出代价不可接受 |
| 节点压制有额外价值 | 有在途干扰证据后，补 K/AK 消融 | 原生静态机制已足够或动态压制使权衡恶化 |
| 机制可迁移 | 冻结策略后使用未参与调参的场景/任务 | 只在单个自研负载有效 |

首轮固定一个 Linux Worker 资源域，会话、空闲预热成员和批 Job 的资源归属明确；控制面、监控、压测器尽量域外。记录 CPU/SMT/steal、内核、cgroup 驱动、祖先 quota/weight/cpuset、内存/swap、磁盘/网络和背景负载。kind 用于功能验证，同机独立 CPU 集仍可能共享 LLC、内存与内核，不能冒充物理多机。

主交互负载为固定类型的真实会话命令，校验结果和时限。首条与后续命令分开，创建/领取失败与业务执行失败分开；后续命令正确及时成功作为首版准入反馈。Mock Core 只作辅助诊断，不替代真实产品路径。图片批任务固定 manifest、库/镜像版本、尺寸、编码参数、线程数与校验规则，不能用 sleep 或必然超时的无限任务计成功吞吐。

### 固定池与静态基线

| 组别 | 会话与预热 | 批任务 | 健康 AdmissionCheck | Koordinator |
| --- | --- | --- | --- | --- |
| R | 固定池、固定模板与到达轨迹 | 无提交 | 关闭 | 关闭 |
| B | 与 R 相同 | Kueue 调优静态配额与固定规格 | 关闭 | 关闭 |
| A | 与 B 相同 | 与 B 相同 | 开启 | 关闭 |
| K | 与 B 相同的共同兼容资源模型 | 与 B 相同 | 关闭 | 开启 |
| AK | 与 K 相同 | 与 K 相同 | 开启 | 开启 |

P3 完成 R/B 和多档静态批配额扫描；P4 比较 B/A；P5 仅在必要时补 K/AK。所有有批任务组都由 Kueue 管理，关闭健康检查不等于取消资源边界；首轮会话和预热池不由 Kueue 管理。

正式组之间固定池目标、会话规格与上限、批模板、配额、运行位置、runtime、镜像缓存、输入/产物机制和观测配置。记录实际空闲/领取/创建/终止成员及资源，不能把 A 组额外预热容量算成准入收益；资源总预算包含空闲池、补池和控制开销，配额不等于物理预留。

先在调参集选择满足交互目标、批产出较好的静态配置，再冻结进入正式集；可报告事后最优点作诊断，不把它当可部署预知策略。P5 若引入 batch 扩展资源、不同 cpuset 或调度依赖，必须重新建立 B/A/K/AK 共同基线，不能拼接旧轮次。

### 到达轨迹与场景

先用 R 标定本环境会话容量与 SLO，冻结输入复杂度、会话寿命、命令间隔与创建轨迹。不照搬历史绝对 QPS。首批只选三类：

- Steady：稳定会话负载与固定批任务积压。
- Step：低→高→低交互负载与持续批任务，观察等待、在途干扰和恢复。
- Fault：同轨迹下注入指标陈旧、Controller 重启或权限失败。

先用小型顺序会话测试验证功能；正式干扰实验按预定开放到达轨迹发起命令，明确每个请求的目标会话与每会话单执行边界。并发冲突要记录为拒绝，不能因服务变慢自动减少请求后再宣称延迟改善。池耗尽测试另列，不与稳定已就绪会话样本混成一个 P99。

可用 k6 open arrival-rate 场景；一次 iteration 对应一条预定命令或明确标记的会话获取操作，记录 offered/sent/completed、失败和 dropped_iterations。生成器不能维持轨迹时本轮测量无效并保留证据。[到达模型](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/)、[丢弃迭代](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/dropped-iterations/)

批任务清单、到达和分片固定，不随完成速度改变输入；未准入、未完成和 drain 完成的任务都保留。后续再增加短突发、超过独占容量、I/O 瓶颈转移和未参与调参的应用。

### 统计、成本与故障有效性

同时报告：

- 会话获取/首次结果/后续命令各自 P50/P95/P99，正确及时成功数量与比例；错误、拒绝、超时、未知和未发出请求不隐藏，不平均各 Pod P99。
- 批任务校验成功的有效工作量、全部完成时间、按期完成比例、排队与最长等待、积压/未完成年龄；窗口外 drain 不计窗口吞吐。
- 池命中与耗尽、空闲/领取/创建/终止成员的数量和驻留时间，预热准备时间、CPU 时间、内存占用随时间及失败补池成本。
- 分别记录模型推理等待、工具执行和会话空闲等待；已领取但等待模型的会话与未领取预热成员分别计成本，用于评估后续生命周期策略，不预设某种回收或暂停方案有效。
- 实际 CPU、quota/节流、PSI、内存/OOM、I/O、网络和控制器/监控开销，注明层级，避免父子重复计费。
- 命令结果与清理分离，记录关闭/TTL 延迟和残留；批重试和产物校验成本全部披露。

缺测为 unknown，不补零；记录抓取/决策间隔与时间同步误差。健康许可、最终准入、Pod 启动和业务开始分别计时，不能从 reconcile 周期推出响应保证。指标失效导致批任务停滞属于策略代价；只改善交互而长期饿死批任务不算通过收益验收。

### 重复、调参与停止规则

配置、镜像和轨迹先冻结；预热准备阶段与正式窗口分开，同时报告准备与空闲成本。窗口长度依任务长度/稳态预实验选择，不承诺固定时间或启动数字。同种子配对、交错/随机策略顺序，先至少 5 次独立运行观察变异；不足时说明范围，不以大量相关请求制造虚假的窄区间。

调参与正式场景分离，不能看结果后放宽 SLO、删除最差轮次或仅保留成功任务。隔离边界突破、持续 OOM、资源归属不明或关键测量缺失时停止并记录本轮失效；业务未达 SLO 是有效负向结果，不能因此删样本。

### 原始证据与结论

每个不可变 run 保存源码/组件版本、配置与镜像 hash、资源域、有效开关、输入 manifest/种子/输出规则；客户端结果、Claim/Sandbox/Pod 身份与领取/执行/关闭事件、Job/Workload/attempt 与产物记录；健康样本、决策、准入与资源轨迹、validity 和 summary。对象/run/request ID 放日志，不作为无界 Prometheus 标签。

正式图从原始数据生成，至少分面展示会话延迟/及时成功、批产出/等待、池状态/空闲成本、实际 CPU/预算和健康/准入。Grafana 用于运行观测，不能替代原始记录。批队列与策略实验命令随 P3/P4 实现后补入；当前平台脚本提供普通 Job、gVisor、Calico 网络及上游冷会话/Go SDK 功能验收，旧本地命令仍按历史口径使用。

报告给出环境、对手配置、独立运行数、收益/代价和失效场景。可接受结论包括“静态配额已足够”“准入保护后续命令但加剧批等待”“在途竞争限制准入作用，需要单独评价节点控制”。不宣称未验证的生产 SLO、绝对安全、跨机或 GPU 收益。
