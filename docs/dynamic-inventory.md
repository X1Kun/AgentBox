# 有界动态预热库存

`-mode=warm -warm-dynamic` 启用库存控制，默认关闭。当前 v2 将普通补货与提高目标分开：只有真实库存阻塞触发上调，持续未消费的备用余量经过推荐稳定窗口后触发下调。固定模式继续使用 `-warm-size`，任务仍一实例一次使用。

## 配置

| CLI | systemd 环境变量 | 默认值 | 含义 |
| --- | --- | --- | --- |
| `-warm-dynamic` | `AGENTBOX_WARM_DYNAMIC` | false | 仅 warm 模式可启用 |
| `-warm-min-size` | `AGENTBOX_WARM_MIN_SIZE` | 1 | 最小 READY 目标，至少 1 |
| `-warm-max-size` | `AGENTBOX_WARM_MAX_SIZE` | 0 | 0 沿用 warm-size；有效最大值不超过 max-sandboxes |
| `-warm-adjust-interval` | `AGENTBOX_WARM_ADJUST_INTERVAL` | 1s | 最小目标动作间隔，100ms～60s |
| `-warm-downscale-window` | `AGENTBOX_WARM_DOWNSCALE_WINDOW` | 30s | 缩容推荐稳定窗口，不短于动作间隔，最多 3600s |

满足 `1 <= min <= max <= max-sandboxes <= 8`，创建并发不超过最大 READY 目标。CLI 接受 Go duration，固定服务配置使用整数 ms 或 s。100ms 短窗与控制器节拍是内部常量，状态变更还会触发观察和唤醒；动作间隔独立限制目标变化频率。启动仍在第一个 READY 后就绪，目标从最小值起步。

本轮将未提交 v1 diff 的 `-warm-idle-for` / `AGENTBOX_WARM_IDLE_FOR` 替换为上面的稳定窗口参数。它不再表示“整池无任务”的连续时长，旧参数没有保留别名。固定服务更新程序与 unit 后，可在 `/etc/agentbox.env` 中设置：

```text
AGENTBOX_MODE=warm
AGENTBOX_WARM_DYNAMIC=true
AGENTBOX_WARM_MIN_SIZE=1
AGENTBOX_WARM_MAX_SIZE=3
AGENTBOX_WARM_ADJUST_INTERVAL=1s
AGENTBOX_WARM_DOWNSCALE_WINDOW=30s
```

部署沿用 [systemd 更新流程](systemd-deployment.md)，本轮不自动更改或重启已安装服务。

## 请求与补货

- 领取成功只通知原有 refill 补货，不产生扩大目标的信号。正常消费后的短暂 READY 缺口不会被保留成扩容需求。
- FIFO 队首每次重查状态时区分 `slot_wait`、`inventory_wait`、`both_wait`。后面的请求首先受 FIFO 顺序约束，不重复代表队首的库存缺口。
- 仅执行名额可用、READY 为零时确认库存阻塞。每个内部排队者首次遇到该原因，增加一次 `inventory_stalled_requests`；重复唤醒不会重复计数。先同时缺两种资源、随后名额释放但仍无 READY，可在此时确认库存事件。
- 新确认的事件在冷却期间合并，至多推动一次一档上调；超过“动作间隔＋100ms”的事件会过期，达到最大值或总量约束时也不保存为未来的投机扩容。容量和压力判断独立于是否发生过库存阻塞。
- 已经真实等待库存的请求即使随后取消或到期，该次事件仍是有效观测；进入队列前已取消、或退出检查已发现取消/到期的请求不再新增事件。队列数量、字节和结果统计沿用既有规则。

补货仍取决于目标与 READY＋CREATING 的缺口，同时受创建并发、总实例容量、压力和清理健康约束。v2 不自动增加执行并发或创建 worker，也不把瞬时创建忙碌诊断成已证实的持续产能瓶颈。可选令牌桶仍留待实际需要时接入。

## 推荐稳定与回收

1. 所有池状态通知会在池锁内观察 READY，因此短暂耗尽不会被普通定时采样漏掉。100ms 短窗的 READY 最小值大于零、且无待处理真实缺货时，原始推荐降低一档；否则保持当前目标。
2. 缩容采用长窗内原始推荐的最大值。用单调队列维护，整数目标 1～8 使历史最多保留 8 条。只存原始推荐，不把稳定化后仍保持的高目标反复写回。
3. 每次接受目标变化后，以新目标重新播种历史；突发刚扩出的库存不会立即回收。逐级下行可能需要多个稳定窗口，不承诺精确缩容时刻。
4. 实际销毁只选择超过接受目标的 READY。已有执行中任务不再一律阻止回收备用量；若 FIFO 请求当前可取得名额和 READY，则先让调度分配。已领取实例不参与缩容。
5. 回收预留与领取使用同一把池锁。先移出 READY、标记 DESTROYING，再锁外销毁，确认成功前仍计入 total；失败保留 intent 并停止接单。

因此，每 10 秒有一个正常短请求也不会把目标从 1 推满；若一开始已有 3 个且一直存在未消费余量，即使请求持续出现，也可以缓慢回到 1。它是有界反馈规则，不能据此声称某个配置最优或吞吐必然提高。

## 压力、观测中断与关闭

enforce 压力调度关闭时，暂停新创建、目标调整和回收预留；已经开始的事务继续收尾。恢复会清掉旧缺货与推荐历史，以当前目标重新播种。observe 压力建议不改变实际门控。

观测或决策发生超过三个内部节拍的间隔、或时间倒退时，重建短窗和推荐历史，不能把缺测解释为空闲；新的有效历史足够后才能缩容。关闭或清理失效优先，停止会等待在途回收结束，再回收剩余库存，不释放未确认清理的容量。

## 状态接口

`GET /v1/status` 的顶层 `target` 仍是实际接受的 READY 目标。启用后增加 `inventory`：

- `raw_target`、`stabilized_target`、`target`：原始推荐、稳定后推荐、接受的目标。
- `min_target/max_target`、`observation_interval_ms`、`adjust_interval_ms`、`downscale_window_ms`：边界与时间配置。
- `reason/limited_by`：如 `inventory_stall`、`unused_reserve`、`sample_gap`，以及 `action_cooldown`、`downscale_stabilized`、`capacity_limited`、`maximum_inventory`。压力暂停单独报告 `qos_pressure`。
- `head_wait_reason`：当前 FIFO 队首可观测的资源条件；不驱动策略动作。
- `inventory_stalled_requests/slot_wait_requests/both_wait_requests`：队首在每种原因上的首次观测计数。同一请求可经历不同原因，这些计数不能相加当作入队人数；精确的按原因等待时长尚未单列，`queue_ms` 仍是总排队时间。
- `scale_ups/scale_downs`、`shrink_attempts/succeeded/failures`、`last_changed_at`：目标与实际回收的不同账本。

固定模式省略该对象。状态查询只读，建议不等于创建或销毁已经完成。

## 验证与依据

回归覆盖两个旧反例、原因转换与去重、取消/到期、冷却合并、最大/最小边界、稳定窗口、观测间隔异常、压力恢复、FIFO 优先、在途回收容量、关闭等待和失败保留。单调队列与朴素窗口扫描作确定性对照。

短真实用例先确认普通消费不扩容，再暂缓可信测试包装器中的替代创建，制造真正的库存等待，验证扩容、Python 执行、稳定回收及关闭无残留：

```bash
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py qos --platform systrap --pattern '^TestDynamicInventoryRuntime$'
```

该方式使用 tmpfs 测试依赖，不能替代正常磁盘路径验收。开源来源与适配思路见[设计与学习路线](dynamic-inventory.md#upstream)；修正前的问题保留在[理论复核](dynamic-inventory.md#theory)，其代码哈希和反例记录对应旧 v1 diff。

## 本轮验证记录

2026-10-20，v2 当前 diff 的完整 `make check` 和程序构建通过。两个旧反例转为不误扩容的确定性回归；另覆盖原因转换、事件合并与去重、取消/到期、上下界、单调队列、缺测和压力恢复、FIFO、在途回收和失败保留。

仅运行一个真实 systrap 用例，使用上述 tmpfs 测试依赖：正常消费未提高目标；可信包装器暂缓替代创建后，队首确认一次库存阻塞，目标 1→2；放行创建，Python 输出 `dynamic-ok`，稳定后目标 2→1。`inventory_stalled_requests=1`、`scale_ups=1`、`scale_downs=1`、`shrink_succeeded=1`、`shrink_failures=0`，关闭后无 bundle 或任务 cgroup 残留。用例报告 1.13 秒，依赖准备不计入其中。

本轮没有更新宿主服务或开展长时间性能实验，不将功能回归解释为延迟/吞吐收益，也不将 tmpfs 通过解释为历史磁盘问题已修复。

<a id="theory"></a>
## 库存理论与历史误判

READY 是缓冲库存，创建 worker 是补货产能，执行名额是服务能力；调整 READY 目标不会直接提高后两者。对稳定且每任务消耗一个实例的简化区间，成功率受 `min(k/E[T_create], C/E[T_slot], 其他约束)` 限制；Little 定律的在途人口不能直接当作最优 READY 公式。

旧 v1 有两个确定性反例，现已转为生产方法回归：已有 READY、仅执行名额不足时不能扩库存；稀疏请求总能及时补货时不能仅凭一次正常消费把目标推到最大。旧源码哈希与反例保存在 [理论证据 JSON](data/performance-theory-evidence.json)的 inventory_v1_review 字段；当前行为以本页 v2 为准。

书中依据支持预先准备、分别识别软件资源饱和、空闲回收和滞回，不证明默认 30 秒/1 秒最优，也不证明当前动态策略优于固定库存。对应引用见 [B07](performance-theory-references.md#b07)、[B28–B30](performance-theory-references.md#b28)，更完整模型见 [性能理论](performance-theory.md)。

<a id="upstream"></a>
## 开源机制与单调队列

以下借鉴的是源码机制，未运行这些上游项目，也不复用其性能数字。服务副本、可复用容器与一次性 READY 库存的语义不同。

| 项目与入口 | 实际机制 | 适合借鉴的部分与边界 |
| --- | --- | --- |
| [Apache OpenWhisk · ContainerPool.scala](https://github.com/apache/openwhisk/blob/master/core/invoker/src/main/scala/org/apache/openwhisk/core/containerpool/ContainerPool.scala) | `adjustPrewarmedContainer` 区分普通补货与周期调整；`increasePrewarms` 计入已预热和启动中实例；`getReactiveCold` 根据窗口内 cold-start 计数、threshold/increment 和 min/max 给出需求。预热实例还可按 TTL 回收。 | 重点是把正常消费补货和缺货反馈分开。AgentBox warm 不回退冷启动，应以真实库存阻塞替代 cold-start 信号；不能照搬其任务容器复用语义。 |
| [Knative Serving · autoscaler.go](https://github.com/knative/serving/blob/main/pkg/autoscaler/scaling/autoscaler.go) | `Scale` 读取 stable/panic 两个窗口，分别计算期望服务副本数；panic 状态只增加规模，延后降低；可配置延迟窗口，数据不可用时返回无有效建议。 | 学习快响应与慢回收、缺测不作低需求判断。它调服务副本，AgentBox 调 READY，不能把其并发量除以目标值的副本公式直接当库存公式。 |
| [Kubernetes HPA · horizontal.go，v1.36.0](https://github.com/kubernetes/kubernetes/blob/v1.36.0/pkg/controller/podautoscaler/horizontal.go) | `stabilizeRecommendationWithBehaviors` 从历史原始推荐值求稳定范围；下行取窗口内最大值。之后 `convertDesiredReplicasWithBehaviorRate` 另行施加动作速率及 min/max。 | 将原始建议、稳定化和执行限制分层。窗口应记录未经动作限速的推荐，避免把“当前仍保持较高目标”反复写回，导致永不缩容。 |
| [kubernetes-sigs/agent-sandbox · sandboxwarmpool_controller.go](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/extensions/controllers/sandboxwarmpool_controller.go) | `reconcilePool` 按声明目标与实际池成员对账，区分 Ready、未 Ready、终止中；创建会计入仍存在的终止成员。`takeRefillTokens` 用每池令牌桶控制替代创建的发起速率；还存在批处理与缓存变更确认机制。 | 最接近沙箱供给层。借鉴目标与供给分离、在途容量和限速补池。AgentBox 没有 Kubernetes informer，不需要照搬缓存 expectations；最多 8 实例也无需引入其大批量并行创建。 |


当前实现将实际阻塞归因、原始建议、稳定建议和动作限制分层。缺测或压力暂停不解释为低需求，恢复时重建有效窗口；普通补货不推动目标上调。可选令牌桶尚未接入，不把创建并发上限误称为精确创建速率限制。

下行稳定窗口采用滑动最大推荐，internal/sandbox/inventory_policy.go 的 recommendationMax 使用单调队列：过期记录从队首移除，不高于新建议的旧记录从队尾移除，再追加新记录；队首即窗口最大值。每条记录最多进入/离开一次，更新摊还 O(1)。写入历史的是原始建议，不能把暂时保持的高目标反复写回，否则可能一直无法缩容。

完整旧调研、反例过程与逐轮流水账已放入整理备份；当前页保留操作、算法与验证边界。
