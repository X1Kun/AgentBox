# 单机 QoS：CPU 预算与压力调度

CPU 预算、压力准入和补池门控使用同一组有效压力观测，分别启用。默认关闭，不改变一次性沙箱、FIFO、固定执行并发或实例容量上限。采样字段与有效性见 [观测说明](observability.md)，已验证结果和限制见 [验收汇总](validation.md#qos)。

## 启用与配置

| 配置 | 实际行为 |
| --- | --- |
| `-qos=off` | 静态资源限制，不启动采样 |
| `-qos=observe` | 只读采样 |
| `-qos=observe -qos-cpu-control` | 预演 CPU 预算，不写配额 |
| `-qos=observe -qos-scheduling` | 预演压力门控，不改变接单或补池 |
| `-qos=enforce` | 执行 CPU 控制并读回确认；调度门控仍默认关闭 |
| `-qos=enforce -qos-scheduling` | 同时执行 CPU 控制、压力准入和补池门控 |
| `-qos=enforce -qos-refill-only` | 仅按 PSI 暂停/恢复新的 Warm 后台补池，不修改 CPU 配额或准入 |

observe/enforce 都需显式指定受保护服务：`-qos-protected-cgroup=/sys/fs/cgroup/...`。该路径不得与沙箱子树相互包含。systemd 配置示例：

```ini
AGENTBOX_QOS=enforce
AGENTBOX_QOS_SCHEDULING=true
AGENTBOX_QOS_PROTECTED_CGROUP=/sys/fs/cgroup/system.slice/mockcore.service
```

预演可改为 observe，并设置 `AGENTBOX_QOS_CPU_CONTROL=true`。模式默认 off，CPU 预演、完整压力调度和仅补池三个开关默认 false。父组预算及 CPU 上下界支持固定服务配置，其余策略参数通过 daemon CLI 设置；部署与更新见 [systemd](systemd-deployment.md)。

<a id="cpu"></a>
## CPU 预算控制

`internal/qos/policy.go` 决策，controller 协调采样和执行器，`internal/cgroup/cpu.go` 只修改 Manager 拥有的沙箱父级。

单实例固定上限另由服务端 `-sandbox-cpu-milli` 配置，默认 1000（1 CPU），见[实例配置](warm-pool.md#配置)。AIMD 不修改子组额度；调低父组预算时，即使子组还有自身额度也可能等待父组补充。这是总预算与实例边界两层控制，不是每个实例各跑一套动态策略。

| 参数 | 默认值 | CLI |
| --- | --- | --- |
| CPU 下界 / 上界 | 0.5 / 4 核等效带宽 | qos-min-cpu / qos-max-cpu |
| protected CPU PSI 低 / 高阈值 | 10% / 20% | qos-cpu-low / qos-cpu-high |
| 压力确认与最小动作间隔 | 1s | qos-control-interval |
| 恢复前持续低压 | 2s | qos-stable-for |
| 乘法下降系数 | 0.8 | qos-decrease-factor |
| 加法恢复速率 | 0.25 核/秒 | qos-increase-cpu |
| CPU bandwidth 周期 | 固定 20ms | 不动态修改 |

高压持续确认且沙箱确实消耗 CPU 时，按乘法下降降低预算；低压持续稳定且存在预算需求（已节流或用量接近预算）时按时间加法恢复。滞回区间或无需求时保持。恢复最多累计两个动作间隔，避免长时间空闲后大幅跃升。动作间隔不得小于采样周期，上述参数属于工程起点，没有最优性证明。

配额写入后读回确认，再提交实际预算。发现外部修改、读取/写入失败或读回不一致时锁定 degraded，停止自动写入，需排除问题并重启。无法确认实际预算时 actual_cpu 为 null。启动 enforce 时可先收紧到配置上界；observe 不写，初始预算低于下界则拒绝初始化。

必需采样缺失、首次基线、计数回退、过期或异常间隔时保持预算，并重建高/低压连续判断。系统或沙箱自身 PSI 不直接触发连续降额；可选 I/O/PID 缺测不单独阻止有效 CPU 决策。预演不模拟预算改变对后续压力的反馈，不能据此证明收益。

<a id="scheduling"></a>
## 压力准入与补池

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
- 压力暂停不销毁现有 READY、不修改库存目标；恢复后按当前目标补池。动态目标由[库存控制器](dynamic-inventory.md)负责。
- 关闭和清理失败优先，不会被恢复通知重新打开。临时压力本身不将 Engine 标为损坏，readyz 也不保证当前有容量或接单开放。

## 状态、关闭与验收

鉴权 status 分别报告 `qos.pressure`（判断）、`qos.scheduling`（门控实际状态）、`qos.cpu_control`（建议/实际预算、动作、读回与错误）。observe 不将建议伪装为生效；HTTP 查询只读缓存，不驱动动作。

pressure_rejections 包含立即和排队后拒绝；queue_pressure_rejected 只统计实际入队者：

```text
queue_entered = assigned.count + canceled.count + timed_out.count
              + unavailable.count + pressure_rejected.count + queued
```

CPU 控制降级不自动关闭 Engine；静态限制和清理失败停止接单仍生效。停止时不主动放宽配额；新 Engine 先恢复静态边界再初始化策略。

### 仅补池的 PSI 作用域

`qos-refill-only` 可在 observe 下预演、enforce 下执行，要求 Warm 模式；与 `qos-cpu-control`、`qos-scheduling`、业务额度借用互斥。它复用 protected CPU PSI 的确认、滞回和缺测保持状态机，但不创建 CPU 执行器，也不关闭准入、清退队列或取消在途请求。已有创建继续完成。初始未知沿用有界静态补池，曾暂停后需有效低压稳定才能恢复。

要替代 idle，配置 `warm-refill-policy=slots`，由 PSI 决定额外暂停条件；数量、创建并发、队列优先及实例总上限继续生效。`qos.pressure.suggested_admission_open` 是通用建议，实际行为看 `qos.scheduling.refill_only/admission_open/refill_paused`。仅补池时，即使建议关闭准入，实际准入仍开放。

```ini
AGENTBOX_MODE=warm
AGENTBOX_WARM_REFILL_POLICY=slots
AGENTBOX_PARENT_CPU_MILLI=1000
AGENTBOX_QOS=enforce
AGENTBOX_QOS_REFILL_ONLY=true
AGENTBOX_QOS_PROTECTED_CGROUP=/sys/fs/cgroup/system.slice/core.service
```

父组静态预算由 `-parent-cpu-milli` 设置，默认 4000（4 CPU），范围 50～4000。CPU AIMD 的 `qos-max-cpu` 不得超过该预算；固定服务可用 `AGENTBOX_QOS_MIN_CPU/MAX_CPU` 配置上下界。PSI 仅补池不修改此预算，不把动态反馈等同于无限可借用资源。当前高/低阈值 20%/10%、确认 1 秒、低压稳定 2 秒为工程起点，不能保证 50ms 业务时延，尤其是首次突发和已经开始的创建。

```bash
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py qos --platform kvm
```

验收使用私有容器、6 CPU/5GiB/2304 tasks，区别于静态对照的外层 2 CPU。覆盖确定性压力下的配额写入/读回，以及真实 CPU PSI 下的准入、排队拒绝、补池暂停、缺测保持、稳定恢复和资源回收。tmpfs 准备不计入请求时延，不能证明普通磁盘冷启动已修复，也不证明动态 QoS 改善业务延迟。

## 动态收益的探索对照

沿用现有实验入口，不新增脚本：

```bash
make build
AGENTBOX_TEST_IMAGE_ROOTFS=1 python3 scripts/container.py static --platform kvm --comparison dynamic --duration 15 --repetitions 1
```

每组包含三个各 15 秒的阶段，固定 1 CPU、固定 2 CPU 与 AIMD 1～2 CPU 使用同一工作量、5/60/5 req/s 核心到达率、两个沙箱客户端。保持静态库存、关闭准入门控，避免把多个策略收益混在一起。预先设定 50ms 核心 SLO，记录拒绝/丢弃、超标秒数、完成量、配额轨迹和恢复；一轮结果不支持显著性推断。2026-10-21 实测动态策略没有守住 SLO，也没有支配固定 1 CPU，默认继续关闭，详见 [验收记录](validation.md)。

## 可选业务健康约束与额度借用

`-qos-base-cpu` 非零并同时配置 `-qos-business-file` 时启用；默认 0/空路径保留原有 PSI-only AIMD。此模式仍默认关闭，基准额度是部署配置，不能当成已证明的 SLO 保证。

```text
-qos=enforce -qos-min-cpu=0.5 -qos-base-cpu=1 -qos-max-cpu=2
-qos-protected-cgroup=/sys/fs/cgroup/.../core
-qos-business-file=/run/core/agentbox-health.json
```

- 控制器初始化时最多使用 base 额度；仅在业务样本有效健康、PSI 连续低压且沙箱存在带宽需求时逐步增额。
- 业务窗口出现拒绝或 SLO 超标，即使 PSI 很低也收回超出 base 的额度。持续业务恶化时，可按动作间隔继续乘法降低至 min，避免把 base 错当成保护保证。
- 业务或 PSI 样本失效时，在下一次可执行的控制周期收回超出 base 的额度；已低于 base 时保持，禁止恢复。恢复需重新积累有效健康与低压窗口。
- observe 只计算，不写配额。配额执行器故障仍按原有规则报告故障并停止写入，不能保证撤额成功。

由核心服务或可信采集器在 `/run` 的受保护目录中原子替换 JSON 文件；不能由模型、请求参数或任务提供路径。数据格式：

```json
{"sampled_at":"2026-10-21T01:00:00Z","window_ms":1000,"requests":100,"rejected":2,"slo_violations":3}
```

这只是格式示例，时间必须由采集器持续更新。五个字段均必需，计数为该窗口内统计；拒绝与超标允许重叠，但各自不能超过请求数。窗口接受 500～5000ms，样本最多保鲜两个窗口；未来时间、缺字段、null、计数错误、缺文件和陈旧样本都不按健康处理。无请求且有新鲜采集窗口可以表示空闲，不能用永久写死的“全零健康”替代实际采集。

文件上限 4KiB，拒绝最终路径符号链接、非普通文件和允许所有用户写入的文件。采集器应覆盖业务关注的请求延迟、拒绝与错误，并据此产生 SLO 超标计数；AgentBox 不自动推断业务 SLO，也不内置生产指标采集服务。建议使用本机 `/run`，避免控制信号依赖慢数据盘。status 增加 `qos.business` 和 `qos.cpu_control.base_cpu`。

固定 systemd 服务增加 `AGENTBOX_QOS_BASE_CPU`（默认 0）与 `AGENTBOX_QOS_BUSINESS_FILE`（默认 none），必须成对配置；其他 CPU 策略参数仍按 CLI 配置。默认 CPU 下界/上界仍为 .5/4，部署应按实际保护需求选定边界。

真实 cgroup 测试已经验证保守启动、健康增额、低 PSI 下业务拒绝触发撤额；真实运行时验证了健康文件的正常/异常/过期接线。它们是机制验证，不是新模式优于静态方案的性能证明。
