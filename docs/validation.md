# 验证指南

本页区分待退役旧实现的历史证据、新平台验证和 Daemon 退出验收。详细旧数据保存在[本地验收归档](archive/local-validation.md)，JSON/图表继续留在 data/assets；它们不代表新平台测试。agentboxd 已决定退役，但新平台和实际移除均未完成，状态见[路线图](roadmap.md)。

## 当前证据摘要

| 范围 | 已有证据支持 | 不能据此推导 |
| --- | --- | --- |
| 旧执行与生命周期 | KVM/systrap 指定环境下的隔离、取消、OOM、回收和重启流程 | 完整安全审计或任意存储环境稳定 |
| 旧 Warm 与库存 | READY 命中和有空闲波次的典型等待下降，库存机制可运行 | 新平台同等启动时延或持续吞吐 |
| 旧 CPU/QoS | 静态预算有局部保护价值，动态机制有真实接线与动作 | AIMD 支配静态配置或保证业务 SLO |
| 旧 Gateway/MCP | 普通用户转发、取消、无自动重放和模型错误修正闭环 | 新 Job 接口兼容或相同取消语义 |
| 新平台 | 目前只有设计与实验协议 | 组件兼容、生产就绪或性能改善 |

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

普通磁盘 create/delete 超时尚未根治；tmpfs 成功不等于磁盘修复。动态库存独立收益、业务健康借用收益、采集净开销、长期生产运行、多租户公平与物理跨机性能仍缺完整证据。Kubernetes 后端与 AdmissionCheck/Koordinator 不以本地证据代验。

<a id="retirement"></a>
## 新平台迁移与 Daemon 退役验收（计划）

新链路独立证明安全与正确性，不要求 Warm、同步等待或旧 API 行为等价。至少核对：

| 验收项 | 必须取得的证据 |
| --- | --- |
| 独立运行 | agentboxd/旧 Gateway 未安装或未运行时，真实 Job 与新 API/MCP 完成任务 |
| 平台所有权 | 实际计算、运行时进程和资源归属 Job Pod；由 kubelet/containerd 管理，没有旁路本地沙箱 |
| 新调用契约 | 版本化提交/查询/取消、断线后继续、重复输入冲突、未知结果查询、attempt 与产物校验 |
| 旧入口退出 | 调用方已切换，旧 API/MCP 不误投新任务，也不成为新平台自动回退目标 |
| 旧资源收敛 | 旧请求排空或明确取消、残留处理、服务/进程/监听/凭据清退记录 |
| 活动发布收敛 | 构建、CI、部署和默认文档无 agentboxd 必需依赖；旧代码版本与证据仍可复现 |

退役完成条件和操作步骤分别见[路线图](roadmap.md#retirement)、[运维指南](operations.md#retirement)。文档修改、安装 containerd 或单个 Job 成功都不能替代上述验收；Koordinator 不作为退役前置条件。

<a id="platform"></a>
## 新平台实验协议（计划）

以下仅用于未来阶段，尚无对应新平台成绩。

### 要回答的问题


| 假设 | 最小比较 | 可以否定该假设的结果 |
| --- | --- | --- |
| 健康准入有价值 | 调优固定配额 vs 同配额＋AdmissionCheck | 在线无明显改善或批任务等待代价不可接受 |
| 节点压制有额外价值 | 无节点压制 vs 原生 Koordinator | 原生资源机制已足够或压制导致更差权衡 |
| 两者互补 | 四组消融 | 联动没有额外收益或造成振荡/饥饿 |
| 机制可迁移 | 冻结策略后使用未参与调参的场景/应用 | 只有单一自研计算接口有效 |

CPU 利用率上升不能单独证明收益。功能通过、测量有效、性能改善分别记录；负向结果仍是交付。

### 固定资源域与工作量

首轮固定一个 Linux Worker，在线 Deployment 与离线 Job 共用明确资源域；控制面、监控和 k6 尽量在域外。记录 CPU 型号、逻辑/物理核、SMT、虚拟化/steal、频率、温度背景、内核、cgroup 驱动、祖先 quota/weight/cpuset、内存、swap、存储及网络。

kind 用于功能验证；同机独立 CPU 集也可能共享 LLC、内存和内核，不能冒充物理独立机器。无法观察的祖先限制和背景干扰明确列出。

在线先使用现有 Mock Core 的固定计算与校验能力，适配 Kubernetes 部署及业务指标，不根据策略改变单请求工作量、并发或入口超时。后续增加一个独立服务验证，不能只凭自研服务声称普适性。

离线选择一份固定图片 manifest，固定库版本、尺寸、编码参数、线程数和输出规则。通过工作量构造短/长任务，不用 sleep 或永远运行后超时的任务计成功吞吐。runc/gVisor、缓存准备、数据分发和存储条件在所有策略组一致。

### 基线与消融

所有平台组均使用 Kueue 管理队列，基础配额始终存在。“关闭健康准入”不表示取消基础资源预算。

| 组别 | 基础平台 | 自研健康检查 | Koordinator CPU Suppress |
| --- | --- | --- | --- |
| R | 只有在线业务及同套观测 | 无离线请求 | 关闭 |
| B | Kueue 固定配额＋固定任务规格 | 关闭 | 关闭 |
| A | 与 B 相同 | 开启 | 关闭 |
| K | 与 B 相同的兼容资源模型 | 关闭 | 开启 |
| AK | 与 K 相同 | 开启 | 开启 |

P1 完成 R/B 和多档固定配额扫描；P2 比较 B/A；P3 才补 K/AK。相同任务规格下，配额可约束最大运行规模，但不把 CPU 配额直接称为任意任务的精确并发槽。

正式比较前在调参集选择满足在线目标、批任务产出较好的静态配置，冻结后用于正式集。可额外报告事后最优静态点作为诊断，不冒充可部署的预知策略。

P3 必须让 B/A/K/AK 的请求资源、预算上限、权重、任务、调度位置、runtime 和采集保持一致；如引入 batch 扩展资源、调度插件或不同 cpuset 模式，应建立共同兼容基线或拆成单独机制实验，不能直接拼接历史 B/A 数据归因。

首轮不同时开发 PSI 替代策略。只有原生 Koordinator 对照暴露可复现问题后，才单独比较同执行器下的信号/策略，保持 SLO guard、调参预算和其他功能开关一致。

### 到达轨迹与评估场景

先在在线独占时扫描容量 C，提前确定实验 SLO、正确性规则和可接受到达偏差。流量按本环境容量比例构造，不能跨机器照搬绝对 QPS。

首批只选三类：

- Steady：稳定中等在线负载与固定批任务积压。
- Step：低→高→低在线负载与持续计算任务，观察等待、压制和恢复。
- Fault：同一轨迹下注入指标陈旧、Controller 重启或权限失败。

之后再增加短突发、超过独占容量、I/O 瓶颈转移和未参与调参的应用。过载场景用于暴露边界，不要求有限资源满足任意 SLO。

使用 k6 open arrival-rate 场景，一次 iteration 对应一个请求；记录计划到达、实际发起、完成、失败和 dropped_iterations。到达率无法维持时标记试验无效并保留证据，不能把实际负载下降后的延迟当收益。[到达模型](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/)、[丢弃迭代](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/dropped-iterations/)

批任务清单、到达时间和输入固定，不随响应速度临时增减任务。队列等待、未准入和未完成任务都属于结果。

### 统计与有效性

在线同时报告：

- offered/sent/completed、正确成功、错误、拒绝、超时和未发出请求。
- 正确且在 SLO 内完成的数量/测量时间，即及时成功 goodput。
- 正确且按时完成数占预先定义请求集合的比例；客户端丢弃另列并参与到达有效性判断。
- 成功延迟 P50/P95/P99、全窗口及预定义突发/恢复窗口；不平均各 Pod P99，不隐藏超时。

离线同时报告窗口内校验成功的工作量、固定清单全部完成时间、排队时间、最长等待、积压及未完成任务年龄。窗口外 drain 完成保留但不计窗口内吞吐；失败、重试、启动和产物校验成本全部披露。

观察 quota、实际 CPU、节流、PSI、内存、OOM、I/O 与 Controller/监控开销，区分父子层级，避免重复累加。Kueue 配额、CPU usage 和物理可用 CPU 是不同口径。

缺测为 unknown，不补成零；记录抓取/决策间隔分布、时间同步误差和采集边界。异步启动延迟单列，不能从查询周期推出控制效果。

### 重复、调参与停止规则

配置、镜像和轨迹先冻结；预热不计主窗口，缓存方式固定。正式窗口根据任务长度与稳态预实验选择，初始可采用 5～10 分钟，不作为未校准的固定承诺。

同种子配对，各策略交错或随机顺序，先安排至少 5 次独立运行观察变异；不足时明确报告范围，不作显著性结论。以独立 run 计算配对差异和不确定性，不能用大量相关请求制造虚假的窄区间。

正式评估场景与调参场景分离。不能看结果后放宽 SLO、删掉最差轮次或只保留成功任务。安全边界突破、持续 OOM、资源归属不明、关键证据缺失时停止本轮并标为无效；业务达不到 SLO 是有效负向结果，不能因此删轮次。

### 原始证据与产物

拟议每个不可变 run 目录保存：

- manifest：源码 commit、组件/镜像版本、配置 hash、资源域、全部有效开关。
- workload manifest：输入哈希、任务规格、轨迹与种子、输出校验规则。
- 客户端原始结果、Job/Pod attempt 与结果验证记录。
- 健康样本、AdmissionCheck 决策/状态、Kueue 预留/准入事件、Koordinator 配置和实际动作证据。
- 环境与资源采样、validity、summary、固定脚本生成的图表。

文件名、版本锁与实验命令随 P0/P1 实现后固定，当前没有新增可运行入口。已有单机四脚本入口继续有效；新平台使用单独配置/场景，避免改变旧结果的复现口径。

图表至少分面显示在线及时成功/延迟、批任务有效产出/等待、实际 CPU/预算、健康判定/准入以及样本有效性。Grafana 用于运行观测，正式图从归档原始数据生成。

### 结论模板

报告同时给出环境、对手配置、任务类型、独立运行数、收益/代价和失效场景。可接受结果包括“健康准入降低在线违约但显著增加批任务等待”“Koordinator 已足够，额外扩展无收益”或“某配置下联动改善权衡”。

不得宣称未验证的生产 SLO、跨机收益、GPU 迁移或完整安全性。历史单机数据不改写成新平台成绩。
