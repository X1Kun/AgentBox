# 一次性预热池与本地调度

AgentBox 支持显式 `cold` 和 `warm` 两种模式，默认保持 `cold`，不自动降级或切换平台。两种模式共用 FIFO 准入、创建并发与容量预算，以及 Python 引导、隔离、结果判定和销毁规则。Cold 不维持 READY 库存；Warm 额外后台补池。

## 预热边界

后台提前创建 bundle、输入目录、cgroup 和 OCI 配置，运行 `runsc create/start`，使 Sentry、Gofer 和等待中的 init 存活。领取后才写入 task.json/stdin，通过附着式 `runsc exec` 启动本次 Python 任务。

这版没有提前加载任务 Python 解释器。等待中的 init 是独立 Python 进程，用户代码仍在新的 Python 进程中执行；解释器启动、exec 控制调用、健康检查和收尾成本仍在请求路径中。

```text
后台创建 → READY → LEASED → RUNNING → DESTROYING → 删除记录
                      ↘ 准备失败/取消 ↗
```

实例只能被领取一次，完成或失败后销毁整个运行环境，不能从 RUNNING 返回 READY。即使任务尚未启动，领取后的取消也需要销毁。不同任务不共享可写工作目录。

## 文件与会话准备

`prepareSandboxFiles` 创建不含用户数据的目录、config.json 和 process.json；`prepareInput` 在领取后写入代码、env 和 stdin。session 在任务 start 时才打开 stdin，预热阶段不持有任务输入。

输入目录为只读 bind mount，宿主之后写入的文件通过 gVisor 的共享文件访问语义可见；不能把该目录设为 exclusive 文件访问缓存。目录与 task.json 的 guest 访问权限仍显式 chmod，独立于 systemd 的 `UMask=0077`。stdin 仅通过宿主打开的描述符传递。

## 配置

| CLI | systemd 环境变量 | 默认值 | 含义 |
|---|---|---|---|
| `-mode` | `AGENTBOX_MODE` | `cold` | cold 或 warm |
| `-concurrency` | `AGENTBOX_CONCURRENCY` | 1 | 任务并发，1～8，直到清理完成才释放 |
| `-warm-size` | `AGENTBOX_WARM_SIZE` | 3 | 固定 READY 目标；也作为动态最大值的默认来源 |
| `-warm-refill-policy` | `AGENTBOX_WARM_REFILL_POLICY` | `slots` | slots 在槽满或有排队时退让；idle 在任一前台任务在途时退让，仅适用 warm |
| `-warm-dynamic` | `AGENTBOX_WARM_DYNAMIC` | false | 按需求在上下界间调整 READY 目标，参数见[动态库存](dynamic-inventory.md) |
| `-max-sandboxes` | `AGENTBOX_MAX_SANDBOXES` | 6 | 所有创建、空闲、领取、运行、销毁实例的总上限，最多 8 |
| `-create-concurrency` | `AGENTBOX_CREATE_CONCURRENCY` | 1 | 按需与后台共享的创建并发，1～4；不受 READY 目标限制 |
| `-queue-size` | `AGENTBOX_QUEUE_SIZE` | 16 | 最多排队请求数，1～128 |
| `-queue-bytes` | `AGENTBOX_QUEUE_BYTES` | 8388608 | 排队请求的解码后文本字节预算，最多 64 MiB |
| `-queue-timeout` | `AGENTBOX_QUEUE_TIMEOUT` | `100ms` | 最大排队时间，1ms～5s |
| `-sandbox-cpu-milli` | `AGENTBOX_SANDBOX_CPU_MILLI` | 1000 | 每实例 CPU 带宽上限，50～4000；1000 表示 1 CPU |
| `-parent-cpu-milli` | `AGENTBOX_PARENT_CPU_MILLI` | 4000 | 沙箱父组静态 CPU 上限，50～4000；AIMD 最大值不得超过它 |

库存目标、执行并发、创建并发分别不得超过总实例上限；例如执行并发 8 需同时显式设置 max-sandboxes=8。扩大可配置范围不增加 CPU/内存配额，也不说明上限配置更快。CLI 接受 Go duration；固定服务配置解析器接受整数 `ms` 或 `s`。创建与排队参数对 Cold/Warm 同时生效；cold 显式启用动态库存会被拒绝。排队预算仅覆盖等待资源名额，预留成功后的创建计入 setup，不占用 queue-timeout。

字节预算计算 code、stdin、request_id、runtime 和 env 键值的字节长度，不是对 Go 堆内存的精确计量。HTTP 原始 body 另有既有上限。

CPU 默认父级 4 CPU 等效带宽、每实例 1 CPU，实例 swap=0、host pids.max=256。内存预算改为启动校验：读取宿主 MemTotal 与可见祖先 memory.max 的最小值，扣除外部保留量；父组默认取剩余额度与 4GiB 的较小值，单实例默认 512MiB，各自 memory.high 为硬上限的 75%。模式下可能同时存在的实例上限乘以单实例内存上限不得超过父组预算，否则拒绝启动：Warm 按 max-sandboxes 校验，Cold 因不存库存且清理前不释放执行槽，按 concurrency 校验。

单实例 CPU 是服务端统一模板，Cold 创建、Warm 初始库存与后续补池都在创建时写入子 cgroup。`500` 对应 `cpu.max=10000 20000`，`1500` 对应 `30000 20000`；整数 milli-CPU 避免浮点换算误差，周期固定 20ms。它是上限，不是绑核或算力预留，也不抬高父组预算。各子组额度可以合计超过父组，竞争时仍共同受父组约束；CPU 超配与内存预算校验的语义不同。

鉴权 status 的 `sandbox_cpu_milli` 报告配置值，不冒充外部修改后的实时读回。修改部署配置后需重启；启动回收旧实例再创建新实例。不支持请求携带 CPU/内存额度，也不在运行中动态调整单实例。需要差异化规格时再处理规格匹配、容量与公平性，当前不拆分多套预热库存。

`parent_cpu_milli` 报告配置的父组上限，默认仍 4000；例如 1000 对应父组 `cpu.max=20000 20000`。动态 CPU 控制可在此上限以下运行，实时值见 QoS 的 `actual_cpu`；配置的 `qos-max-cpu` 超出父组上限会拒绝启动。父组预算通过 CLI/systemd 正式部署，不再仅依赖实验直接修改 cgroup。

| CLI | systemd 环境变量 | 默认 MiB |
|---|---|---|
| `-parent-memory-mib` | `AGENTBOX_PARENT_MEMORY_MIB` | 0（按部署容量计算） |
| `-sandbox-memory-mib` | `AGENTBOX_SANDBOX_MEMORY_MIB` | 512 |
| `-memory-reserve-mib` | `AGENTBOX_MEMORY_RESERVE_MIB` | 512 |

保留量是规划额度，并不为其他服务建立独占内存保护，也不等同于当前可用内存。2GiB 部署可显式使用 parent=1024、sandbox=256、reserve=768、max-sandboxes=4、warm-size=1、concurrency=2；仍须结合业务实际内存调整。宿主共享依赖、管理进程与可见层级之外的竞争需要纳入部署评估。

## 调度与故障

领取 READY 实例和取得执行名额在同一把池锁下完成。已有等待者时新请求不能插队；队列限制同时检查数量和字节。取消和到期会移除对应等待者、释放预算并唤醒后继请求。

后台补池在执行槽饱和或存在排队请求时退让，status 的 `scheduling.refill_blocked_by` 说明阻塞原因；已预留创建允许完成，按需创建仍按统一预算服务 FIFO。压力门控同样暂停补池。

可选 `idle` 策略把退让条件扩大到任一前台任务持槽，包括准备、运行和销毁，报告 `foreground_active`；执行槽满时仍报告 `execution_saturated`。它只阻止**新的后台创建预留**，不取消已经开始的 create/start，也不阻止 FIFO 按需创建。最后一个前台任务清理并释放后，后台按既有并发与总容量预算恢复补池。短暂的无任务间隙也可触发补池，当前没有额外的空闲防抖等待。

`scheduling.refill_policy` 报告配置；`refill_creation_attempts` 统计后台创建预留，`refill_started_with_foreground` 统计预留当时已有前台任务的次数。计数在同一把调度锁内更新，可直接检查 idle 的后一计数为零；它不等于创建进程与前台执行完全没有时间重叠。Cold 不接受 idle 配置，切回 Cold 时同时恢复 slots。默认 slots 保留历史行为，不将 idle 宣称为对所有负载都更快。

需要替代 idle 的压力反馈时，可在 Warm/slots 下启用 `-qos=enforce -qos-refill-only -qos-protected-cgroup=...`。它复用现有 protected CPU PSI 状态机，仅暂停新的后台补池，不修改父组 CPU、准入、队列代次或在途请求；缺货按需创建仍可进行。使用有效 `some.total` 差分，高于 20% 持续 1 秒后暂停、低于 10% 稳定 2 秒后恢复；这些是可配置的工程参数。缺测保持上一次门控，启动未知沿用有界静态行为。作用域见 `qos.scheduling.refill_only` 和实际 `refill_paused`，不能把 `qos.pressure` 的通用准入建议当成已经拒绝任务。详见 [QoS](qos.md#scheduling)。

队列满或到期返回 503。READY 不足时，FIFO 队首可原子预留执行名额、创建名额和总容量，在锁外创建全新实例；资源不足才继续等待。按需创建不会绕过总容量、创建并发、压力门控或 FIFO，也不会在 Warm 超时后独立重放 Cold。排队取消不创建执行记录、不启动任务，返回取消结果时没有 execution_id、cleanup_complete 为 false，表示未取得需要清理的资源。

领取后先检查 runsc state 和 cgroup populated，再注入任务。空闲实例已死亡时，本次请求返回基础设施错误，销毁该实例并补充库存；不自动重放用户请求。当前不做空闲实例周期巡检。

后台创建失败采用 200ms 退避；按需创建失败返回请求错误。连续三次非取消创建失败后使 readiness 失败。清理失败立即停止接单和补池，保留 intent 和已占用容量，等待管理员解决后重启回收。已在创建中的操作允许完成并进入清理。

## 启动、关闭与恢复

`New` 完成检查和历史回收，实际引擎此时 readiness=false。`Start` 在统一 20 秒启动上下文内先创建一个一次性实例、执行只输出一个字节的可信 Python 探测并确认销毁；执行子预算为 2 秒，清理保留独立预算。通过后 Cold 不留库存，Warm 再等待至少一个 READY。探测不计入业务请求/池创建指标，成本归服务启动；Start 幂等，失败不隐式重试。关闭会取消探测并等待其回收。

探测保证的是启动时链路可用，不保证未来不发生磁盘抖动；不可中断内核 I/O 仍可能超出用户态截止时间。主程序给预热启动最多 20 秒；达到最低库存、绑定 HTTP listener 后才发送 systemd READY，剩余库存后台补齐。

readiness 表示初始化完成且未关闭或降级，不表示当前有空闲库存。临时耗尽但可补池时 readiness 仍为 true，接单结果由队列决定。

关闭时 HTTP drain 与 Engine.Close 同时开始。Close 先关接单门、取消引擎生命周期和后续补池，等待创建者收尾与所有已接单请求退出，再以最多 4 路并行销毁未使用的 READY 库存，最后释放状态锁。每个实例只在清理成功后释放容量；个别失败不会中止其他实例的清理，全部错误会汇总返回。运行中的请求即使由直接调用者提供 Background context，也会因引擎关闭而取消。runsc delete 使用 5 秒预算，之后的 cgroup kill/空组等待另有 2 秒预算，运行时管道收尾可能额外等待 1 秒；不可中断 I/O 仍不受用户态硬截止保证。主程序会报告库存清理错误；systemd mixed 终止策略在主进程退出或停止超时后终止剩余服务进程，继续提供最后兜底。

runsc 的状态文件使用截断后写入，参见其 [StateFile.SaveLocked 实现](https://github.com/google/gvisor/blob/master/runsc/container/state_file.go)。已经开始的 create/start 操作因此使用独立的 10 秒总预算：普通请求取消或服务关闭时，等待当前操作完成，不启动后续操作或用户代码，再进入销毁。这样避免主动取消在写入中途杀死 CLI。硬超时或外部 SIGKILL 仍可能破坏元数据；无法确认清理时保留记录并拒绝继续接单，不通过直接删除损坏状态伪装成功。

HTTP drain 预算为 20 秒；创建者/运行任务并行收尾按约 18 秒预留，随后最多两批库存清理按约 18 秒预留。systemd `TimeoutStopSec=45` 提供调度余量和最终强制终止边界。这是正常可中断操作的工程预算，不是对不可中断内核 I/O 的实时保证。

进程崩溃后，以 bundles 下的 intent 为恢复依据，清理未用 READY、部分创建和运行中的实例，再建立全新库存。配置所有权不绑定 cold/warm 模式，允许清理后在同一个状态目录切换模式。

## 代码职责

- `api.StatusReport` 定义对外状态响应；HTTP 的 `Executor` 接口明确要求 `Status()`，缺失能力在编译期发现。JSON 字段保持兼容。
- 池提供内部 `poolSnapshot`，不依赖 API 类型；Engine 将快照转换为状态报告，并合并引擎健康状态。
- `destroyInstanceResources` 统一回收容器、cgroup、输出流和实例目录，支持空闲实例及部分创建的实例；不生成任务报告，也不修改池状态。资源清理失败仍使引擎停止接单并保留 intent。
- `finalizeExecution` 调用资源回收，再填写任务输出、耗时、截断标志和最终状态。已领取任务进入 DESTROYING 的时机由执行流程协调；后台库存的状态由池管理。
- session 通过 `outputResult()` 和只读的 `outputLimitExceeded()` 通知提供输出能力，Engine 不直接访问收集器内部字段。

## 观测与时延口径

鉴权的 `GET /v1/status` 返回模式、readiness、库存目标、总上限，以及 creating/idle/leased/running/destroying、队列数量与字节数、创建次数/失败次数/累计耗时和清理失败次数。启用[动态库存](dynamic-inventory.md)时增加目标边界与动作统计，target 为实时目标。Cold 也提供同一调度与容量统计，target=0；`memory` 字段报告实际部署预算。

执行结果新增 `queue_ms`，原有字段保持：

- `setup_ms`：按需创建时包含创建运行环境和准备任务；READY 命中时为领取后的健康检查和任务文件准备。
- `execution_ms`：启动任务并等待结果，仍受 timeout_ms 约束。
- `cleanup_ms`：销毁运行环境、收集最终输出、删除记录。
- `total_ms`：本次引擎调用的排队、准备、执行和清理；不含提前预热与空闲等待，也不含 HTTP 传输和解码。

Usage 覆盖该实例整个 cgroup 生命周期，包括运行时创建及 warm 空闲期，不能据此直接比较纯 Python 用量。宿主 runsc CLI 位于 manager，继续不计入单实例 Usage。

## 系统服务启用

已经安装固定服务时，由管理员更新二进制和 unit，再用编辑器在 `/etc/agentbox.env` 中添加 `AGENTBOX_MODE=warm`，保留原 token、平台和其他配置：

```bash
make build
sudo systemctl stop agentbox.service
sudo python3 scripts/deploy.py base --update
sudoedit /etc/agentbox.env
sudo systemctl start agentbox.service
mkdir -p artifacts
sudo python3 scripts/deploy.py smoke --lifecycle --output "$PWD/artifacts/host-warm-systemd.json"
```

证据路径必须不存在。切回 cold 时只需修改模式并重启。预热实例是正常库存，smoke 只要求已完成任务的 ID 无残留，并检查总容量；停止服务后仍要求进程完全回收。功能用例先等待可领取库存，满载拒绝用例直接发送且不重试；不会把库存耗尽时合法的 503 当成用户代码执行错误。

## 可复现实验

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

保护判定预先固定为：Core 全窗口、沙箱到达窗口、其余恢复窗口，分别至少 99% 请求在 50ms 内成功返回，且成功 P99 ≤50ms；同时保留拒绝和沙箱代价。`CoreTargetPassed` 按三个窗口共同计算，不能只用全窗口均值/分位数掩盖波次失败。`ParentCPUStart/Final`、`QoSFinal`、新鲜 protected 采样数与暂停采样数记录动作是否真实发生；JSONL 保留压力状态、原因、Core CPU PSI、CPU 实际额度和变更次数。恢复阈值及 CPU 参数见[QoS](qos.md)，结果见[验收](validation.md)。

结果写入新的 `artifacts/benchmark-<platform>-<随机后缀>/`：

- requests.csv：保留所有请求，新增 wave、计划到达至响应、实际完成时刻和拒绝原因；原 wall 仍从实际发出请求计时，计划口径另包含客户端调度滞后。
- summary.json：配置、运行时/测试二进制哈希、各场景统计；wave_trace 保存相同到达轨迹，wave_runs 保存逐波结果及整个回放窗口资源。基准中的每秒完成数包含空闲时间，不能当作机器的持续容量。
- cold-pool.jsonl / warm-pool.jsonl：仅 waves 写出，保存 50ms 原始状态及资源样本；旧三场景继续使用原有 1ms 库存峰值采样。

`--samples` 只控制 inventory/steady 的请求数，`--rate` 只控制 steady；波次使用固定的 5/20 请求和 `--wave-cycles`（1～5，默认 2）。成功分位数仅统计成功且确认清理的响应，拒绝和错误单列；执行器不健康、采样无效不会伪装成普通容量拒绝。

持续接单能力同时受执行和创建约束。设执行并发 C、领取至清理完成平均时间 S、共享创建并发 B、平均创建时间 Tc，则稳定接单率需同时满足 λS < C 和 λTc < B；这些是容量约束，不是负载分布、CPU 争抢和资源回收条件下的精确吞吐预测。有限库存只能吸收短时突发。

容器基准包含外层 CPU/内存限制，并使用容器 PID/cgroup 命名空间；不能把其数值直接当作裸机 systemd 服务性能。需要裸机数据时，可在专门委派的独立测试环境中设置 AGENTBOX_BENCHMARK、AGENTBOX_BENCH_OUTPUT、AGENTBOX_RUNSC、AGENTBOX_ROOTFS、AGENTBOX_CGROUP_ROOT、AGENTBOX_PLATFORM 等变量后运行 `go test -tags=integration -run TestColdWarmHTTPBenchmark -count=1 -v ./tests/benchmarks`。不能与正在运行的服务共用委派子树或状态目录。

systemd mixed 的终止顺序依据 [systemd 249 官方说明](https://github.com/systemd/systemd/blob/v249/man/systemd.kill.xml)。真实服务测试要求正常停止结果为 success 且无执行记录残留；主进程崩溃时仍验证整个服务进程集合被回收。测试容器将 `/opt` 放在可执行 tmpfs 内，安装、更新和卸载仍执行完整文件复制和校验。

实际功能验收与冷暖数据见 [本阶段验收记录](validation.md#warm)。

本基准固定关闭动态策略，优先评估给定到达模式下的预热体验；动态控制与其他能力见各自文档。
