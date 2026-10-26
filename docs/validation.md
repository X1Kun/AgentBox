# 验证指南

本页区分待退役旧实现的历史证据、新平台验证和 Daemon 退出验收。详细旧数据保存在[本地验收归档](archive/local-validation.md)，JSON/图表继续留在 data/assets；它们不代表新平台测试。agentboxd 已决定退役，但新平台和实际移除均未完成，状态见[路线图](roadmap.md)。

## 当前证据摘要

| 范围 | 已有证据支持 | 不能据此推导 |
| --- | --- | --- |
| 旧执行与生命周期 | KVM/systrap 指定环境下的隔离、取消、OOM、回收和重启流程 | 完整安全审计或任意存储环境稳定 |
| 旧 Warm 与库存 | READY 命中和有空闲波次的典型等待下降，库存机制可运行 | 新平台同等启动时延或持续吞吐 |
| 旧 CPU/QoS | 静态预算有局部保护价值，动态机制有真实接线与动作 | AIMD 支配静态配置或保证业务 SLO |
| 旧 Gateway/MCP | 普通用户转发、取消、无自动重放和模型错误修正闭环 | 新会话/批任务接口兼容或相同取消语义 |
| 新平台基线 | 已实现 kind 预检与普通 Job smoke 入口，真实记录见下节 | gVisor/Agent Sandbox/Kueue 集成、生产就绪或性能改善 |

<a id="platform-bootstrap"></a>
## kind/containerd 功能基线

入口见[运维指南](operations.md#platform-lab)。tests/scripts/platform_test.py 经 Go 包装纳入 make check，覆盖 context 隔离、归属缺失/变化、部分创建失败、工具校验、镜像 digest、超时、错误输出及清理；普通回归不连接 Docker。

2026-10-26 已完成三次创建/任务/删除闭环，固定结果均为 333833500，任务在专用 worker 执行，Pod 和实验集群最终回收。节点为 Kubernetes v1.36.4、containerd 2.3.4；工具为 kind v0.33.0、kubectl v1.35.8。前两轮属于集成修正过程，最后一轮创建、smoke、删除均匹配最终脚本哈希。make check 及 22 个平台回归用例通过，原有三个 kind 集群列表保持不变。

开发中保留了冷文件页等待、kubeadm 配置版本不匹配、节点回环代理、异步 Pod 回收及 kind 部分删除改写 kubeconfig 等失败记录；一次旧开发状态的恢复也单独留证。脱敏摘要、逐阶段源码/报告哈希及边界见[平台验收 JSON](data/platform-bootstrap-validation.json)，完整日志仍在忽略的 artifacts 目录，不将这些功能结果解读为生产稳定性或性能收益。

此阶段仅可信普通 Python 容器的 Kubernetes/containerd/Job 路径，不证明 gVisor 隔离、会话/预热池、Kueue 准入或批任务性能；它无需构建或启动 agentboxd。

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
- 实际 CPU、quota/节流、PSI、内存/OOM、I/O、网络和控制器/监控开销，注明层级，避免父子重复计费。
- 命令结果与清理分离，记录关闭/TTL 延迟和残留；批重试和产物校验成本全部披露。

缺测为 unknown，不补零；记录抓取/决策间隔与时间同步误差。健康许可、最终准入、Pod 启动和业务开始分别计时，不能从 reconcile 周期推出响应保证。指标失效导致批任务停滞属于策略代价；只改善交互而长期饿死批任务不算通过收益验收。

### 重复、调参与停止规则

配置、镜像和轨迹先冻结；预热准备阶段与正式窗口分开，同时报告准备与空闲成本。窗口长度依任务长度/稳态预实验选择，不承诺固定时间或启动数字。同种子配对、交错/随机策略顺序，先至少 5 次独立运行观察变异；不足时说明范围，不以大量相关请求制造虚假的窄区间。

调参与正式场景分离，不能看结果后放宽 SLO、删除最差轮次或仅保留成功任务。隔离边界突破、持续 OOM、资源归属不明或关键测量缺失时停止并记录本轮失效；业务未达 SLO 是有效负向结果，不能因此删样本。

### 原始证据与结论

每个不可变 run 保存源码/组件版本、配置与镜像 hash、资源域、有效开关、输入 manifest/种子/输出规则；客户端结果、Claim/Sandbox/Pod 身份与领取/执行/关闭事件、Job/Workload/attempt 与产物记录；健康样本、决策、准入与资源轨迹、validity 和 summary。对象/run/request ID 放日志，不作为无界 Prometheus 标签。

正式图从原始数据生成，至少分面展示会话延迟/及时成功、批产出/等待、池状态/空闲成本、实际 CPU/预算和健康/准入。Grafana 用于运行观测，不能替代原始记录。实验命令随 P1/P3/P4 实现后补入；现有平台脚本只有普通 Job 功能，旧本地命令仍按历史口径使用。

报告给出环境、对手配置、独立运行数、收益/代价和失效场景。可接受结论包括“静态配额已足够”“准入保护后续命令但加剧批等待”“在途竞争限制准入作用，需要单独评价节点控制”。不宣称未验证的生产 SLO、绝对安全、跨机或 GPU 收益。
