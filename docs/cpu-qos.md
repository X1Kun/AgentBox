# CPU QoS 闭环

本阶段交付单机 CPU 资源控制的功能闭环：读取已有 PSI 快照、作出有界 AIMD 决策、预演或写入沙箱父级 `cpu.max`、读回确认，并通过状态和日志报告结果。目标是先完成可以使用和验收的功能；没有开展参数寻优，也不承诺固定恢复时间或收益。

压力状态、请求准入与补池门控现已接入，见 [单机压力 QoS 调度](qos-scheduling.md)。CPU 写入与调度门控分别启用，观察模式均只预演。

## 使用模式

| 启动配置 | 行为 |
| --- | --- |
| `-qos=off` | 默认，关闭观测和动态控制，使用静态限制 |
| `-qos=observe` | 保留原有只读观察行为 |
| `-qos=observe -qos-cpu-control` | 在内存中预演 CPU 预算变化，实际配额不变 |
| `-qos=enforce` | 自动开启 CPU 策略，写入并读回沙箱父级配额 |

observe/enforce 均要求显式指定 `-qos-protected-cgroup=/sys/fs/cgroup/...`，且不得与沙箱子树相互包含。保持原有 root、委派 cgroup 和运行时前置条件；不能用控制器绕过静态隔离要求。

在已部署且完成委派的服务配置中，可以设置：

```text
AGENTBOX_QOS=enforce
AGENTBOX_QOS_PROTECTED_CGROUP=/sys/fs/cgroup/system.slice/mockcore.service
```

路径需要对应实际受保护服务。预演使用 `AGENTBOX_QOS=observe` 和 `AGENTBOX_QOS_CPU_CONTROL=true`。默认 `off/false`。本轮没有安装、更新或启停宿主固定服务；更新后 unit 才具备新参数。

## 实现路径

```text
Observer 的新一轮采样
    → 检查 protected / sandboxes 的有效 CPU 差分
    → AIMD 决策器
    → observe：提交虚拟预算
      enforce：cgroup 写入 → 读回确认 → 提交实际预算
    → 缓存状态与结构化动作日志
```

`internal/qos/policy.go` 只处理参数、时间和状态，没有文件 I/O 或 goroutine。`controller.go` 协调快照、策略和执行器；`internal/cgroup/cpu.go` 只修改 Manager 所拥有的沙箱父级。入口、HTTP、pool 和 session 仍负责原有职责。

## 当前规则和默认参数

| 参数 | 默认值 | 命令行参数 |
| --- | --- | --- |
| CPU 下界 / 上界 | 0.5 / 4 核等效带宽 | `-qos-min-cpu` / `-qos-max-cpu` |
| 低 / 高压力阈值 | protected CPU PSI 10% / 20% | `-qos-cpu-low` / `-qos-cpu-high` |
| 压力确认与最小动作间隔 | 1s | `-qos-control-interval` |
| 恢复前连续低压时间 | 2s | `-qos-stable-for` |
| 乘法下降系数 | 0.8 | `-qos-decrease-factor` |
| 加法恢复速率 | 0.25 核/秒 | `-qos-increase-cpu` |
| CPU bandwidth 周期 | 固定 20ms | 控制器不修改周期 |

这些是功能初版的配置起点，没有经性能调参确认最优。动作间隔不得短于观察采样周期。固定服务目前使用默认策略参数，模式和预演开关可通过上述环境变量配置；自定义策略参数使用 daemon 命令行。

高压必须持续达到确认时间，且沙箱确实消耗 CPU，才降低预算；每次下降不能突破最小预算或动作间隔。低压必须持续满足稳定时间，并且沙箱有预算需求（已节流或用量接近当前预算），才恢复。处于滞回区间或没有需求时保持。恢复按实际经过时间计算，单次计入的时间至多为两个动作间隔，避免长时间空闲后突然增加大量预算。

系统/沙箱自身的 PSI 不作为连续减配额的直接触发源。CPU 压力信号用于保护候选判断，不表示已经证明某个邻居是根因。低频 I/O/PID 缺失不会单独阻止有效的 CPU 决策。

## 边界与失败行为

- 必需来源缺失、首次基线、计数回退、采样过期或间隔异常时保持预算，并重建连续高/低压判断，不把缺测当低压。
- 控制器识别重复采样序号；序号在该进程生命周期内递增。
- 写入只允许固定 20ms 周期和静态上限以内的有限 quota，不能超过原有 4 CPU 上限。以微秒取整后仍遵守用户配置的上下界。
- enforce 的最大预算若低于初始静态预算，在启动控制器时先收紧到该配置上界；observe 不执行该收紧。初始预算低于配置下界时拒绝初始化。
- 发现外部配额变更、读取失败、写入失败或读回不一致，控制器进入锁定的 degraded，停止后续自动写入，需要排除原因并重启。不能将建议值记录为实际生效值；无法确认时 actual_cpu 为 null。
- 动态控制降级不自动把 Engine 标为不可接单；原有静态资源限制和清理失败停止接单机制继续工作。关键采样恢复可解除采样降级，执行器故障则不会自动重试写入。
- 停止时不主动放宽配额；保留最后预算完成回收。新 Engine 初始化仍建立静态父级约束，然后初始化所选策略。

预演在同一组真实观测上演化虚拟预算，可以验证动作规则；它不模拟新预算对后续压力的影响，不能用预演证明业务收益。

## 接口和日志

鉴权的 `GET /v1/status` 在启用 CPU 策略时增加 `qos.cpu_control`，包括状态、原因、动作、采样序号、实际/建议 CPU、applied、决策/变化/写入失败计数，以及生效配置。observe 下 changes 是虚拟预算变化，enforce 下是确认写入后的变化，applied 只表示对应那次采样发生了成功写入。

结构化日志记录预算动作及失败。runtime 的必要诊断增加 create/start/state/delete 操作开始、PID、结束和耗时，记录上下文超时但不记录任务代码、输入、环境或 guest 输出。池启动失败保留创建与清理错误链。没有加入高开销系统跟踪。

`scripts/observe.py` 可以记录 observe/enforce 两种模式，完整状态快照包含预算和动作；该工具不自动评价策略优劣。

## 功能验收

```bash
make check build
bash scripts/qos-container.sh kvm
```

验收在独立私有 cgroup/PID/网络容器中运行，外层限制 6 CPU、5GiB 和 2304 个任务，使用 Docker init 回收进程。它是功能验收环境，不是此前外层 2 CPU 的性能实验条件。

测试包括确定性压力样本驱动真实 `cpu.max`，验证 observe 不写和 enforce 写入/读回；再启动真实 Engine，检查两种模式的状态、配置上界、Python 执行、五阶段清理和关闭残留。确定性压力测试验证控制功能，不证明真实 PSI 触发的业务保护收益。

如果磁盘读取影响运行时初始化，可显式选择测试专用准备方式：

```bash
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 bash scripts/qos-container.sh kvm
```

此时在容器内两个各限 512MiB 的 tmpfs 中准备运行时二进制与可信 rootfs，保留 sidecar 和文件权限布局；rootfs 在 OCI 沙箱内仍只读。准备在 daemon 启动前进行，不计入原有 10 秒创建/5 秒清理预算。它不更改宿主部署，也不证明磁盘启动路径已经修复。

本阶段不调整 FIFO、执行并发、库存目标或内存限制。压力准入与补池暂停/恢复已通过独立开关接入；动态库存与多租户公平性仍为后续功能。

## 本轮验证记录

2026-10-19，代码基线 `a8b8718` 加本轮工作区修改。`make check build` 通过 Go race、Python 部署/实验测试、benchmark summary、vet、格式和语法检查；最后追加的采样/动作周期一致性校验也通过 QoS race 测试。

真实 cgroup 测试使用确定性压力输入，在 observe 下保持 `80000 20000`，在 enforce 下确认写入 `64000 20000`。这是控制接口的功能验证，没有宣称真实业务压力被消除。

直接只读挂载磁盘依赖时，两种模式均在 runsc create 的 10 秒预算内未完成，随后 delete 也超时。只准备运行时二进制后，create/start/delete 已完成，但磁盘 rootfs 上 Python 仍未在 5 秒执行预算内完成。上述失败保留了创建与清理阶段信息，不通过放宽预算改判成功。

使用 `AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 bash scripts/qos-container.sh kvm`，在依赖准备结束后通过全部四个子用例：真实配额 observe/enforce，以及真实 Engine 的 observe/enforce Python 执行。验证实际/建议预算状态、启动配置上界、stdout、五个清理阶段、清理完成和关闭后无 bundle/cgroup 残留。平台为 KVM，内核 `6.8.0-138-generic`，runsc 使用项目固定 `release-20261005.0`。

原始日志为本机被忽略的 `artifacts/qos-kvm-prepared.log`。这一结果证明指定准备条件下的功能链路可用，不证明磁盘冷启动故障已修复，也不验证真实 PSI 反馈的业务保护收益。宿主固定服务未更新，未运行长时间性能压测或执行 Git 写命令。
