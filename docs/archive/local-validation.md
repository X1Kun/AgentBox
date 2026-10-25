# 本地后端历史验收与复现

归档于 2026-10-25。下文保留原实验日期、数字、条件及失败记录；不是当前平台路线或新版本重测结果。常用入口与结论摘要见[验证指南](../validation.md)。

本文与其附录中的命令均从仓库根目录执行；移动文档不改变原始 artifacts 路径或实验文件内容。

最近实验更新：2026-10-22；文档范围说明更新：2026-10-25。本页统一保存本地执行、预热池、QoS、Gateway、MCP 的结果及适用范围；配置和复现入口分别见各功能说明。历史批次不自动代表当前版本重新完成性能测试。

新方向为 Kubernetes＋Kueue 平台、健康感知 AdmissionCheck 及后续 Koordinator 对照，均尚无本项目实现/验收结果。规划见[实施计划](../roadmap.md)，未来证据遵循[平台实验协议](../validation.md#platform)并单独标记后端与版本。本次文档改版不改变下述历史数据，也不表示重新运行了这些实验。

## 同日后续：预算余量、PSI 补池与 AIMD 组合

已增加 `qos-refill-only`：复用 protected CPU PSI 的短窗、确认和滞回，仅暂停新后台补池，不改 CPU、准入、队列代次或在途任务；在 observe 下预演，enforce 下执行，与 CPU/完整压力调度互斥。父组 CPU 通过 `parent-cpu-milli`／`AGENTBOX_PARENT_CPU_MILLI` 正式配置，默认 4000；AIMD 上界必须不超过该预算，固定服务增加 QoS CPU 上下界。默认配置及宿主服务未改变。

重读本书印刷页 330～331、613～615、648，分别核对压力时间、资源上限/优先级和单因素对照；CPU PSI 接口按 Linux 6.8 文档核对。书中没有给出本项目的 20%/10%、确认 1 秒、低压稳定 2 秒，也不证明这些值能保障 50ms SLO，见[引用索引](performance-references.md#b16)。

**预先定义六方案、两轮反向排序**：共享组 2 CPU、外层 6 CPU/5GiB、固定 Core 60 req/s/50,000 次 SHA-256、相同 54 秒波次、执行/创建并发均 4、总实例 8、Warm 目标 3、队列 100ms。静态组也开启同套 observe 采样，PSI 组去掉 idle，仅按压力控制补池；组合组使用既有 AIMD 1～2 CPU、完整压力准入与补池，BaseCPU=0，没有启用业务健康文件。库存目标不动态。仅对同预算下的两种补池条件、同条件下的两档预算作单因素解释，组合组按整体方案评价，不与上一批次直接相减。

**验收条件在运行前固定**：Core 全窗口、沙箱到达窗口、其余初始空闲/恢复窗口，分别要求至少 99% 全部请求在 50ms 内成功，且成功 P99 ≤50ms。拒绝/错误/客户端丢弃均计入代价，不能只看全窗口成功延迟。

| 方案 | Core 全窗口 P99，两轮 ms | 到达窗口 P99，两轮 ms | Core 目标通过 | 沙箱完成数，两轮（每轮50） |
|---|---:|---:|---:|---:|
| Core-only | 20.05 / 18.45 | 21.86 / 16.93 | 2/2 | 无请求 |
| idle＋2 CPU | 101.98 / 105.68 | 146.27 / 141.42 | 0/2 | 39 / 40 |
| idle＋1 CPU | 21.71 / 31.06 | 23.89 / 61.30 | 1/2 | 38 / 39 |
| PSI 仅补池＋2 CPU | 114.86 / 148.20 | 147.13 / 167.20 | 0/2 | 39 / 39 |
| PSI 仅补池＋1 CPU | 20.31 / 20.68 | 21.38 / 22.63 | 2/2 | 33 / 36 |
| AIMD＋完整压力调度 | 100.22 / 101.79 | 146.63 / 161.45 | 0/2 | 26 / 26 |

**达标运行点与收益归因分开**：1 CPU＋PSI 仅补池两轮共 6,480 个 Core 请求均在 50ms 内成功、无拒绝，三个窗口都达标；该配置可作为本实验资源与工作量下的部署参考。但两轮均未观测到 PSI 暂停，不能宣称动态门控使其达标，证据主要支持 CPU 预算余量的重要性。idle＋1 CPU 第二轮全局按期率 99.51%、P99 31.06ms，看似达标，但到达窗口按期率 97.78%、P99 61.30ms，且有 2 次拒绝，故保留为失败；逐请求发起滞后很小，超期发生于真实 Core 执行/等待，未改判为压测端问题。

**动态不自动优于保守配置**：2 CPU 的 PSI 仅补池有 326/334 个暂停采样点，动作真实发生，但仍未保护到目标；采样中首次暂停约在 3.25～3.45 秒，Core 在初始补齐阶段已发生超期。组合方案有 7/9 次真实写回，实际额度在 1.6～2 CPU 间变化，仍未达标且只完成 26/50 个沙箱请求。反馈需要采样和持续确认，不能追回已经发生的等待，也不能取消已开始创建。

**单看 Core 压力不等于保护沙箱前台**：PSI 仅补池＋1 CPU 在 Core 低压时继续并行准备，沙箱成功 P50 约 1186.85/1263.09ms，idle＋1 CPU 为 318.45/399.16ms；完成数也更少。不能把“Core 达标”说成整体性能全面改善，选择时需明确业务优先级及拒绝/等待代价。

`make check build`、34 个部署/入口回归、真实 PSI 高压/恢复与缺测保持、PSI 仅补池不改准入/CPU、systemd 252/KVM Warm 生命周期与父组 1 CPU/实例 0.5 CPU 接线均通过。完整回归还暴露并修复 Gateway 截止时间竞态：后端恰在截止点返回空响应时，按绝对截止时间分类为 504；保留 outcome_unknown，不重放，增加单次转发回归。

共核对 38,880 条 Core、500 条沙箱请求，355 个成功实例全部唯一且确认清理；12,877 帧没有读取错误、不健康或 OOM 增量，最终 Core/shared 与沙箱子组、bundle 均回收。所有 Core 传输/响应校验错误和客户端丢弃为 0；PSI 仅补池组所有采样中实际准入均开放、CPU 变更计数为 0，固定组最终额度不变。新鲜 protected 采样占比约 99.7%以上，目标 50ms 资源采样的实际最大间隔约 380ms；不将采样峰值视为精确瞬时峰值。

本轮形成了有边界的保护验收点，不是通用最优、所有 Python 负载或裸机实时保证。若继续研究动态借用，应从已校准的保守额度启动，并验证真实业务健康反馈；不能用本轮结果代替该可选模式的性能验收。原始数据、配置/哈希、控制轨迹和检查日志归档 `artifacts/psi-protection-validation-20261022.tar.gz`；汇总见[证据 JSON](../data/performance-theory-evidence.json)的 `psi_protection_validation_20261022`。

复现沿用 [Core 对照入口](#补池与-core-争用对照)，增加 `--refill-comparison protection`。现阶段可进入文档/提交收尾，不继续在同一轨迹上搜索参数。

## 2026-10-22：补池时机与 Core 争用

增加可选 `warm-refill-policy=idle`：任一前台任务持槽时暂停新的后台创建预留，按需创建仍可用，已开始创建继续完成；前台清理结束后恢复补池。默认 `slots` 保留原有槽满／排队退让条件。新增后台创建次数与“预留时已有前台任务”计数，不仅凭 creating 采样判断机制。

`make check build`、race 摘要及 33 个部署/入口回归通过。真实 KVM 验证了 idle 提前退让、缺货按需创建、全新实例、库存恢复与清理；systemd 252/KVM Warm 生命周期及 idle＋0.5 CPU 的 unit 展开、内核限额、执行与回收通过。未更新宿主服务。

**固定竞争组对照**：外层 6 CPU／5GiB，Core 与 Sentry/Gofer 共享 2 CPU 子树；压测端与 runsc 控制 CLI 在竞争组外。沙箱父组 2 CPU、单实例 1 CPU，执行/创建并发均 4，总实例 8、Warm 目标 3、FIFO 100ms，QoS/动态库存关闭。Core 每请求 50,000 次 SHA-256、服务并发 4、60 req/s，50ms 是实验期限。KVM、有界 tmpfs 依赖与状态，不代表普通磁盘或裸机性能。

每个 profile 同轨迹 54 秒，沙箱两轮 5/1s＋空闲10s＋20/5s＋空闲10s，不等 READY、不重试、不按响应推迟到达。第一轮 baseline／Cold／Warm-slots／Warm-idle，第二轮反向排序；baseline 保留空闲 Cold 引擎与相同采样，只不发沙箱任务。启动与最终停服销毁不计入窗口，同一 Core 进程持续运行。

| Profile | Core 成功 P99，两轮 ms¹ | Core 按期成功率，两轮¹ | 沙箱完成数，两轮（每轮50） | 沙箱成功 P50，两轮 ms |
|---|---:|---:|---:|---:|
| Core-only | 20.06 / 19.43 | 100% / 100% | 无请求 | — |
| Cold | 125.98 / 119.57 | 82.78% / 82.10% | 36 / 37 | 1234.58 / 1198.37 |
| Warm-slots | 148.01 / 132.17 | 79.10% / 79.10% | 37 / 37 | 1153.03 / 1190.11 |
| Warm-idle | 99.85 / 96.40 | 83.30% / 84.48% | 39 / 40 | 647.58 / 603.98 |

¹ 从计划到达计时。Core 每轮每 profile 3,240 请求，按期率分母包含拒绝；对应拒绝数 Cold 229/215、slots 271/283、idle 186/176，基线均无拒绝。沙箱未完成项都是容量拒绝，不能只凭成功分位数声称可靠性改善。

slots 窗口内后台预留 36/36 次，全部在已有前台任务时启动；idle 为 31/29 次，前台在途时启动均为 0。两种 Warm 每波前都自然恢复 READY=3。相同预算和到达轨迹下，两轮 idle 都降低 Core P99、减少拒绝并增加沙箱完成，支持此策略在本场景中的作用，不能外推任意负载。

Core 在沙箱到达期间的按期率，slots 为 34.17%/33.75%，idle 为 49.03%/54.86%；其余初始空闲和恢复期间为 slots 91.94%/92.06%、idle 93.10%/92.94%。全窗口结果不能掩盖恢复期干扰。沙箱 P95 未全面改善：第一轮 idle 1683.53ms 略高于 slots 1658.10ms，第二轮为 1461.57/1770.90ms。

**idle 仍未守住 Core 50ms 目标。** 基线约消耗 0.70 CPU，而沙箱仍可使用共享组全部 2 CPU；减少部分后台创建不能替代 CPU 预算余量或业务反馈。下一步固定 idle，对照更保守的父组配额及沙箱代价；当前不提高上限、开启每实例 AIMD或改变服务默认配置。

核对了 25,920 条 Core、300 条沙箱请求及 226 个唯一成功实例，清理全部确认；8,554 帧无读取错误、不健康或 OOM 增量，最终 bundle、子组与 Core/shared 树回收通过。Core 无传输/响应校验错误或客户端丢弃，全窗口发起滞后 P99 ≤1.22ms。资源目标采样间隔 50ms，但实际最大间隔约 652ms，一次首样本晚约 398ms；积分仅覆盖实际首尾，不声称精确瞬时峰值或完整初始空闲 CPU 成本。请求统计和调度动作计数不依赖该采样间隔。

仅两轮固定轨迹、反向排序，无置信区间或生产流量。原始 CSV/JSONL、配置/哈希和检查日志归档 `artifacts/refill-core-validation-20261022.tar.gz`，汇总见[既有证据 JSON](../data/performance-theory-evidence.json)的 `refill_core_validation_20261022`；复现见[补池与 Core 对照](#补池与-core-争用对照)。

## 同日后续：执行槽／CPU 配额对照与单实例限额配置

已增加服务端 `-sandbox-cpu-milli`／`AGENTBOX_SANDBOX_CPU_MILLI`，默认 1000（1 CPU）、范围 50～4000；鉴权 status 增加配置值。Cold 新建、Warm 库存及补池共用同一模板，父组默认仍 4 CPU，请求不能指定资源。KVM 在 Cold/Warm、500/1500 milli-CPU 四个组合下实际读回子组和父组限制，执行与回收通过，Warm 替换库存仍保持额度。systemd 252/KVM Cold 生命周期也通过：环境变量 500 实际写成子组 `10000 20000`，父组保持 `80000 20000`，任务成功清理；更新、回退、重装与卸载保持原语义。`make check build`、31 个部署/入口回归通过；未更新宿主服务。

**固定负载下的五组对照**：同一测试二进制，10 req/s，Cold/Warm 各 200 请求，1 秒／95% 按期成功目标；创建并发 4、总实例 8、Warm 目标 3、单实例 1 CPU、父组内存 4GiB、队列 16／8MiB／100ms 均固定。外层 6 CPU／5GiB，KVM、tmpfs 依赖及控制目录，QoS/动态库存关闭，无 Core 压测服务。五组按 8槽/4CPU、2槽/4CPU、8槽/2CPU、4槽/4CPU、8槽/3CPU 串行，每组 Cold 在先；不是完整参数搜索。

| 执行槽 / 父组 CPU | Cold / Warm 完成数¹ | Cold / Warm 按期成功率 | Cold / Warm 成功 P95 |
|---|---:|---:|---:|
| 2 / 4 | 124 / 155 | 62.0% / 77.5% | 433.69 / 434.02ms |
| 4 / 4 | 189 / 182 | 94.5% / 91.0% | 527.37 / 578.99ms |
| 8 / 4 | 150 / 152 | 15.5% / 36.0% | 1268.81 / 1247.75ms |
| 8 / 3 | 109 / 112 | 3.0% / 5.5% | 1740.18 / 1694.08ms |
| 8 / 2 | 85 / 87 | 1.0% / 1.0% | 2326.63 / 2254.58ms |

¹ 每项分母均为 200，未完成项都是容量拒绝；按期率分母包含拒绝。全部 2,000 条请求分类、成功清理和 2,572 帧资源记录核对通过，无执行/传输/基础设施错误、OOM、缺测、实例超预算或外层 CPU 节流；最终 bundle 和子 cgroup 目录检查通过。

在相同父组 4 CPU 下，Cold 从 8 槽改为 4 槽，按期完成由 31 增至 189，父组累计节流约 110.35→0.95 秒，成功执行阶段中位数约 742.87→306.56ms；创建准备约 248.22→94.84ms，清理约 44.90→18.09ms。**执行槽满是准入受限的直接现象，不能单据此增加槽位。** 本轮更支持限制过多在途生命周期、使执行并发与 CPU 预算匹配；没有用阶段分位数相减推断精确因果。

固定 8 槽，父组 2→3→4 CPU 时两路完成数上升、P95 下降，说明固定 CPU 配额确实限制了这组工作负载。它不证明物理 CPU 耗尽；本轮也未突破现有 4 CPU 上界。Cold 2槽/4CPU 基本无父组节流却拒绝较多，说明执行槽同样可能成为限制，两者不是二选一。

**Warm 仍有待定位的争用**：4槽/4CPU 时 Warm 的 setup 中位数约 14.94ms，明显低于 Cold 的 94.84ms，但 Warm 完成数更少、父组累计节流约 39.32 秒，高于 Cold 的 0.95 秒。这提供了补池与前台共享额度可能产生干扰的线索；没有单独隔离补池变量，不能把全部差异直接归因于补池，也不能用命中收益宣称持续容量领先。

没有任何组合达到本次 95% 目标；4槽/4CPU 是本轮更合理的候选，尚未校准生产运行点。每路仅约 20 秒到达窗口、单次顺序比较，无重复或随机交错，不能声称全局最优或与上一批次直接作提升归因。后续优先用波次＋Core 对照验证补池干扰，再考虑前台资源预留；按任务资源规格与单实例动态控制另行实现。服务默认参数保持不变。

原始配置、CSV、JSONL、systemd/runtime/check 日志统一归档 `artifacts/cpu-policy-validation-20261021.tar.gz`；摘要与哈希见[既有证据 JSON](../data/performance-theory-evidence.json)的 `cpu_policy_validation_20261021`。复现使用[容量入口](#容量扫描寻找可用边界)，固定 `--capacity-rates 10 --capacity-samples 200 --create-concurrency 4 --max-sandboxes 8`，只改变 `--execution-concurrency` 或 `--capacity-parent-cpu`。

## 容量阶梯：旧并发限制不是电脑上限

2026-10-21，宿主 i7-9750H（6 物理核／12 SMT 线程）、约 31.26GiB 内存。新增独立 capacity 场景；执行并发可配置上限从 4 扩至 8、创建从 2 扩至 4，总实例仍最多 8，默认配置不变。真实 KVM 短流程在 Cold/Warm 各并发提交 8 个独立任务，执行和回收通过；这不是最优并发或硬件极限的证明。

容量探索固定执行 8、创建 4、总实例 8、Warm 目标 3、队列 16／8MiB／100ms，关闭动态 QoS/库存。外层 `cpu.max=600000 100000`／5GiB，父组 `80000 20000`／4GiB；固定 Hello World、KVM、tmpfs 依赖及有界 tmpfs 控制目录，Cold 后 Warm。仅逐级提高请求率，预先设定 1 秒期限／95% 按期成功目标，每档 200 请求；越界停止，不运行更高档。总计 1,200 请求、六个负载窗口，扫描用例约 216 秒。

| 请求率 | Cold 完成／发起 | Warm 完成／发起 | Cold / Warm 按期成功率¹ | Cold / Warm 成功 P95¹ |
|---|---:|---:|---:|---:|
| 4/s | 200/200 | 200/200 | 100% / 100% | 328.34 / 170.50ms |
| 8/s | 200/200 | 200/200 | 100% / 100% | 436.50 / 219.29ms |
| 12/s | 134/200 | 135/200 | 22.5% / 27.5% | 1207.58 / 1232.95ms |

¹ 从计划到达时刻计时，包含发起滞后。按期成功率的分母包括所有已发起请求；成功分位数不包括拒绝。12/s 分别拒绝 66/65，按期成功为 45/55，窗口内完成交付为 7.56/7.62 次/秒；不能把全部完成数或快速 503 当作按期有效交付。全部六档均无执行/传输/基础设施错误、OOM、无效采样或内存停止；逐请求核对全部成功均清理，结束 Engine.Close 和 bundle 检查通过。

**两种模式均在 8/s 达标、12/s 不达标；这是待细分的容量区间，不是 8/s 的精确上限。** Warm 的中低负载延迟更低，过载容量未拉开。12/s 到达窗口中，执行槽满的采样占比为 Cold 86.1%、Warm 66.3%；创建并发满仅约 3.0%／3.6%，虽然两者都曾达到峰值。父组累计节流约 97.12／98.68 秒（多 CPU 累计计数，不等于墙钟停顿），CPU PSI 差分约 57%，外层节流为 0。沙箱父组采样内存峰值不超过 328MiB，外层不超过 828MiB。证据更支持先核对父组带宽限制与任务持槽时间，暂不支持盲目把创建并发继续加大；没有证明物理 CPU 已耗尽或最终瓶颈只有一种。

本轮只有单次顺序扫描；不同负载窗口约 17～50 秒，只用于粗定位。下一次需要容量结论时，在 8～12/s 细分，延长并重复边界附近档位；再固定其他配置，分别扫描执行并发和创建并发。不能与旧 60 请求批次直接作提升归因，也不外推长计算、普通磁盘、真实 Gateway 或核心服务混部。复现和停止规则见[容量扫描](#容量扫描寻找可用边界)，书中依据见[理论补充](performance-theory.md#capacity)。

`make check build` 通过，覆盖 race、摘要回归、脚本回归、vet、格式和构建；最初受沙箱本地 socket 权限限制，授权后完整重跑通过。原始 CSV、六份资源 JSONL、配置/二进制哈希摘要和日志统一归档 `artifacts/capacity-validation-20261021.tar.gz`；汇总见[既有证据 JSON](../data/performance-theory-evidence.json)的 `capacity_validation_20261021`。测量后的统计回归将发起滞后扩展到包括拒绝在内的全部请求；离线核对六档全部请求 P95 滞后均小于 1.5ms，不改变本次分类。性能数据只代表归档哈希的测量二进制。

## 波次请求：利用空闲提前准备，降低下一波等待

2026-10-21 新增 waves 场景，未修改生产调度。固定种子先生成相对到达轨迹，Cold/Warm 原样重放：共同空闲 2 秒后，每轮为 1 秒内 5 请求、空闲 10 秒、5 秒内 20 请求、空闲 10 秒；两轮各 50 请求、54 秒。回放中不等 READY、不重试拒绝，也不因响应慢而延后下一波。固定 Hello World、KVM、外层 6 CPU/5GiB、创建并发 2、执行并发 4、总上限 6、Warm 目标 3、FIFO 100ms；QoS/动态库存关闭，runsc/rootfs 为 tmpfs 测试依赖。

| 指标 | Cold | Warm |
|---|---:|---:|
| 完成 / 发起 | 44/50 | 46/50 |
| 容量拒绝 / 基础设施错误 | 6 / 0 | 4 / 0 |
| 成功请求计划到达至响应 P50 | 331.64ms | 182.98ms |
| 同口径 P95 | 422.70ms | 415.53ms |
| 四波首请求响应范围 | 257.98～280.33ms | 133.91～143.69ms |
| 沙箱父组时间加权平均内存 | 9.44MiB | 108.56MiB |
| 命名空间根累计 CPU 时间 | 16.10 CPU秒 | 19.63 CPU秒 |

逐请求核对了双方相同的计划到达时间，共 100 行；成功均确认清理。每波开始前 Cold 的 READY 为 0，Warm 自然恢复为 3。Warm 的前 3 波在到达窗口结束后的首个采样点就观察到满库存，末波在结束后约 101ms 采样到恢复；观测间隔为 50ms，不表示精确恢复时刻，未使用补池屏障制造命中。

**本次结果支持在该波次轨迹中减少首请求和典型等待，不证明持续吞吐提高。** P95 改善很小，Warm 的 CPU 与待命内存成本更高；完成数不同也影响 CPU 总量。资源覆盖整个 54 秒回放及空闲期，根计数包含测试驱动、CLI、init 和共享依赖；不含启动、依赖准备、最终停服销毁。只运行一条轨迹，Cold 在前 Warm 在后，每波样本少，不能外推为生产 P95 或任意流量收益，也没有测同机核心业务干扰。

`make check`、race 摘要回归及真实 KVM 回放通过；2,157 个原始资源样本无读取错误或执行器不健康。原始 CSV、轨迹/摘要、库存资源 JSONL 和日志归档为 `artifacts/wave-arrival-validation-20261021.tar.gz`；摘要追加至 [既有证据 JSON](../data/performance-theory-evidence.json) 的 `wave_arrival_validation_20261021`。

## 同日后续：启动探测、补池退让与业务反馈

按顺序落地：启动必须完成可信 Python 创建/执行/销毁探测才开放 readiness；超时现场关联挂载和设备开始/结束计数；执行槽饱和或存在 FIFO 请求时暂停后台补池；可选业务健康约束从配置的 base CPU 启动，业务恶化时收回借用额度，持续恶化可降至 min，失效信号禁止恢复。最后一项默认关闭，依赖核心服务或可信采集器提供实际窗口计数，不自动替用户采集生产 SLO。

**补充独立基线**：外层 2 CPU/5GiB，保持相同 5/60/5 req/s、每段 15 秒的核心请求；测量期没有沙箱工作或库存，保留空闲 daemon/observer，启动探测在窗口之外。1,050 个请求全部成功，442 帧观测有效；高负载 900/900 成功，P99 16.88ms，三段均没有 50ms SLO 超标秒。说明目标在本次环境中可达，不证明任意混部配额都能达标；与上一轮是不同时间的单次测量。

**补池退让短测**：沿用上轮创建并发 2、执行并发 4、总容量 6、Warm 目标 3、FIFO 100ms；6 CPU/5GiB KVM，runsc/rootfs tmpfs 和固定镜像，使用 init。启动探测成本不计入任务时延。

| 场景 | Cold 完成/发起 | Warm 完成/发起 | Cold/Warm 成功 P50 |
|---|---:|---:|---:|
| 库存充足 | 60/60 | 60/60 | 272.05 / 147.09ms |
| 瞬时突发 | 3/32 | 4/32 | 439.03 / 312.47ms |
| 持续 10 req/s | 52/60 | 51/60 | 470.50 / 420.47ms |

304 个请求无基础设施错误，其余为容量拒绝。Warm 本轮持续完成数更接近 Cold，但没有重复批次和显著性证明，不把 49→51 全部归因为补池策略，也不声称持续吞吐领先。

`make check build` 通过（race、vet、Python 部署/测量回归、格式和构建）。真实 KVM 验证了启动前拒绝、探测成功、探测 exec 失败不就绪、启动中关闭回收；Cold/Warm 并发隔离、2GiB 预算、Warm 超时/OOM/取消/清理失败恢复与动态库存回归通过。业务约束已验证真实 cpu.max 读回、健康文件正常/异常/过期到 status 的接线；没有新增该模式的业务性能对照。原 PSI-only AIMD 的性能数据保留在下节，不挪用为新模式收益。

本地原始结果归档到 `artifacts/control-refinement-20261021.tar.gz`；摘要追加到 [既有证据 JSON](../data/performance-theory-evidence.json) 的 `sequential_refinement_20261021`。磁盘慢 I/O 尚未根治，本轮没有修改宿主部署或运行新的参数搜索。

## 2026-10-21：可靠性、统一供给与动态对照

本轮实现：Cold/Warm 统一有界 FIFO 与创建配额；Warm 缺货按需创建；清理兜底保留独立 2 秒预算；启动时校验宿主/祖先 cgroup 内存预算并在 status 报告；超时前采集有界内核现场。默认仍为 Cold、动态 QoS/库存关闭。以下为单轮探索，未做参数寻优，不能将与历史批次的差异全部归因于代码。性能样本对应各自记录的二进制哈希；后续内存校验及队列计数修正另有回归，不视作最终源码的全面性能验收。

`make check` 通过 race、vet、Python 回归与格式检查；真实 KVM 验证覆盖 Cold/Warm 共 12 次并发全新实例、超时、OOM、取消、清理失败停接单及重启修复。2GiB 委派子树拒绝超出预算的 Warm 配置，改为 1GiB 父组/256MiB 实例后执行与回收通过。未更新宿主已部署服务，本轮未重跑配对 systemd 全套。

**同预算冷暖短测**：KVM，外层 6 CPU/5GiB；双方创建并发 2、执行并发 4、总上限 6、FIFO 16/8MiB/100ms，Warm 目标 3。runsc/rootfs 位于 tmpfs，rootfs 来自固定 Python 镜像；使用 Docker init，benchmark 的控制状态未额外挂入 tmpfs。

| 场景 | Cold 完成/发起 | Warm 完成/发起 | Cold/Warm 成功 wall P50 |
|---|---:|---:|---:|
| 库存充足、顺序请求 | 60/60 | 60/60 | 265.33 / 150.11ms |
| 瞬时突发 | 4/32 | 4/32 | 450.69 / 329.40ms |
| 10 req/s，约 6 秒 | 52/60 | 49/60 | 461.47 / 457.59ms |

304 个请求全部分类，无基础设施错误；非完成项均为 503。新路径保留 READY 命中收益，但持续完成数 Warm 仍略低，不证明 Warm 提升吞吐，也不证明历史退化已经在所有环境消除。旧 Cold 可随 4 个执行槽并行创建，与本轮双方共享 2 个创建名额不同。

**动态 CPU 对照**：KVM，外层 2 CPU/5GiB，tmpfs 依赖与测量目录；每组连续运行 45 秒，核心请求率依次为 5/60/5 req/s，每段 15 秒；两个沙箱客户端持续执行固定工作量。比较固定 1 CPU、固定 2 CPU、AIMD 1～2 CPU；动态库存与压力准入门控均关闭，预先设定核心服务 50ms SLO。

| 策略 | 高负载成功请求 P99 | 高负载失败/丢弃 | 高负载 SLO 超标秒数¹ | 45 秒内沙箱完成数 |
|---|---:|---:|---:|---:|
| 固定 1 CPU | 73.76ms | 12/900 | 5/15 | 16 |
| 固定 2 CPU | 135.43ms | 161/900 | 15/15 | 25 |
| AIMD 1～2 CPU | 93.69ms | 72/900 | 13/15 | 26 |

¹ 按一秒窗口检查：有核心失败/丢弃或成功请求 P99 超过 50ms 即计超标；低压每秒仅 5 个样本，不作稳定 P99 推断。所有三组恢复阶段均从首秒开始出现连续三个达标窗口。共 3,150 个核心请求、1,338 帧有效观测；沙箱无拒绝/错误，回收检查通过。核心失败/丢弃保留在结果中，未算成成功延迟样本。

AIMD 发生 5 次配额变化，实测 2→1.6→1.28→1.024→1.524→2 CPU。首次降额约在测量第 17.70 秒（高负载开始约 2.70 秒后），高负载末段已开始恢复。相比固定 2 CPU，观察到较低的核心 P99；相比固定 1 CPU，保护更弱。它没有守住 50ms SLO，也未证明支配静态方案，默认继续关闭。没有无沙箱高负载基线、重复批次或置信区间，不作生产收益承诺。下一步若继续研究，应核对 PSI 与业务拒绝/SLO 的对应关系，尤其是业务拒绝减少实际工作量后 PSI 是否会提前回落；本轮不调整参数追求好看结果。

**普通磁盘现场**：一次 create 超时捕获到 runsc CLI 为 D 状态，栈为 `filemap_fault → folio_wait_bit_common`；此时目标 cgroup populated=0、memory.current=0、OOM 计数为 0。依赖位于 ext4，宿主 I/O PSI 较高。这次故障发生于文件缺页等待阶段，不能归因于尚未发生的任务执行/cgroup 销毁，也不能外推所有历史超时同因。普通磁盘问题仍未修复；该次测试 observe 子用例失败、enforce 子用例通过，不合并成通过结论。

摘要、配置与失败快照合并在 [既有理论证据 JSON](../data/performance-theory-evidence.json) 的 `implementation_review_20261021` 中；本地原始 CSV/JSONL/日志统一归档为 `artifacts/implementation-validation-20261021.tar.gz`（Git 忽略）。

<a id="runtime"></a>
## 执行隔离与生命周期

- 2026-10-12～16，KVM/systrap 的 cold/warm 真实执行用例通过，覆盖 stdin/stdout、UID 1000、环境允许列表、Python main/pickle、代码错误、用户退出码 125/128、超时、输出洪水、只读根、禁网、工作目录隔离、后台进程销毁、OOM、取消、容量拒绝和重启回收。
- 输入准备在 umask 0022/0077 下保留镜像权限；根及挂载点显式 0755，任务输入只读。fork 测试降低 guest rlimit 后验证拦截，不声称实测精确的 64 进程边界；OOM 专门区分软水位与硬限制。
- 每任务全新实例，待命 init 保持 Sentry 存活，使用附着 runsc exec 获取短任务退出码；清理失败保留 intent 并停止接单。
- create/start 已开始时在独立预算内完成，避免取消导致半写元数据。空闲库存四路回收，清理确认前仍占容量。
- systemd 252 容器的 KVM cold/warm、systrap warm 验收包括执行中停止、主进程 SIGKILL 后恢复、更新/回退/卸载重装和 purge。注入每实例删除延迟 2 秒，8 实例停止约 4.1～4.4 秒；不是生产停止时间上界。
- 2026-10-15 宿主 systemd 249 的 KVM cold 服务另有安装与生命周期记录，见 [部署说明](../operations.md#deployment)。后续 Gateway/MCP 只在隔离容器验收，没有替换该宿主服务。

平台固定使用项目记录的 runsc release-20261005.0 和 Python digest；功能容器通常为 6 CPU/5GiB/2304 host tasks、私有 PID/cgroup。功能通过不等于完整隔离安全证明或固定时延承诺。

<a id="warm"></a>
## 预热池与冷暖对比

历史 KVM 基准时间为 2026-10-15，内核 6.8.0-111-generic，Go 1.25.11；执行并发 4、固定 warm 库存 3、创建并发 1、总上限 6、队列 16/8MiB/100ms。每路先探测两次，inventory/steady 各 60 请求，burst 32 请求，steady 仅约 6 秒。

| 模式 | 场景 | 发起 / 完成 / 拒绝 | 成功 wall P50 | 成功 wall P99 |
| --- | --- | ---: | ---: | ---: |
| cold | inventory | 60 / 60 / 0 | 287.78ms | 387.23ms |
| cold | burst | 32 / 4 / 28 | 418.92ms | 442.47ms |
| cold | steady | 60 / 49 / 11 | 405.68ms | 497.72ms |
| warm | inventory | 60 / 60 / 0 | 156.40ms | 372.63ms |
| warm | burst | 32 / 3 / 29 | 184.40ms | 207.68ms |
| warm | steady | 60 / 34 / 26 | 247.29ms | 560.30ms |

全部 304 行核对无传输或基础设施错误；成功必须确认清理，拒绝单列，P99 为 nearest-rank，小样本 burst 的尾分位接近最大值。库存充足时 setup P50 从 72.57ms 降至 10.24ms；warm steady 成功数反而更低，说明 READY 缓冲与持续创建产能不同。阶段分位数不能相减作严格归因。

3 READY 时父组 memory.current 单次约 99.29MiB，不是单实例固定成本。以上支持提前准备降低命中等待，不证明动态库存策略提高长期吞吐或节省固定比例内存。2026-10-20 动态库存 v2 已通过实际阻塞归因、缩容稳定、容量守恒和一次真实目标调整/回收；收益对照仍未完成。

<a id="qos"></a>
## 观测与 QoS

- 只读真实内核采集通过：CPU/内存与一秒 I/O/PID 增量有效，目录变化、缺测、过期和计数回退按无效数据处理。
- 确定性压力驱动真实 cpu.max：observe 保持 `80000 20000`，enforce 确认写入 `64000 20000`。
- 真实 PSI 测试使受保护组限为 0.05 CPU 作为压力源；observe 不改变接单，enforce 拒绝新请求与排队者、保持在途任务、暂停补池，缺测时保持限制，低压稳定后恢复。
- KVM 普通磁盘依赖曾在 runsc create/delete 或 Python 启动预算内失败。准备 runsc/rootfs 到容器 tmpfs 后，上述控制与执行链路通过。新观测字段的普通磁盘 KVM 端到端短测失败仍是已知问题，没有用延长超时改判成功。
- 真实 PSI 到控制器的功能通路已验证；测试压力源不证明沙箱噪声的因果，也没有动态 QoS 相对静态保护的业务收益结论。

<a id="static"></a>
## 静态 CPU 对照

2026-10-17，Linux 6.8.0-138-generic、KVM、外层 2 CPU/5GiB，九组各有一分钟比较窗口。配置与统计方法见 [实验说明](#static-method)。

| 组别 | 三轮 Core P99（ms） | 三轮沙箱完成率（次/秒） |
| --- | --- | --- |
| 无沙箱基线 | 19.64 / 21.44 / 17.15 | 0 / 0 / 0 |
| 默认 4 CPU 父级配额 | 94.52 / 81.11 / 80.64 | 0.550 / 0.533 / 0.567 |
| 静态 1 CPU 父级配额 | 21.77 / 23.77 / 22.88 | 0.367 / 0.367 / 0.367 |

共 12,600 个 Core 请求成功，177 个正式沙箱任务成功且清理（默认 105、protected 72）；6,225 帧状态全部可用，无拒绝、客户端丢弃、传输、基础设施或采样错误。窗口外 drain 完成不计窗口吞吐。

降低静态配额使 Core P99 接近基线，同时降低沙箱完成率。此结论限于外层 2 CPU、固定工作量和旧版本，不外推裸机或动态控制收益。[实验 JSON](../data/static-cpu-experiment.json)记录版本、哈希、逐轮配置和结果；[离线分析](../data/observability-analysis.json)关联到全部 177 个任务。依赖快照准备约 508 秒，在测量窗口之外，不能算入 API 时延。

早期中止批次出现 delete 超时，没有对应 OOM 增量；Docker init 和宿主侧 tmpfs 修正后正式批次未复现。缺少现场内核栈，未确定旧失败的精确根因。

![正式三轮静态对照](../assets/static-cpu-comparison.png)

<a id="gateway"></a>
## Gateway 验收与开销

2026-10-20，普通用户 UID 1000 Gateway，经 Unix HTTP 调用 root Engine 和真实 systrap runsc。

- 入口上限 1 时多余请求约 0.764ms 返回 gateway_capacity；同次 readyz/status 均可访问。
- 客户端取消、Gateway SIGTERM 后，确认清理并补足库存的单次观测约 50.194ms/13.214ms；任务资源删除、容量恢复。不是取消延迟分位数。
- 完整 1MiB NUL 输出转义后约 6MiB JSON，未截断；单请求进程 RSS 高水位约 20.84MiB。不能证明 64 个并发大响应适配默认 512MiB unit，默认仍为 8。
- 两路交替、每路预热 3 次后采样 30 次，均成功且清理：Unix 直连 wall 中位数 170.859ms/P90 200.545ms，Gateway 为 177.594ms/195.549ms。
- 对每个请求计算客户端 wall 减 Engine total_ms，再逐对相减，中位数约 0.257ms。这是轻载下入口路径开销估计，包含客户端与编码，不能当纯 Gateway CPU 时间；端到端中位数之差不能全部归因于 Gateway。

[完整 60 个样本与摘要](../data/gateway-validation.json)保留原始精度。条件为 warm、库存 2、执行并发 1、总容量 4、systrap、tmpfs 依赖；不报告 P99、吞吐、KVM 或普通磁盘收益。

隔离 systemd 252/systrap 配对生命周期通过：专用普通 UID 999、root:专用组 0750 目录/0660 socket，其他用户不能连接，Gateway 不能读取 root 凭据文件；启动依赖、daemon 重启后恢复、停止传播、Gateway 更新、移除恢复独立模式、保留凭据重新配对和 purge 均通过。共检查 5 次配对与 1 次独立执行。

首次 systemd PID 1 因 inotify_init 返回 EMFILE 退出；root 诊断只有 4 个 FD、RLIMIT_NOFILE 为 1,048,576，当时实例限额为 128。用户临时提高为 1024 后同一套件通过，未确定占用最多的进程，没有声称发生泄漏。宿主配对服务没有切换。

<a id="mcp"></a>
## MCP 与真实 DeepSeek

实际普通用户 MCP/Gateway stdio 链路通过成功、代码异常、超时、RPC 取消与直接关闭传输的回收；模拟 API 的 3 次模型请求、2 次真实执行完成除零错误后修正为 42。SDK 优雅 Close 会等在途调用，不能当作实际断线；进程生命周期另显式取消 HTTP 转发。

配置私有凭据后，真实 deepseek-flash API 同样以 3 次模型请求、2 次真实 runsc 执行完成 failed/ZeroDivisionError → succeeded/42，两次 cleanup_complete 均确认。Agent/MCP/Gateway 使用实际编译二进制，API key 不进入 MCP 或任务环境，未出现在输出与待提交文件中。[脱敏证据](../data/deepseek-live-validation.json)记录实际计数、最终回答和日志哈希；详细用法见 [MCP](../usage.md#mcp)。

真实 API 用例必须显式选择，普通 make check 不调用付费模型。当前无跨宿主机或多 Worker 验收；历史后续设想现由[开源平台路线](../roadmap.md)取代，kind 仅用于新平台的功能验证。

<a id="evidence"></a>
## 原始数据与复现

本次整理后，artifacts 保留近期关键验收日志、宿主部署摘要与 `reference-data.tar.gz`。归档中保存原始路径下的正式静态九组 CSV/JSONL/日志和冷暖 requests.csv/summary.json；派生绘图、失败重试和旧流水账已移出活跃目录。

```bash
# 按原路径恢复原始数据，随后可离线分析，不启动负载。
tar -xzf artifacts/reference-data.tar.gz -C .
python3 scripts/experiment.py analyze artifacts/static-kvm-XCmQHtIX \
  --output /tmp/agentbox-analysis --no-plots
```

历史 JSON 中的绝对路径、脚本哈希和记录版本是当时来源，不是当前文件布局或当前版本性能成绩。源码重构不修改这些历史数字；恢复原始数据后可核对哈希。只保留一种规范 JSON 汇总，删除重复导出的 docs CSV。

整理前 docs/scripts/artifacts 的完整备份位于 `.local/repo-cleanup-20261020-165120.tar.gz`，备份位置、压缩包 SHA-256 另见 `.local/cleanup-backup.json`。它未包含 DeepSeek 配置，不进入 Git；仅在需要回看历史材料时恢复。其余旧验收和失败批次也保存在这份完整备份中。新实验仍使用独立输出目录，不自动覆盖或删除用户已有结果。

## 当前检查与未验证项

普通 `make check` 检查 Go race、vet、脚本语法、部署与测量边界，真实平台测试需显式开启。本次整理已通过完整 make check、公共入口/引用检查、一次新入口驱动的真实 MCP/systrap 短流程及 systemd 配对生命周期。压缩原始数据解包后，经新分析入口重算得到相同的 9 组有效对照和 177 个匹配任务；未重跑长时间性能实验或付费 API 演示。

普通磁盘 create/delete 超时的精确根因、新采集净开销、动态 QoS/库存收益、多租户公平与物理跨机资源隔离仍未取得完整证据，不能用本页功能通过替代。


<a id="host-systemd"></a>
## 附录：宿主 systemd 验收

用户在本机终端完成管理员安装、启动和 `python3 scripts/deploy.py smoke --lifecycle`，原始证据保存在本机被 Git 忽略的 `artifacts/host-systemd.json`。本节保留可提交的验收摘要。

| 项目 | 实际结果 |
|---|---|
| 记录时间 | 2026-10-15 02:33:03 UTC（北京时间 10:33:03） |
| Linux | `6.8.0-111-generic` |
| systemd | `249.11-0ubuntu3.23` |
| runsc | `release-20261005.0`，KVM |
| 服务 | `agentbox.service`，`Type=notify`，`Delegate=yes` |
| cgroup | `/sys/fs/cgroup/system.slice/agentbox.service` |
| 最终状态 | `ready=true`；只读核对时为 `active/running` |
| 父级约束 | 4 CPU 等效带宽，memory.high 3 GiB、memory.max 4 GiB、swap 0、pids.max 2048 |
| 单沙箱约束 | 1 CPU 等效带宽，memory.high 384 MiB、memory.max 512 MiB、swap 0、pids.max 256 |
| 运行时归属 | Sentry 与 Gofer 均位于任务 cgroup |

14 项结果均符合预期，且每项 `cleanup_complete=true`：Python 模块和权限、用户异常、退出码 125/128、超时、输出限额、后台子进程、文件系统隔离、新工作目录、三轮重启、执行中停止恢复和主进程崩溃恢复。用户失败用例返回 `failed` 为预期行为。

证据中的 daemon SHA-256 为 `bf4af9080ce1d3de02fc9421b50348a1a2c2ada2623e0607930215daad04f936`，已核对与工作区构建产物、宿主部署二进制及 INSTALL.json 一致；runsc 校验值和镜像 digest 也与安装 manifest 一致。

只读核对确认部署权限：程序目录和二进制由 root 持有、0755；配置 0600；状态目录 0700；unit 0644。没有读取或输出 API token。

本轮宿主验证范围为 KVM 安装与执行生命周期；systrap、更新、回退、卸载重装仍以先前的 systemd 252 隔离容器验证为依据，没有在宿主重复执行。该记录不包含性能基准或动态 QoS 验收。

官方参考：[systemd 委派](https://systemd.io/CGROUP_DELEGATION/)、[systemd 249 service 语义](https://github.com/systemd/systemd/blob/v249/man/systemd.service.xml)、[gVisor OCI 使用](https://gvisor.dev/docs/user_guide/quick_start/oci/)。

<a id="gateway-validation"></a>
## 附录：Gateway 验证

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

随后完成的真实取消/过载、大输出和 30 对延迟采样，以及解除 inotify 环境限制后的 systemd 配对验收与改进判断，见[有限验收记录](#gateway)。其中已验证行为与未验证项分别列出，原始样本可供复核。

<a id="inventory-validation"></a>
## 附录：动态库存 v2 验证

2026-10-20，v2 当前 diff 的完整 `make check` 和程序构建通过。两个旧反例转为不误扩容的确定性回归；另覆盖原因转换、事件合并与去重、取消/到期、上下界、单调队列、缺测和压力恢复、FIFO、在途回收和失败保留。

仅运行一个真实 systrap 用例，使用上述 tmpfs 测试依赖：正常消费未提高目标；可信包装器暂缓替代创建后，队首确认一次库存阻塞，目标 1→2；放行创建，Python 输出 `dynamic-ok`，稳定后目标 2→1。`inventory_stalled_requests=1`、`scale_ups=1`、`scale_downs=1`、`shrink_succeeded=1`、`shrink_failures=0`，关闭后无 bundle 或任务 cgroup 残留。用例报告 1.13 秒，依赖准备不计入其中。

本轮没有更新宿主服务或开展长时间性能实验，不将功能回归解释为延迟/吞吐收益，也不将 tmpfs 通过解释为历史磁盘问题已修复。

<a id="pool-experiments"></a>
## 附录：本地供给实验方法

```bash
make check
python3 scripts/container.py runtime --platform kvm
python3 scripts/container.py runtime --platform systrap
make build
python3 scripts/container.py systemd --platform kvm --mode warm
python3 scripts/container.py systemd --platform systrap --mode warm
python3 scripts/container.py benchmark --platform kvm --samples 60 --rate 10
```

运行时脚本依次跑 cold/warm 原有执行套件，warm 另测创建过程中关闭、并发独占与新工作目录、排队取消、引擎关闭取消及空闲实例死亡后的回收补池。systemd 入口使用 `--mode cold|warm`，默认 cold。warm 生命周期额外填满 8 个实例，将每次 runsc delete 延迟 2 秒，要求正常停止在 12 秒内完成且无记录残留，直接覆盖串行清理需要至少 16 秒的回归场景。

对比入口在同一个独立容器中顺序运行 Cold/Warm，使用相同 rootfs、runsc、平台和 Hello World 任务。基准默认执行并发 4、总容量 6、创建并发 1，可分别用 `--execution-concurrency`、`--max-sandboxes`、`--create-concurrency` 修改。Warm 固定目标 3，QoS 和动态库存关闭；客户端和真实 HTTP Handler 位于测试进程中，不包含外部网络或 systemd 开销。

`--scenario` 可选择单个场景，默认 all 覆盖前四项；容量扫描必须显式选择：

| 场景 | 到达模式 | 用途 |
|---|---|---|
| inventory | 顺序请求；Warm 每次等待库存补齐 | 隔离观察 READY 命中收益，偏理想化 |
| burst | 32 个请求同时发出 | 容量拒绝与极端突发边界 |
| steady | 固定速率开放式发起 | 持续供给与过载边界 |
| waves | 不均匀波次与真实空闲交替 | 交互式请求延迟和自然补池恢复 |
| capacity | 逐档提高开放式请求率，越界停止 | 给定延迟目标和配额下的可用容量区间 |
| refill-core | 同轨迹波次＋独立 Core 进程，比较两种补池条件 | 观测前台等待、宿主业务干扰与库存恢复；必须显式选择 |

### 波次回放：预热池的主要评价场景

```bash
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py benchmark --platform kvm \
  --scenario waves --wave-cycles 2 --create-concurrency 2
```

正常启动后，双方都进入固定的 2 秒初始空闲期；随后每轮为：1 秒内 5 个请求 → 空闲 10 秒 → 5 秒内 20 个请求 → 空闲 10 秒。默认两轮，每种模式 50 个请求、54 秒回放。各波第一个请求位于波次起点，其余时间由固定种子产生不均匀偏移；完整相对时间轨迹写入 JSON，Cold/Warm 原样重放，不受响应速度影响。

**回放期间不等待 READY、不重启池、不重试拒绝，也不按上一波完成时间推迟下一波。** 单独运行 waves 不增加旧场景的两次未计分请求或库存补齐屏障；正常服务启动探测仍执行，其耗时单列为 startup_ms。默认 all 的旧三场景保留原有预先探测，waves 再按固定初始空闲开始。

重点比较每波的首请求、计划到达至响应的 P50/P95、完成/拒绝/错误、以及到达窗口结束后恢复库存的时间。恢复必须等本波全部响应结束，且没有排队、执行或创建；Warm 需补回固定目标，Cold 需没有实例。下一波已到仍未恢复记为 null，不从后续空闲期补一个好看的值。每波只有 5 或 20 个请求，分位数只是本次描述，不能解释成稳定生产尾延迟。

每 50ms 记录库存及资源：沙箱父组 CPU，以及可见 cgroup 命名空间根的 CPU 增量和时间加权内存，包含全部空闲间隔。隔离 Docker 中根计数还包括客户端、运行时 CLI、init 和共享依赖，不是纯 Python 成本；启动、依赖准备与最终停服库存销毁不在这段回放资源窗口内。缺测或计数回退保留无效状态，不按零占用处理。

**Warm 的目标是利用空闲时间提前准备、降低下一波等待，接受有界的备用资源成本；持续吞吐不是本场景的胜负指标。** 固定机器容量仍需要 steady 测边界，波次不能替代过载验证。当前场景没有同机核心业务负载，不能据此宣称预热不干扰业务。

### 容量扫描：寻找可用边界

```bash
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py benchmark --platform kvm --scenario capacity \
  --execution-concurrency 8 --create-concurrency 4 --max-sandboxes 8 \
  --capacity-rates 4,8,12,16 --capacity-samples 200 \
  --latency-slo-ms 1000 --on-time-target .95
```

此处 1 秒／95% 是可修改的实验目标，不是产品承诺。每种模式从低档开始，每档固定开放式发送，不随响应变慢而自动降速；共同空闲 2 秒后发起，结束排空并保留 3 秒恢复期。每档 100～1000 请求，率为 1～64 req/s 的最多六个递增整数。先报告最后达标档和首个不达标档，全部达标则只报告已测下界，不外推精确极限。

达标同时要求：所有已发起请求中至少目标比例成功、确认清理且从**计划到达**至响应不超过期限，以及成功请求同口径 P95 不超期。快速 503 不算有效交付。单列窗口内完成率、拒绝、生产端调度滞后、成功排队 P95；100ms 采样执行/创建/排队峰值与满载占比、父组和外层节流、父组 CPU PSI、CPU/内存成本。阶段末排空完成和恢复期不计入到达窗口交付率。

任一 SLO 不达标停止提高该模式负载；内存超过父组或外层预算 85%、新 OOM、执行器不健康、缺测或执行错误立即停止继续发起，并保留已发起请求结果。生产端明显滞后单列为测试无效，不能算服务容量。原始数据为 `capacity.json`、`requests.csv`、`capacity-<mode>-<rate>-pool.jsonl`。仅允许经私有 cgroup 的一次性 Docker 入口运行，外层仍为 6 CPU/5GiB，父组仍默认 4 CPU/4GiB；本场景 `/tmp` 为有界 tmpfs，测量的是受限 CPU/创建链路，不是普通磁盘能力。

范围缩小后，先在边界两侧细分请求率，再用每档 500～1000 请求和重复批次确认波动。寻找并发参数时，每次只改变一个：先固定创建并发测试执行并发 1/2/4/8，再固定执行并发测试创建并发 1/2/4；不要同时放大配额、队列和并发。当前 8/4 是允许探索的上限，不是推荐最优值；配额节流、创建满载和排队的证据用于决定下一步改哪一项。Hello World 的边界也不能替代 CPU 重任务、内存或磁盘场景。

为区分执行槽与父组配额，可在相同命令中指定 `--capacity-parent-cpu 2|3|4`，通过既有比较、写入、读回接口仅收紧父组；不修改服务默认配额或 QoS 上界。`--sandbox-cpu-milli` 设置基准的单实例模板，默认仍 1000。做父组或执行并发对照时固定它，避免同时改变两个额度。两项参数、实际父组/外层限制和二进制哈希都写入摘要。

### 补池与 Core 争用对照

```bash
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py benchmark --platform kvm --scenario refill-core \
  --wave-cycles 2 --repetitions 2 --execution-concurrency 4 \
  --create-concurrency 4 --max-sandboxes 8
```

显式场景启动真实 mockcore 独立进程，将 Core 与 Sentry/Gofer 放入共享 2 CPU 子树；沙箱父组固定 2 CPU，单实例默认 1 CPU。压测端与 runsc 控制 CLI 在竞争组之外，整个容器仍受 6 CPU／5GiB 限制；这不等同于裸机部署的资源拓扑。Core 自身 256MiB／64 host tasks，固定每请求 50,000 次 SHA-256、服务并发 4；60 req/s 开放式发起，客户端最多 32 个在途，无界排队改记为显式丢弃。

每轮使用相同波次轨迹，依次运行 Core-only 基线、Cold、Warm-slots、Warm-idle；第二轮反转顺序。各次正常引擎启动探测在窗口之外，初始空闲 2 秒、每波后的空闲 10 秒都按计划保留，不等库存或重试请求。基线保留空闲 Cold 引擎与相同采样，仅不发送沙箱任务；同一 Core 进程持续服务，避免每种模式各自重新校准工作量。`--repetitions` 在本场景限 1～3；默认 all 不附带此对照。

Core 报告全部请求的按期成功率（示例期限 50ms）、成功 P50/P99、拒绝、错误、丢弃和发起滞后；分开统计沙箱到达窗口与其余初始空闲／恢复窗口。客户端明显滞后、缺测、沙箱错误或回收失败使比较无效；Core 基线要求 99% 按期成功且成功 P99 ≤50ms，失败则停止后续比较，不自动改参。该目标用于实验检查，不是生产承诺。

每个 profile 子目录保存 Core 与沙箱原始 CSV、50ms 库存/资源 JSONL；后者增加 Core 与共享组的 CPU/PSI、共享节流及补池计数。资源采样覆盖波次及两路请求排空，最终停服和库存销毁单列在清理结果，不混入用户响应。`refill-core.json` 保存逐轮配置、顺序、哈希、全窗口及分窗口统计、后台创建预留计数与最终回收确认。只改变 slots/idle 的配对才用于判断补池条件；Cold 与 Warm 的差异包含预热和补池两部分。结果仍需结合任务类型、配额和波动解释。

### 数据与统计口径

保护方案使用同一入口加 `--refill-comparison protection`，两轮比较：Core-only 基线、idle＋2 CPU、idle＋1 CPU、PSI 仅补池＋2 CPU、PSI 仅补池＋1 CPU、AIMD 1～2 CPU＋完整压力调度。所有组均采集同一套 QoS 数据，静态组只 observe；PSI 组关闭 idle，只控制补池，组合组额外改变 CPU 与准入，作为整体策略单列。库存目标仍固定 3，不宣称所有参数都动态。

保护判定预先固定为：Core 全窗口、沙箱到达窗口、其余恢复窗口，分别至少 99% 请求在 50ms 内成功返回，且成功 P99 ≤50ms；同时保留拒绝和沙箱代价。`CoreTargetPassed` 按三个窗口共同计算，不能只用全窗口均值/分位数掩盖波次失败。`ParentCPUStart/Final`、`QoSFinal`、新鲜 protected 采样数与暂停采样数记录动作是否真实发生；JSONL 保留压力状态、原因、Core CPU PSI、CPU 实际额度和变更次数。恢复阈值及 CPU 参数见[QoS](../runtime.md#qos)，结果见[验收](local-validation.md)。

结果写入新的 `artifacts/benchmark-<platform>-<随机后缀>/`：

- requests.csv：保留所有请求，新增 wave、计划到达至响应、实际完成时刻和拒绝原因；原 wall 仍从实际发出请求计时，计划口径另包含客户端调度滞后。
- summary.json：配置、运行时/测试二进制哈希、各场景统计；wave_trace 保存相同到达轨迹，wave_runs 保存逐波结果及整个回放窗口资源。基准中的每秒完成数包含空闲时间，不能当作机器的持续容量。
- cold-pool.jsonl / warm-pool.jsonl：仅 waves 写出，保存 50ms 原始状态及资源样本；旧三场景继续使用原有 1ms 库存峰值采样。

`--samples` 只控制 inventory/steady 的请求数，`--rate` 只控制 steady；波次使用固定的 5/20 请求和 `--wave-cycles`（1～5，默认 2）。成功分位数仅统计成功且确认清理的响应，拒绝和错误单列；执行器不健康、采样无效不会伪装成普通容量拒绝。

持续接单能力同时受执行和创建约束。设执行并发 C、领取至清理完成平均时间 S、共享创建并发 B、平均创建时间 Tc，则稳定接单率需同时满足 λS < C 和 λTc < B；这些是容量约束，不是负载分布、CPU 争抢和资源回收条件下的精确吞吐预测。有限库存只能吸收短时突发。

容器基准包含外层 CPU/内存限制，并使用容器 PID/cgroup 命名空间；不能把其数值直接当作裸机 systemd 服务性能。需要裸机数据时，可在专门委派的独立测试环境中设置 AGENTBOX_BENCHMARK、AGENTBOX_BENCH_OUTPUT、AGENTBOX_RUNSC、AGENTBOX_ROOTFS、AGENTBOX_CGROUP_ROOT、AGENTBOX_PLATFORM 等变量后运行 `go test -tags=integration -run TestColdWarmHTTPBenchmark -count=1 -v ./tests/benchmarks`。不能与正在运行的服务共用委派子树或状态目录。

systemd mixed 的终止顺序依据 [systemd 249 官方说明](https://github.com/systemd/systemd/blob/v249/man/systemd.kill.xml)。真实服务测试要求正常停止结果为 success 且无执行记录残留；主进程崩溃时仍验证整个服务进程集合被回收。测试容器将 `/opt` 放在可执行 tmpfs 内，安装、更新和卸载仍执行完整文件复制和校验。

实际功能验收与冷暖数据见 [本阶段验收记录](#warm)。

本基准固定关闭动态策略，优先评估给定到达模式下的预热体验；动态控制与其他能力见各自文档。

<a id="static-method"></a>
## 附录：静态 CPU 实验方法

### 三组实验


| 组别 | 执行环境 | 沙箱任务 | 沙箱父级 `cpu.max` |
| --- | --- | --- | --- |
| `core-baseline` | cold daemon＋观察器，无运行时库存 | 无 | `80000 20000`，空子树 |
| `default-quota` | warm，目标库存 3、最大实例 6、执行并发 2 | 两个客户端持续提交固定计算任务 | `80000 20000`，4 CPU 等效上限 |
| `protected-quota` | 与默认配额组相同 | 相同任务和客户端数 | `20000 20000`，1 CPU 等效上限 |

三组都位于同一个外层 2 CPU 带宽、5GiB 内存、2304 个任务上限的独立容器。默认沙箱上限高于外层可用带宽，表示保留项目当前静态配置，并不意味着可以使用四个完整 CPU。protected 组只改变沙箱父级 quota，周期保持 20ms；没有 CPU 绑定或独占保证。

选择 2 CPU 外层预算，是为了让两个 CPU 密集沙箱与 Core 在可控条件下产生预算竞争。实验验证的是这个场景中的权衡，不能据此认定本机裸机默认 4 CPU 沙箱上限必然损害核心服务。

运行前将同一份可信 runsc、rootfs 和程序复制到容器内 1GiB tmpfs，状态及原始记录也放在那里；rootfs 在沙箱中仍只读。组件全部停止后才导出记录到宿主结果目录，减少宿主磁盘压力对本轮 CPU 实验的影响。tmpfs 计入外层 5GiB 内存预算。这个实验不测磁盘持久化成本或冷缓存启动延迟，准备复制与导出耗时不计入测量。

运行时可能使用的宿主侧 `/tmp` 也挂载为容器内独立 256MiB tmpfs；它与 OCI 配置中的沙箱 `/tmp` 是两个不同的挂载。

核心服务放在独立 cgroup：内存上限 256MiB、无 swap、任务上限 64；其 CPU 没有单独配额，但仍受外层限制。沙箱各项内存、进程数和单实例 CPU 限制保留。daemon manager、runsc CLI 和压测客户端不属于沙箱父级，也会消耗外层预算，所以限制沙箱到 1 CPU 不能被解释为给核心服务保留了一个独占核。

基线包含空闲 cold daemon 与观察器，避免采集路径发生变化；它不是核心服务完全独占机器的结果。默认组与 protected 组之间的比较才用于评估降低 quota 的效果。

### 固定工作量与时间窗口

Mock Core 每次请求执行 50,000 次 Go SHA-256 链计算，最多四个并发；以每秒 20 次的计划时间发送请求。沙箱每个任务通过纯 Python 执行 8,000,000 次固定整数运算 `s = (1664525*s + 1013904223) & 0xffffffff`，初值为 1，输出确定的校验值 `cd496201`，任务超时为 20 秒。负载不导入额外计算库。记录器检查完成状态、校验值与清理结果。这样配额收紧造成的延迟与完成率变化可以被测量，超时不算成功。

每组先等待观察器有效、warm 库存充足，执行 20 个核心服务预热请求；负载组额外并发执行两个沙箱预热任务，等待库存补齐。预热结果不计入测量。

正式时间轴依次为：五秒核心服务请求、配置时长的比较窗口、五秒核心服务请求。负载组只在比较窗口内提交沙箱任务，结束后等待在途请求收尾，不强行取消。基线同样保留三个时间段，比较窗口内没有沙箱任务。每组结束停止 daemon 并检查任务目录和子 cgroup 已清理，再进入下一组。

默认每个比较窗口 60 秒，重复三轮，三轮执行顺序分别为：

```text
baseline → default → protected
default → protected → baseline
protected → baseline → default
```

这能分散固定顺序带来的偏差，但不能消除温度、频率、缓存和宿主其他进程的影响。重复超过三轮时继续轮换；少于三轮属于流程检查，未覆盖三个顺序位置。

### 运行与绘图

项目本地已准备 runsc、rootfs、Docker 和 KVM 后，安装独立绘图环境：

```bash
python3 -m venv .local/plot-venv
.local/plot-venv/bin/python -m pip install -r scripts/plot-requirements.txt
make build
python3 scripts/container.py static --platform kvm --duration 60 --repetitions 3
```

也可以执行 `make static-experiment` 使用默认参数。没有 KVM 的环境显式选择 `systrap`；测试不会自动更换平台。参数分别为 --platform、--duration（5～300 秒）、--repetitions（1～9）。`--platform kvm --duration 5 --repetitions 1` 只用于快速检查流程。已有 Matplotlib 环境可通过 `AGENTBOX_PLOT_PYTHON` 指定 Python 可执行文件。

实验入口只使用 Docker 的私有 PID、cgroup 和网络命名空间，项目只读挂载，只有本次宿主结果目录可写；不挂载宿主 cgroup 或 Docker socket 到容器。privileged 的用途与现有集成验收一致。正常或 Python 异常退出时，停止组件后导出已写记录，再删除容器；只有整轮通过检查才生成 `experiment.json` 并自动绘图。外部 SIGKILL 或容器 OOM 可能丢失尚未导出的内存记录。

容器使用 Docker `--init` 持续回收运行时产生的孤儿子进程；init 与测试控制器都先移入管理叶子，再启用根部控制器。Python 控制器负责等待其直接启动的服务进程，不承担 PID 1 的收割职责。

结果保存在新的 `artifacts/static-<platform>-*/`：

- `plan.json`：执行顺序、工作负载、环境、依赖版本、二进制与测量脚本哈希。
- `rN-<group>-core.csv`、`*-sandboxes.csv`、`*-observations.jsonl`：原始请求与完整状态快照。
- `rN-<group>-summary.json`：单组汇总、资源限制前后核对与数据质量计数。
- `experiment.json`：全部成功完成的实验组及退出清理检查。
- `figures/comparison.png` 和 `.svg`：组间分布、逐轮 P99、固定任务完成率。
- `figures/rN-<group>-timeline.png` 和 `.svg`：每组延迟、PSI、CPU 用量／上限、节流周期比例。
- `figures/comparison.csv`：逐轮比较窗口指标、错误／拒绝／丢弃和观测质量。
- `figures/plot-metadata.json`：绘图库版本和绘图脚本哈希；各组件日志同目录保留。

测试使用包装器启用 runsc `delete` 的调试日志，保存为 `runsc-debug.log`，写入同一内存暂存目录并在退出后导出。三组使用相同设置；它属于实验诊断配置，未加入生产 daemon。异常任务响应也以有大小上限的内容保存在原始 CSV 的 error 字段。

可对已有结果单独重画，绘图脚本不连接服务或修改资源限制：

```bash
.local/plot-venv/bin/python scripts/experiment.py plot artifacts/static-kvm-实际目录
```

### 统计与图表口径

核心服务同时记录实际发送后的耗时 `wall_ms`，以及从计划发送到响应完成的 `scheduled_wall_ms`。图表和主要对照采用后者，把客户端发送滞后包含进来；另报告 `send_lag_p99_ms` 用于判断负载生成器是否跟上。客户端队列加运行总数上限为 32，超过时明确记录丢弃，不默默降低目标负载。

Core 分位数只纳入比较窗口内计划发送、返回成功且内容有效的请求；拒绝、丢弃和错误单独记录。CDF 为各轮成功请求合并后的经验分布；P99 使用逐轮 nearest-rank，图中的短横线是逐轮 P99 的中位数，不是合并样本 P99。时间图画逐请求延迟，不生成样本不足的秒级 P99。

沙箱完成率为“比较窗口内响应且成功并完成清理的任务数 / 比较窗口秒数”。结束后收尾完成的任务保留原始记录，但不计入该完成率。任务耗时统计包含已提交任务的完整收尾结果；因此它与只统计窗口内完成数的口径不同。任务 CPU 用量从实例创建累计到销毁前采样，包含预热和运行时创建，可能漏掉采样后的后台任务和清理消耗；它既不是完整生命周期成本，也不是纯 Python 计算 CPU 时间。超时或取消的 kill 完成情况不在该次采样前等待确认；资源是否回收另看 `cleanup_complete`。

CPU PSI 采用相邻累计值与实际时间间隔的比例。节流图为 `Δnr_throttled / Δnr_periods`，窗口没有活跃配额周期时留空。观测不可用、过期或尚未建立窗口时，曲线断开，不补零。浅色阴影表示比较窗口；结束标记表示停止提交，最后的在途任务可能继续执行。

默认组的沙箱父级上限为 4 CPU，其自身节流统计可能很低，但外层 2 CPU 配额仍会影响任务等待；不能只根据一条节流曲线判断是否发生争用，需要同时看 PSI、配额层级与 Core 延迟。

### 如何判断结果

先确认请求错误、拒绝、客户端丢弃与观测有效性，再比较各轮 Core 延迟与沙箱完成率。静态保护可能降低核心服务延迟，也可能因实验负载不足、外层调度或宿主噪声而没有清晰收益；所有轮次都应保留，不能只展示最好的一轮。

绘图前还会检查整组原始状态记录中的执行器 ready。如果执行器因基础设施故障停止接单，脚本拒绝为该批次生成对照图，避免将负载意外停止后的 Core 恢复当成配额保护收益；失败记录仍保留用于诊断。

这一阶段衡量的是固定预算下的权衡，尚未证明动态策略比静态配置更好。宿主 `/proc/pressure` 是系统背景数据，容器局部压力使用各自 cgroup 文件；容器结果不能直接当作裸机容量和延迟承诺。


<a id="observation-analysis"></a>
## 附录：历史观测分析

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

历史九组数据匹配到 177 个正式任务、无缺失事件。默认组 Core 发送滞后 P99 为 44.51/54.83/12.31ms，服务端 work P99 为 71.39/76.38/77.35ms，说明客户端等待也需单独观察。[分析 JSON](../data/observability-analysis.json)保留逐轮数字；旧数据缺少新增 manager、祖先、I/O、PID 等字段，不补写新指标。

![同批请求的时间口径](../assets/observability-core-layers.png)

![正式任务阶段](../assets/observability-task-stages.png)

![第一轮默认组的供给时间线](../assets/observability-pool-supply.png)

ECDF 只是经验分布，不是置信区间。新增采集净开销、长期告警和持久追踪未独立验证；普通磁盘 KVM 观测短测的失败仍作为已知问题保留。内核定义参见 [PSI](https://docs.kernel.org/6.8/accounting/psi.html)和 [cgroups v2](https://docs.kernel.org/6.8/admin-guide/cgroup-v2.html)。
