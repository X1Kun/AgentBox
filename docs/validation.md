# 验收与性能证据

最近更新：2026-10-21。本页统一保存执行、预热池、QoS、Gateway、MCP 的结果及适用范围；配置和复现入口分别见各功能说明。历史批次不自动代表当前版本重新完成性能测试。

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

本地原始结果归档到 `artifacts/control-refinement-20261021.tar.gz`；摘要追加到 [既有证据 JSON](data/performance-theory-evidence.json) 的 `sequential_refinement_20261021`。磁盘慢 I/O 尚未根治，本轮没有修改宿主部署或运行新的参数搜索。

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

摘要、配置与失败快照合并在 [既有理论证据 JSON](data/performance-theory-evidence.json) 的 `implementation_review_20261021` 中；本地原始 CSV/JSONL/日志统一归档为 `artifacts/implementation-validation-20261021.tar.gz`（Git 忽略）。

<a id="runtime"></a>
## 执行隔离与生命周期

- 2026-10-12～16，KVM/systrap 的 cold/warm 真实执行用例通过，覆盖 stdin/stdout、UID 1000、环境允许列表、Python main/pickle、代码错误、用户退出码 125/128、超时、输出洪水、只读根、禁网、工作目录隔离、后台进程销毁、OOM、取消、容量拒绝和重启回收。
- 输入准备在 umask 0022/0077 下保留镜像权限；根及挂载点显式 0755，任务输入只读。fork 测试降低 guest rlimit 后验证拦截，不声称实测精确的 64 进程边界；OOM 专门区分软水位与硬限制。
- 每任务全新实例，待命 init 保持 Sentry 存活，使用附着 runsc exec 获取短任务退出码；清理失败保留 intent 并停止接单。
- create/start 已开始时在独立预算内完成，避免取消导致半写元数据。空闲库存四路回收，清理确认前仍占容量。
- systemd 252 容器的 KVM cold/warm、systrap warm 验收包括执行中停止、主进程 SIGKILL 后恢复、更新/回退/卸载重装和 purge。注入每实例删除延迟 2 秒，8 实例停止约 4.1～4.4 秒；不是生产停止时间上界。
- 2026-10-15 宿主 systemd 249 的 KVM cold 服务另有安装与生命周期记录，见 [部署说明](systemd-deployment.md)。后续 Gateway/MCP 只在隔离容器验收，没有替换该宿主服务。

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

2026-10-17，Linux 6.8.0-138-generic、KVM、外层 2 CPU/5GiB，九组各有一分钟比较窗口。配置与统计方法见 [实验说明](static-cpu-experiment.md)。

| 组别 | 三轮 Core P99（ms） | 三轮沙箱完成率（次/秒） |
| --- | --- | --- |
| 无沙箱基线 | 19.64 / 21.44 / 17.15 | 0 / 0 / 0 |
| 默认 4 CPU 父级配额 | 94.52 / 81.11 / 80.64 | 0.550 / 0.533 / 0.567 |
| 静态 1 CPU 父级配额 | 21.77 / 23.77 / 22.88 | 0.367 / 0.367 / 0.367 |

共 12,600 个 Core 请求成功，177 个正式沙箱任务成功且清理（默认 105、protected 72）；6,225 帧状态全部可用，无拒绝、客户端丢弃、传输、基础设施或采样错误。窗口外 drain 完成不计窗口吞吐。

降低静态配额使 Core P99 接近基线，同时降低沙箱完成率。此结论限于外层 2 CPU、固定工作量和旧版本，不外推裸机或动态控制收益。[实验 JSON](data/static-cpu-experiment.json)记录版本、哈希、逐轮配置和结果；[离线分析](data/observability-analysis.json)关联到全部 177 个任务。依赖快照准备约 508 秒，在测量窗口之外，不能算入 API 时延。

早期中止批次出现 delete 超时，没有对应 OOM 增量；Docker init 和宿主侧 tmpfs 修正后正式批次未复现。缺少现场内核栈，未确定旧失败的精确根因。

![正式三轮静态对照](assets/static-cpu-comparison.png)

<a id="gateway"></a>
## Gateway 验收与开销

2026-10-20，普通用户 UID 1000 Gateway，经 Unix HTTP 调用 root Engine 和真实 systrap runsc。

- 入口上限 1 时多余请求约 0.764ms 返回 gateway_capacity；同次 readyz/status 均可访问。
- 客户端取消、Gateway SIGTERM 后，确认清理并补足库存的单次观测约 50.194ms/13.214ms；任务资源删除、容量恢复。不是取消延迟分位数。
- 完整 1MiB NUL 输出转义后约 6MiB JSON，未截断；单请求进程 RSS 高水位约 20.84MiB。不能证明 64 个并发大响应适配默认 512MiB unit，默认仍为 8。
- 两路交替、每路预热 3 次后采样 30 次，均成功且清理：Unix 直连 wall 中位数 170.859ms/P90 200.545ms，Gateway 为 177.594ms/195.549ms。
- 对每个请求计算客户端 wall 减 Engine total_ms，再逐对相减，中位数约 0.257ms。这是轻载下入口路径开销估计，包含客户端与编码，不能当纯 Gateway CPU 时间；端到端中位数之差不能全部归因于 Gateway。

[完整 60 个样本与摘要](data/gateway-validation.json)保留原始精度。条件为 warm、库存 2、执行并发 1、总容量 4、systrap、tmpfs 依赖；不报告 P99、吞吐、KVM 或普通磁盘收益。

隔离 systemd 252/systrap 配对生命周期通过：专用普通 UID 999、root:专用组 0750 目录/0660 socket，其他用户不能连接，Gateway 不能读取 root 凭据文件；启动依赖、daemon 重启后恢复、停止传播、Gateway 更新、移除恢复独立模式、保留凭据重新配对和 purge 均通过。共检查 5 次配对与 1 次独立执行。

首次 systemd PID 1 因 inotify_init 返回 EMFILE 退出；root 诊断只有 4 个 FD、RLIMIT_NOFILE 为 1,048,576，当时实例限额为 128。用户临时提高为 1024 后同一套件通过，未确定占用最多的进程，没有声称发生泄漏。宿主配对服务没有切换。

<a id="mcp"></a>
## MCP 与真实 DeepSeek

实际普通用户 MCP/Gateway stdio 链路通过成功、代码异常、超时、RPC 取消与直接关闭传输的回收；模拟 API 的 3 次模型请求、2 次真实执行完成除零错误后修正为 42。SDK 优雅 Close 会等在途调用，不能当作实际断线；进程生命周期另显式取消 HTTP 转发。

配置私有凭据后，真实 deepseek-flash API 同样以 3 次模型请求、2 次真实 runsc 执行完成 failed/ZeroDivisionError → succeeded/42，两次 cleanup_complete 均确认。Agent/MCP/Gateway 使用实际编译二进制，API key 不进入 MCP 或任务环境，未出现在输出与待提交文件中。[脱敏证据](data/deepseek-live-validation.json)记录实际计数、最终回答和日志哈希；详细用法见 [MCP](mcp-deepseek.md)。

真实 API 用例必须显式选择，普通 make check 不调用付费模型。当前无跨宿主机或多 Worker 验收，下一步为独立 kind 功能模拟。

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
