# 从业务延迟追到资源和供给

这一阶段根据[性能理论](performance-theory.md)补齐观测链路，先回答“慢在哪里、谁在消耗、哪种容量不够”，再讨论动态策略。它增加状态、事件和离线分析，不改变资源配额、任务调度策略、运行时超时预算或服务默认配置。

检查结果、只读真实内核采集及 KVM 短测未通过的记录见[验收记录](observability-validation.md)。

本页描述只读观测层。后续 CPU QoS 初版已提供预算预演与显式 enforce，实际控制配置和功能验收见 [CPU QoS](cpu-qos.md)。

按业务结果、时间、供给、资源、生命周期和对照验证组织的六阶段诊断流程见[观测系统总览](observability-system.md)。

## 1. 资源范围与采样边界

`-qos=observe` 的 `GET /v1/status` 保留原有来源，并补充管理进程和沙箱子树的可见祖先：

| 来源 | 范围 | 需要解释的问题 |
| --- | --- | --- |
| `host` | `/proc/pressure` | 系统背景中的 CPU、内存和 I/O 等待 |
| `sandboxes` | 沙箱父级 cgroup | 整批沙箱的消耗、等待和节流 |
| `protected` | 显式配置的核心服务 cgroup | 被保护业务是否发生资源等待 |
| `manager` | daemon 当前实际所在的 cgroup | daemon、它启动的控制命令等管理侧成本 |
| `ancestor_0` 等 | 从沙箱父级的直接父级向上，最多八级 | 是否存在更上层的共享预算与节流 |

manager 根据进程实际归属发现，不假设它一定在某个名为 `manager` 的目录中；该 cgroup 也可能有其他进程。祖先统计包含后代，某些部署中还会和 manager 指向同一路径，**不能把这些来源的用量相加**。比较时同时记录 `name` 和 `path`。

`qos.hierarchy_boundary` 和 `hierarchy_boundary_reason` 记录发现的终点。真实内核根没有 `cpu.max` 时不将其作为完整 cgroup 来源；私有 cgroup 命名空间的根若暴露 `cpu.max` 则采集。容器外不可见的祖先不在报告中，不能据此声称观察了整个宿主层级。系统级来源仍只采 PSI，没有采 `/proc/stat` CPU 利用率。

CPU/内存沿用原采样周期，CPU 增加 `user_usec`、`system_usec` 的原始值及核等效差分。`qos.sequence` 在进程内每完成一轮采集递增；重启归零。HTTP 查询只取缓存，多次查询可能取得同一个 sequence。分析来源时还需使用该来源的 `sampled_at`，不能把查询帧数当成独立资源样本数。

## 2. I/O 与 PID 使用独立的慢速采集

每个来源的 `sample.auxiliary` 有自己的 `sampled_at`、`age_ms`、`read_ms` 和 `stale`。当前按一秒间隔刷新，与 CPU/内存的高频采样分开：

| 指标族 | 内容 | 解释限制 |
| --- | --- | --- |
| `io_pressure` | 原始 PSI 与实际间隔上的 some/full 差分 | 系统级或 cgroup 级等待，不是某个文件的延迟 |
| `io_devices` | `io.stat` 每设备累计读写字节、I/O 次数 | 不提供设备 await；空设备列表与文件缺失不同 |
| `pids` | current、max、累计限额命中及命中增量 | 宿主 cgroup 任务数，不等于沙箱内 Python 的进程数 |

各族独立报告 `available/error`。缺少 I/O 控制器、权限或统计文件时，该族缺测，不会让正常的 CPU/内存采样失效，也不会改变 readiness；不为采集额外启用或委派 I/O 控制器。文件仍有 64KiB 上限，设备最多 128 个。

首次、目录身份变化、计数回退、过长采样间隔时重新建立差分基线。辅助数据年龄超过三秒或主快照过期时不暴露有效速率；原始计数仍带时间戳保留。分析辅助数据必须按其自己的 `sampled_at` 去重，不能每查询一次就累加同一秒的事件增量。有效的零是数字 `0`，不可用的采样或差分是 `null`，不能补零。

## 3. 把库存、创建产能和执行名额分开看

warm 模式的状态增加 `scheduling`，同时保留原有池字段。

- `execution_in_flight/execution_limit`：领取后尚未完成收尾的执行名额，包括清理阶段。
- `creation_limit`、`creation_attempts`：创建并发上限和已预留的创建尝试数；进行中的数量仍看 `creating`。
- `creation_succeeded/failed/aborted`：按结果分组的创建耗时。succeeded 表示已经收入 READY；aborted 包括停止后返回，或创建成功但池已失效而不能收入库存。
- 队列数量、字节上限、等待预算和 `oldest_queue_ms`：当前积压与等待边界。
- `queue_entered` 及 `queue_assigned/canceled/timed_out/unavailable`：所有实际入队者的驻留时间，包含未获得实例的请求；立即领取不算入队。
- `queue_count_full/queue_bytes_full`：入队前因数量或文本预算拒绝的次数，不混入排队耗时。

每种耗时报告 `count/total_ms/max_ms/buckets`。均值只可用**同一组** total/count 计算；旧的 `creation_total_ms` 与 `creation_count` 口径不同，不应相除。直方图为非累计桶，上界依次为 1、5、10、25、50、100、250、500、1000、5000、10000ms；最后 `upper_ms=null` 表示溢出桶，不提供精确分位数。

同一份池快照应满足：

```text
creation_attempts = succeeded.count + failed.count + aborted.count + creating
queue_entered = assigned.count + canceled.count + timed_out.count + unavailable.count + pressure_rejected.count + queued
```

这些计数在进程生命周期内累计，重启归零。创建 worker 的尝试耗时不含两次失败之间的退避，因此不能用 `并发 / 单次均值` 直接宣称持续供给能力。成功创建计数的窗口差分也只是该窗口的观测创建率，不是最大容量；窗口两端的在途实例、初始库存和 drain 会让它与窗口完成率不同。

## 4. 拒绝和清理也属于结果

503 响应在原有 `error` 外增加 `reason`，包括 `queue_count_full`、`queue_bytes_full`、`queue_timeout`、`execution_slots_full`、`executor_unhealthy`；兼容没有分类信息的执行器时使用 `capacity_exhausted`。只有实际入队的拒绝才有 `queue_ms`，请求关联使用 `request_id`。不改变已有错误映射，也不提供自动重试保证。记录器把拒绝原因与各阶段耗时保存到沙箱 CSV。

清理过程拆出五个**观测阶段**，并保留原操作顺序与独立清理上下文：

```text
runtime_delete → cgroup_kill → cgroup_empty_and_remove
               → output_readers → bundle_remove
```

任务响应的 `cleanup_phases` 包含实际执行的阶段、sandbox_id、起止时间、耗时和错误。`cgroup_empty_and_remove` 当前合并等待变空和删除，不是更细的内核等待归因。失败时可能只有部分阶段：例如外部资源未清完就保留 bundle/intent。

状态中的 `cleanup.active` 在操作开始时即出现，可以在删除卡住时看出正在等待哪个阶段；`cleanup.recent` 只保留最近 64 个已结束阶段，`dropped` 记录历史淘汰数量。空闲库存同样用 sandbox_id 记录，不构造假的任务结果。事件缓存位于内存，进程退出后不保留；尚未自动抓取线程栈、FD 或内核等待栈。启动初始化失败、HTTP 尚未可用时，仍需依赖组件日志等诊断。

## 5. 先分析已有实验，不重新制造数据

已有三轮静态 CPU 实验可以离线补出三种 Core 时间口径、客户端发送滞后、正式任务阶段分布和库存时间线：

```bash
.local/plot-venv/bin/python scripts/analyze-observations.py \
  artifacts/static-kvm-XCmQHtIX \
  --output artifacts/observability-analysis-new
```

没有绘图依赖时可用 `python3` 加 `--no-plots`。输出目录必须不存在，源数据只读；分析 JSON 记录输入文件 SHA-256。脚本不启动任何负载，不修改原实验结果。

Core 的三种分布使用同一窗口内同一批成功请求；同时报告 offered、成功、拒绝、客户端丢弃和其他错误。沙箱阶段只匹配正式 CSV 中成功且清理完成的 execution_id，排除 warmup 日志；drain 中完成的正式请求仍可用于阶段分析，但不能计入固定窗口吞吐。分位数采用 nearest-rank，不用各阶段 P99 相加或相减归因。故障/未就绪批次保留诊断时间线，不生成有效对照分布。

此次从 2026-10-17 的既有九组记录匹配到 **177 个正式任务，未匹配数为零**。默认组的 Core 发送滞后 P99 为 44.51、54.83、12.31ms，服务端 work P99 为 71.39、76.38、77.35ms；说明需要同时观察客户端与服务端，不能将所有变化归于业务处理。完整逐轮数据见 [分析 JSON](data/observability-analysis.json) 与 [CSV](data/observability-analysis.csv)。

![同一批 Core 请求的三种时间口径](assets/observability-core-layers.png)

![排除预热请求后的任务阶段分布](assets/observability-task-stages.png)

![默认配额第一轮的 Core 与池供给时间线](assets/observability-pool-supply.png)

图中的合并 ECDF 只展示这批请求的经验分布，不是重复实验的置信区间。第一轮默认组有 685 个状态查询帧，但只有 684 个不同的沙箱资源采样时间。历史记录只含 host/sandboxes/protected，没有本轮新增 manager、祖先、I/O、PID 和分类直方图；分析结果明确保留这些缺测，不能把新指标补写进旧实验。

这些图解释的是旧版本的实验。本轮新增采集逻辑的功能验收与性能结论分开记录；没有重跑完整性能实验，也没有测量新增采集的净开销。后续再安排独立生成器、固定其余条件的 quota 扫描与动态控制对照。本轮不实现 AIMD、压力准入、动态补池，也不改默认资源限制。
