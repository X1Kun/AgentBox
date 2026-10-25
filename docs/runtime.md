# 待退役 Daemon 的运行时参考

agentboxd 已决定退役，本文记录仍存在于当前源码的旧实现，供迁移收尾、代码复盘和历史实验理解。新平台不依赖本地 FIFO/Warm/AIMD，不将本页状态机移植成另一套节点 Agent。旧配置见[运维参考](operations.md#config)，旧接口见[使用参考](usage.md)，证据见[验证指南](validation.md)。

有价值的经验会转化为平台验收：隔离边界、资源归属、结果不确定性、取消后的收敛与故障证据。运行管理由 Job/containerd/gVisor 承接，队列由 Kueue 承接；这不要求保留旧进程、接口或性能指标。实际移除以[退役条件](roadmap.md#retirement)为准，历史描述不表示目标能力已实现。

<a id="pool"></a>
## 一次性实例与原子调度

Cold 不维持 READY 库存，Warm 提前准备全新实例；二者共用 FIFO、创建并发、总容量与相同执行/清理规则。一次性语义用于减少跨任务进程、内存、文件描述符和解释器状态残留，不是完整安全性证明。

后台提前创建 bundle、输入目录、cgroup 和 OCI 配置，运行 `runsc create/start`，使 Sentry、Gofer 和等待中的 init 存活。领取后才写入 task.json/stdin，通过附着式 `runsc exec` 启动本次 Python 任务。

这版没有提前加载任务 Python 解释器。等待中的 init 是独立 Python 进程，用户代码仍在新的 Python 进程中执行；解释器启动、exec 控制调用、健康检查和收尾成本仍在请求路径中。

```text
后台创建 → READY → LEASED → RUNNING → DESTROYING → 删除记录
                      ↘ 准备失败/取消 ↗
```

实例只能被领取一次，完成或失败后销毁整个运行环境，不能从 RUNNING 返回 READY。即使任务尚未启动，领取后的取消也需要销毁。不同任务不共享可写工作目录。

### 输入准备

`prepareSandboxFiles` 创建不含用户数据的目录、config.json 和 process.json；`prepareInput` 在领取后写入代码、env 和 stdin。session 在任务 start 时才打开 stdin，预热阶段不持有任务输入。

输入目录为只读 bind mount，宿主之后写入的文件通过 gVisor 的共享文件访问语义可见；不能把该目录设为 exclusive 文件访问缓存。目录与 task.json 的 guest 访问权限仍显式 chmod，独立于 systemd 的 `UMask=0077`。stdin 仅通过宿主打开的描述符传递。

### 调度与失败

领取 READY 实例和取得执行名额在同一把池锁下完成。已有等待者时新请求不能插队；队列限制同时检查数量和字节。取消和到期会移除对应等待者、释放预算并唤醒后继请求。

后台补池在执行槽饱和或存在排队请求时退让，status 的 `scheduling.refill_blocked_by` 说明阻塞原因；已预留创建允许完成，按需创建仍按统一预算服务 FIFO。压力门控同样暂停补池。

可选 `idle` 策略把退让条件扩大到任一前台任务持槽，包括准备、运行和销毁，报告 `foreground_active`；执行槽满时仍报告 `execution_saturated`。它只阻止**新的后台创建预留**，不取消已经开始的 create/start，也不阻止 FIFO 按需创建。最后一个前台任务清理并释放后，后台按既有并发与总容量预算恢复补池。短暂的无任务间隙也可触发补池，当前没有额外的空闲防抖等待。

`scheduling.refill_policy` 报告配置；`refill_creation_attempts` 统计后台创建预留，`refill_started_with_foreground` 统计预留当时已有前台任务的次数。计数在同一把调度锁内更新，可直接检查 idle 的后一计数为零；它不等于创建进程与前台执行完全没有时间重叠。Cold 不接受 idle 配置，切回 Cold 时同时恢复 slots。默认 slots 保留历史行为，不将 idle 宣称为对所有负载都更快。

可选压力门控与补池范围见[压力调度](#pressure)；配置只在[运维指南](operations.md#qos-config)维护。

队列满或到期返回 503。READY 不足时，FIFO 队首可原子预留执行名额、创建名额和总容量，在锁外创建全新实例；资源不足才继续等待。按需创建不会绕过总容量、创建并发、压力门控或 FIFO，也不会在 Warm 超时后独立重放 Cold。排队取消不创建执行记录、不启动任务，返回取消结果时没有 execution_id、cleanup_complete 为 false，表示未取得需要清理的资源。

领取后先检查 runsc state 和 cgroup populated，再注入任务。空闲实例已死亡时，本次请求返回基础设施错误，销毁该实例并补充库存；不自动重放用户请求。当前不做空闲实例周期巡检。

后台创建失败采用 200ms 退避；按需创建失败返回请求错误。连续三次非取消创建失败后使 readiness 失败。清理失败立即停止接单和补池，保留 intent 和已占用容量，等待管理员解决后重启回收。已在创建中的操作允许完成并进入清理。

<a id="lifecycle"></a>
## 启动、关闭与恢复

`New` 完成检查和历史回收，实际引擎此时 readiness=false。`Start` 在统一 20 秒启动上下文内先创建一个一次性实例、执行只输出一个字节的可信 Python 探测并确认销毁；执行子预算为 2 秒，清理保留独立预算。通过后 Cold 不留库存，Warm 再等待至少一个 READY。探测不计入业务请求/池创建指标，成本归服务启动；Start 幂等，失败不隐式重试。关闭会取消探测并等待其回收。

探测保证的是启动时链路可用，不保证未来不发生磁盘抖动；不可中断内核 I/O 仍可能超出用户态截止时间。主程序给预热启动最多 20 秒；达到最低库存、绑定 HTTP listener 后才发送 systemd READY，剩余库存后台补齐。

readiness 表示初始化完成且未关闭或降级，不表示当前有空闲库存。临时耗尽但可补池时 readiness 仍为 true，接单结果由队列决定。

关闭时 HTTP drain 与 Engine.Close 同时开始。Close 先关接单门、取消引擎生命周期和后续补池，等待创建者收尾与所有已接单请求退出，再以最多 4 路并行销毁未使用的 READY 库存，最后释放状态锁。每个实例只在清理成功后释放容量；个别失败不会中止其他实例的清理，全部错误会汇总返回。运行中的请求即使由直接调用者提供 Background context，也会因引擎关闭而取消。runsc delete 使用 5 秒预算，之后的 cgroup kill/空组等待另有 2 秒预算，运行时管道收尾可能额外等待 1 秒；不可中断 I/O 仍不受用户态硬截止保证。主程序会报告库存清理错误；systemd mixed 终止策略在主进程退出或停止超时后终止剩余服务进程，继续提供最后兜底。

runsc 的状态文件使用截断后写入，参见其 [StateFile.SaveLocked 实现](https://github.com/google/gvisor/blob/master/runsc/container/state_file.go)。已经开始的 create/start 操作因此使用独立的 10 秒总预算：普通请求取消或服务关闭时，等待当前操作完成，不启动后续操作或用户代码，再进入销毁。这样避免主动取消在写入中途杀死 CLI。硬超时或外部 SIGKILL 仍可能破坏元数据；无法确认清理时保留记录并拒绝继续接单，不通过直接删除损坏状态伪装成功。

HTTP drain 预算为 20 秒；创建者/运行任务并行收尾按约 18 秒预留，随后最多两批库存清理按约 18 秒预留。systemd `TimeoutStopSec=45` 提供调度余量和最终强制终止边界。这是正常可中断操作的工程预算，不是对不可中断内核 I/O 的实时保证。

进程崩溃后，以 bundles 下的 intent 为恢复依据，清理未用 READY、部分创建和运行中的实例，再建立全新库存。配置所有权不绑定 cold/warm 模式，允许清理后在同一个状态目录切换模式。

<a id="resources"></a>
## 资源与统计边界

父组控制沙箱总预算，子组提供单实例上限。CPU quota 是带宽上限，不代表绑核或预留；子额度之和可大于父额度。内存部署校验按可见宿主/祖先容量扣除保留量，Warm 校验总实例上限、Cold 校验执行并发能否容纳，保留量不是实际空闲内存保证。

任务使用非 root、只读 rootfs、独立有界临时目录、禁外网、空 capabilities 和 NoNewPrivileges。代码与环境经结构化文件传入，不拼接到宿主 shell；资源规格不由请求任意指定。宿主 pids 限额不等于 guest 精确进程数。

Usage 在任务退出或触发终止后、清理前采样，包含运行时创建和 Warm 待命，不含后续清理和 manager 中的 runsc CLI；因此既不是纯 Python 成本，也不是完整生命周期记账。OOM 依据宿主 cgroup 事件，不能只凭退出码推断。

<a id="inventory"></a>
## 动态 READY 库存

正常领取只触发补货，不自动扩大库存目标。库存、执行槽和创建产能分开控制；更多 READY 不能解决 CPU 或长期创建产能不足。

- 领取成功只通知原有 refill 补货，不产生扩大目标的信号。正常消费后的短暂 READY 缺口不会被保留成扩容需求。
- FIFO 队首每次重查状态时区分 `slot_wait`、`inventory_wait`、`both_wait`。后面的请求首先受 FIFO 顺序约束，不重复代表队首的库存缺口。
- 仅执行名额可用、READY 为零时确认库存阻塞。每个内部排队者首次遇到该原因，增加一次 `inventory_stalled_requests`；重复唤醒不会重复计数。先同时缺两种资源、随后名额释放但仍无 READY，可在此时确认库存事件。
- 新确认的事件在冷却期间合并，至多推动一次一档上调；超过“动作间隔＋100ms”的事件会过期，达到最大值或总量约束时也不保存为未来的投机扩容。容量和压力判断独立于是否发生过库存阻塞。
- 已经真实等待库存的请求即使随后取消或到期，该次事件仍是有效观测；进入队列前已取消、或退出检查已发现取消/到期的请求不再新增事件。队列数量、字节和结果统计沿用既有规则。

补货仍取决于目标与 READY＋CREATING 的缺口，同时受创建并发、总实例容量、压力和清理健康约束。v2 不自动增加执行并发或创建 worker，也不把瞬时创建忙碌诊断成已证实的持续产能瓶颈。可选令牌桶仍留待实际需要时接入。

### 缩容稳定与回收

1. 所有池状态通知会在池锁内观察 READY，因此短暂耗尽不会被普通定时采样漏掉。100ms 短窗的 READY 最小值大于零、且无待处理真实缺货时，原始推荐降低一档；否则保持当前目标。
2. 缩容采用长窗内原始推荐的最大值。用单调队列维护，整数目标 1～8 使历史最多保留 8 条。只存原始推荐，不把稳定化后仍保持的高目标反复写回。
3. 每次接受目标变化后，以新目标重新播种历史；突发刚扩出的库存不会立即回收。逐级下行可能需要多个稳定窗口，不承诺精确缩容时刻。
4. 实际销毁只选择超过接受目标的 READY。已有执行中任务不再一律阻止回收备用量；若 FIFO 请求当前可取得名额和 READY，则先让调度分配。已领取实例不参与缩容。
5. 回收预留与领取使用同一把池锁。先移出 READY、标记 DESTROYING，再锁外销毁，确认成功前仍计入 total；失败保留 intent 并停止接单。

因此，每 10 秒有一个正常短请求也不会把目标从 1 推满；若一开始已有 3 个且一直存在未消费余量，即使请求持续出现，也可以缓慢回到 1。它是有界反馈规则，不能据此声称某个配置最优或吞吐必然提高。

### 压力与观测中断

enforce 压力调度关闭时，暂停新创建、目标调整和回收预留；已经开始的事务继续收尾。恢复会清掉旧缺货与推荐历史，以当前目标重新播种。observe 压力建议不改变实际门控。

观测或决策发生超过三个内部节拍的间隔、或时间倒退时，重建短窗和推荐历史，不能把缺测解释为空闲；新的有效历史足够后才能缩容。关闭或清理失效优先，停止会等待在途回收结束，再回收剩余库存，不释放未确认清理的容量。

### 库存状态字段

`GET /v1/status` 的顶层 `target` 仍是实际接受的 READY 目标。启用后增加 `inventory`：

- `raw_target`、`stabilized_target`、`target`：原始推荐、稳定后推荐、接受的目标。
- `min_target/max_target`、`observation_interval_ms`、`adjust_interval_ms`、`downscale_window_ms`：边界与时间配置。
- `reason/limited_by`：如 `inventory_stall`、`unused_reserve`、`sample_gap`，以及 `action_cooldown`、`downscale_stabilized`、`capacity_limited`、`maximum_inventory`。压力暂停单独报告 `qos_pressure`。
- `head_wait_reason`：当前 FIFO 队首可观测的资源条件；不驱动策略动作。
- `inventory_stalled_requests/slot_wait_requests/both_wait_requests`：队首在每种原因上的首次观测计数。同一请求可经历不同原因，这些计数不能相加当作入队人数；精确的按原因等待时长尚未单列，`queue_ms` 仍是总排队时间。
- `scale_ups/scale_downs`、`shrink_attempts/succeeded/failures`、`last_changed_at`：目标与实际回收的不同账本。

固定模式省略该对象。状态查询只读，建议不等于创建或销毁已经完成。

<a id="inventory-history"></a>
### 历史误判与理论边界

READY 是缓冲库存，创建 worker 是补货产能，执行名额是服务能力；调整 READY 目标不会直接提高后两者。对稳定且每任务消耗一个实例的简化区间，成功率受 `min(k/E[T_create], C/E[T_slot], 其他约束)` 限制；Little 定律的在途人口不能直接当作最优 READY 公式。

旧 v1 有两个确定性反例，现已转为生产方法回归：已有 READY、仅执行名额不足时不能扩库存；稀疏请求总能及时补货时不能仅凭一次正常消费把目标推到最大。旧源码哈希与反例保存在 [理论证据 JSON](data/performance-theory-evidence.json)的 inventory_v1_review 字段；当前行为以本页 v2 为准。

书中依据支持预先准备、分别识别软件资源饱和、空闲回收和滞回，不证明默认 30 秒/1 秒最优，也不证明当前动态策略优于固定库存。对应引用见 [B07](archive/performance-references.md#b07)、[B28–B30](archive/performance-references.md#b28)，更完整模型见 [性能理论](archive/performance-theory.md)。

<a id="upstream"></a>
### 机制来源

以下借鉴的是源码机制，未运行这些上游项目，也不复用其性能数字。服务副本、可复用容器与一次性 READY 库存的语义不同。

| 项目与入口 | 实际机制 | 适合借鉴的部分与边界 |
| --- | --- | --- |
| [Apache OpenWhisk · ContainerPool.scala](https://github.com/apache/openwhisk/blob/master/core/invoker/src/main/scala/org/apache/openwhisk/core/containerpool/ContainerPool.scala) | `adjustPrewarmedContainer` 区分普通补货与周期调整；`increasePrewarms` 计入已预热和启动中实例；`getReactiveCold` 根据窗口内 cold-start 计数、threshold/increment 和 min/max 给出需求。预热实例还可按 TTL 回收。 | 重点是把正常补货和缺货反馈分开。AgentBox Warm 缺货可有界按需创建，但只把真实排队库存阻塞作为扩容信号；不能把每次按需创建都当成扩库存依据，也不复用执行过的实例。 |
| [Knative Serving · autoscaler.go](https://github.com/knative/serving/blob/main/pkg/autoscaler/scaling/autoscaler.go) | `Scale` 读取 stable/panic 两个窗口，分别计算期望服务副本数；panic 状态只增加规模，延后降低；可配置延迟窗口，数据不可用时返回无有效建议。 | 学习快响应与慢回收、缺测不作低需求判断。它调服务副本，AgentBox 调 READY，不能把其并发量除以目标值的副本公式直接当库存公式。 |
| [Kubernetes HPA · horizontal.go，v1.36.0](https://github.com/kubernetes/kubernetes/blob/v1.36.0/pkg/controller/podautoscaler/horizontal.go) | `stabilizeRecommendationWithBehaviors` 从历史原始推荐值求稳定范围；下行取窗口内最大值。之后 `convertDesiredReplicasWithBehaviorRate` 另行施加动作速率及 min/max。 | 将原始建议、稳定化和执行限制分层。窗口应记录未经动作限速的推荐，避免把“当前仍保持较高目标”反复写回，导致永不缩容。 |
| [kubernetes-sigs/agent-sandbox · sandboxwarmpool_controller.go](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/extensions/controllers/sandboxwarmpool_controller.go) | `reconcilePool` 按声明目标与实际池成员对账，区分 Ready、未 Ready、终止中；创建会计入仍存在的终止成员。`takeRefillTokens` 用每池令牌桶控制替代创建的发起速率；还存在批处理与缓存变更确认机制。 | 最接近沙箱供给层。借鉴目标与供给分离、在途容量和限速补池。AgentBox 没有 Kubernetes informer，不需要照搬缓存 expectations；最多 8 实例也无需引入其大批量并行创建。 |


当前实现将实际阻塞归因、原始建议、稳定建议和动作限制分层。缺测或压力暂停不解释为低需求，恢复时重建有效窗口；普通补货不推动目标上调。可选令牌桶尚未接入，不把创建并发上限误称为精确创建速率限制。

下行稳定窗口采用滑动最大推荐，internal/sandbox/inventory_policy.go 的 recommendationMax 使用单调队列：过期记录从队首移除，不高于新建议的旧记录从队尾移除，再追加新记录；队首即窗口最大值。每条记录最多进入/离开一次，更新摊还 O(1)。写入历史的是原始建议，不能把暂时保持的高目标反复写回，否则可能一直无法缩容。

历史反例与理论依据见[研究归档](archive/performance-theory.md)，逐轮验收见[本地记录](archive/local-validation.md#inventory-validation)。

<a id="qos"></a>
<a id="cpu"></a>
## CPU 控制与压力调度

本地观察器读取受保护业务、沙箱、manager、祖先与宿主背景信号。CPU 策略是纯决策逻辑，执行器只修改本 Manager 的沙箱父组；用户态策略决定动作，内核执行限制。配置组合和默认阈值见[运维指南](operations.md#qos-config)，不能声称所有动作可任意独立组合。

CPU AIMD 在受保护业务持续高压且沙箱实际消耗 CPU 时乘法降额；低压稳定且沙箱已节流或用量接近预算时加法恢复。恢复最多累计两个动作间隔，避免长时间空闲后的跃升。上下界、滞回与冷却限制动作，不构成稳定性或 SLO 保证。

写入后读回确认，再提交预算。外部修改、读取/写入失败或读回不一致会锁定 degraded 并停止自动写入，需要排除问题后重启；实际值不确定时为 null。observe 只计算虚拟建议，不模拟配额改变后的反馈。启动 enforce 可先收紧到配置上界，初始预算低于下界拒绝初始化。

首次/缺失/回退/过期采样保持预算并重建连续窗口；沙箱自身高 PSI 不直接触发连续降额。辅助 I/O/PID 缺测不单独否决有效 CPU 决策。数据有效性定义见[运维指南](operations.md#sampling)。

<a id="pressure"></a>
### 压力门控

| 状态 | 条件 | 接单与补池 |
| --- | --- | --- |
| normal | 正常或恢复过渡结束 | 开放 |
| pressured | protected 高压持续达到确认时间 | 拒绝新任务，暂停新创建预留 |
| recovering | 连续有效低压达到稳定时间 | 恢复；一个动作间隔后转 normal |
| unknown | 必需采样缺失、过期或无差分 | 保持上次有效门控 |

初始 unknown 沿用静态有界行为；warm 先取得最小 READY 再启动观测，避免循环依赖。曾经关闭的门控必须由有效低压解除，沙箱用量归零不等于 protected 已恢复。CPU 执行器故障不阻止压力状态机继续工作。

- Cold/Warm 都在入口与最终资源预留处检查，排队期间也响应门控。
- 压力拒绝返回 `503 reason=qos_pressure`。尚未领取的排队者被唤醒、释放队列数量/字节并记录真实等待；门控关闭的代次避免快速关闭后重开漏掉排队者。
- 已领取任务继续执行，不由压力门控取消。已预留的 create 事务继续完成，新预留暂停后等待事件，不空转。
- 压力暂停不销毁现有 READY、不修改库存目标；恢复后按当前目标补池。动态目标由[库存控制器](#inventory)负责。
- 关闭和清理失败优先，不会被恢复通知重新打开。临时压力本身不将 Engine 标为损坏，readyz 也不保证当前有容量或接单开放。

完整压力门控与 CPU 执行器健康独立；CPU 写入失败不会绕过门控，也不自动令整个 Engine 损坏。shutdown 和清理失败优先，不能被低压恢复重新打开。

仅补池模式复用同一压力分类，但只阻止新的 Warm 后台预留，不改 CPU、准入或队列代次，不取消在途任务，前台按需创建仍可进行。通用压力建议不代表门控实际生效；查看 qos.scheduling 的实际字段。

<a id="business"></a>
### 可选业务反馈与额度借用

base CPU 非零且配置业务健康文件时启用：从不超过 base 的额度启动，仅业务样本有效健康、PSI 连续低压且确有需求时增额。业务有拒绝或 SLO 超标时撤回超过 base 的额度；持续恶化可继续降到 min，base 本身不保证 SLO。

样本失效时，在下一可执行控制周期撤回超过 base 的额度；已低于 base 则保持并禁止恢复。恢复重新积累有效窗口；执行器故障仍会停止写入，不能保证撤额成功。采集器文件契约见[配置说明](operations.md#business-config)，AgentBox 不自动生成生产业务指标。

<a id="lessons"></a>
## 实现取舍与代码入口

短任务不直接充当 init，而由待命 init 维持 Sentry，再用附着 exec 获取退出结果。task PID 文件确认任务已创建，结合状态检查区分用户退出 125/128 与控制故障；没有实例复用。

清理记录是 bundle 目录及配置，不是独立的 intent 标记文件。flock 和所有权检查限制恢复范围；不能保证恢复所有未记录资源或主机掉电损坏。普通磁盘超时曾观察到 CLI 在文件缺页等待中的 D 状态，不能把所有历史故障都归因于同一存储原因；详见[历史证据](archive/local-validation.md)。

- `api.StatusReport` 定义对外状态响应；HTTP 的 `Executor` 接口明确要求 `Status()`，缺失能力在编译期发现。JSON 字段保持兼容。
- 池提供内部 `poolSnapshot`，不依赖 API 类型；Engine 将快照转换为状态报告，并合并引擎健康状态。
- `destroyInstanceResources` 统一回收容器、cgroup、输出流和实例目录，支持空闲实例及部分创建的实例；不生成任务报告，也不修改池状态。资源清理失败仍使引擎停止接单并保留 intent。
- `finalizeExecution` 调用资源回收，再填写任务输出、耗时、截断标志和最终状态。已领取任务进入 DESTROYING 的时机由执行流程协调；后台库存的状态由池管理。
- session 通过 `outputResult()` 和只读的 `outputLimitExceeded()` 通知提供输出能力，Engine 不直接访问收集器内部字段。

研究推导和引用保留在[理论归档](archive/performance-theory.md)及[引用索引](archive/performance-references.md)。本页作为旧实现参考保留，不承担新平台设计或持续功能扩展。
