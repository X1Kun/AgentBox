# 单机压力感知 QoS 调度

本阶段把 CPU 预算控制、请求准入和补池门控组成单机功能闭环。库存目标默认固定，可独立启用[动态库存](dynamic-inventory.md)；执行并发和总实例容量仍为静态配置，实例仍一次性使用并完整销毁。本页描述实际实现；CPU 参数及早期验收见 [CPU QoS](cpu-qos.md)。

## 启用方式

| 参数 | 行为 |
| --- | --- |
| `-qos=off` | 静态资源限制与既有 FIFO 调度 |
| `-qos=observe -qos-scheduling` | 计算压力状态和调度建议，实际接单和补池不变 |
| `-qos=observe -qos-cpu-control -qos-scheduling` | 同时预演预算和调度建议 |
| `-qos=enforce` | 原有 CPU 控制，调度门控默认关闭 |
| `-qos=enforce -qos-scheduling` | 执行 CPU 控制和压力调度门控 |

observe/enforce 均要求显式受保护 cgroup 路径。固定服务可配置：

```text
AGENTBOX_QOS=enforce
AGENTBOX_QOS_SCHEDULING=true
AGENTBOX_QOS_PROTECTED_CGROUP=/sys/fs/cgroup/system.slice/mockcore.service
```

替换为真实受保护服务路径。默认关闭，本轮没有更新或启停宿主服务。`AGENTBOX_QOS_SCHEDULING=true` 在 observe 下只预演；`AGENTBOX_QOS_CPU_CONTROL=true` 可另外打开 CPU 预演。

## 共享压力状态

Observer 使用有效的 protected/sandboxes CPU 差分生成一份压力状态。Engine 接收调度建议，CPU 控制器也受同一状态约束；CPU 写入故障不会阻止压力判断和调度门控。I/O/PID 等可选指标缺失不单独触发门控。

| 状态 | 进入条件 | 接单与补池建议 |
| --- | --- | --- |
| normal | 正常，或恢复过渡结束 | 开放接单，按原目标补池 |
| pressured | CPU 压力高于高阈值，持续达到确认时间 | 拒绝新任务，暂停新的创建预留 |
| recovering | 从高压状态持续低于低阈值，满足稳定时间 | 恢复接单与补池，CPU 预算按自身需求和冷却规则恢复 |
| unknown | 必需采样缺失、过期、间隔异常或无有效基线 | 保持上次有效门控，不把缺测当成恢复 |

高低阈值、确认和稳定时间复用 CPU 策略配置，默认分别为 20%/10%、1s/2s。滞回区间保持上次有效状态。recovering 在一个动作间隔后转入 normal；重新持续高压仍可再次关闭。

初始 unknown 维持原有静态有界行为。warm 启动先取得最小 READY 实例，再启动观测，避免“等待观测才能补池、等待实例才能启动观测”的循环依赖。已有高压限制则必须等有效低压样本持续达标才能解除。

暂停后沙箱 CPU 用量可能归零，但只要受保护服务仍高压，调度限制不会因此自动解除。CPU 降配额额外要求沙箱确实消耗 CPU；恢复额外要求有预算需求。这些执行条件与节点压力状态分别表达。

## 实际请求行为

- cold 在取得执行名额前检查门控，warm 在入口和最终领取实例/名额的位置检查。
- 高压下新请求返回 HTTP 503，`reason=qos_pressure`，沿用容量拒绝的错误映射。
- 尚未领取实例的排队请求被唤醒并拒绝，释放队列数量与字节预算，报告真实排队耗时。
- 排队者记录门控关闭的代次：即使关闭后立即恢复，也不会漏掉其经历过的关闭事件。
- 已领取实例和名额的任务继续执行，保留原有超时、取消与清理机制；压力门控本身不取消它们。

临时压力不把 Engine 标为损坏。`/readyz` 继续表达引擎健康、已准备且未关闭，不能据此认定当前请求会被放行。消费者应同时检查实际 admission 状态或执行接口的 503 原因。

## 补池与关闭规则

创建 worker 在同一把池锁下检查暂停状态、预留创建容量。暂停后等待已有变更通知，不循环空转。已经预留的创建事务允许完成，并将成功实例收入 READY；恢复后唤醒 worker，继续补足原库存目标。

不通过取消 runsc 元数据事务、销毁现有空闲库存或修改库存目标实现暂停。创建、空闲、领取、运行和销毁仍共同受 MaxSandboxes 约束。

关闭和清理失败优先。停止或失效的池不会被恢复通知重新打开，关闭会唤醒暂停中的 worker 并完成资源回收。锁顺序保持 Engine → pool，等待 worker/任务退出时不持有 Engine 锁；策略回调不在 Observer 锁内执行。

## 状态和计数

鉴权的 `GET /v1/status` 分别暴露：

| 路径 | 含义 |
| --- | --- |
| `qos.pressure` | state、effective_state、reason、建议接单/补池状态、采样序号和时间 |
| `qos.scheduling` | 是否启用/实际执行、实际 admission_open/refill_paused、累计 pressure_rejections |
| `qos.cpu_control` | CPU 控制器自身状态、预算、动作和执行器错误 |
| 顶层 `scheduling.queue_pressure_rejected` | warm 排队者因压力拒绝的驻留分布 |

建议与实际状态分开，observe 下不会把建议当成已执行。unknown 报告保留上次 effective_state。过期的缓存报告显示 unknown，但不通过 HTTP 查询改变任何门控。

pressure_rejections 包含立即拒绝和排队后拒绝；queue_pressure_rejected 只统计真实入队者。池快照内守恒关系更新为：

```text
queue_entered = queue_assigned.count + queue_canceled.count
              + queue_timed_out.count + queue_unavailable.count
              + queue_pressure_rejected.count + queued
```

压力拒绝不计入 timeout、cleanup failure 或 infrastructure error。各组件快照不是跨组件原子事务，比较计数时需匹配范围和时间。

## 功能验证

`make check build` 覆盖配置、状态机、HTTP 错误映射与 race 回归。专项测试覆盖压力确认、滞回、未知保持、恢复、CPU 执行器故障隔离、队列关闭/立即重开、在途任务与创建不取消、补池恢复，以及关闭优先。

```bash
bash scripts/qos-container.sh kvm
```

新增集成用例在私有容器中，把可信测试进程限制在受保护 cgroup 的 0.05 CPU 带宽，产生真实 CPU PSI。验证 observe 不改变执行行为；enforce 拒绝新请求和已有排队者、保留在途任务、暂停补池，并在来源缺失时保持限制、重建来源并低压稳定后恢复。检查执行和关闭后的资源回收，不用于评价业务保护收益。

直接磁盘依赖无法及时启动时，可使用已有 tmpfs 准备模式。为避免再次复制磁盘 rootfs，还提供测试专用选项：

```bash
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  bash scripts/qos-container.sh kvm
```

此方式在容器 tmpfs 准备 runsc，并从同一固定 Python 镜像的 bin/etc/lib/lib64/sbin/usr/var 提取测试 rootfs，建立空的挂载点，排除测试挂载、宿主内核文件系统和任务状态。OCI 中仍只读挂载。用例检查 guest 无法看到测试程序、宿主 runsc 或 cgroup 文件。该环境与正式磁盘 rootfs 的准备路径不同，不用于替代冷启动或性能验收。

动态库存已通过独立开关接入，遵循同一压力门控。动态执行并发、多租户公平性、内存压力准入和多节点路由留到后续实现。

## 本轮验收记录

2026-10-19，基线 `a8b8718` 加 CPU 控制与本轮调度门控的未提交修改。完整 Go race、脚本测试、vet、格式、语法检查和构建通过；压力/门控专项 race 测试重复 20 次通过。

直接磁盘挂载的测试仍在 runsc 创建阶段超时，保留于 `artifacts/qos-scheduling-kvm.log`。最终使用上面的 tmpfs 运行时与固定 Python 镜像提取的测试 rootfs，`bash scripts/qos-container.sh kvm` 的三个测试、六个子用例全部通过，原始记录为 `artifacts/qos-scheduling-kvm-prepared.log`：

| 用例 | 结果 |
| --- | --- |
| 真实 PSI＋observe | 产生高压建议，实际接单和补池不受门控，任务执行和清理成功 |
| 真实 PSI＋enforce | 新请求和排队请求被压力拒绝，在途任务完成；补池暂停；来源缺失时保持限制；来源重建并低压稳定后恢复执行 |
| 真实 cgroup＋observe | 确定性压力输入未修改 cpu.max |
| 真实 cgroup＋enforce | 配额写入及读回成功 |
| 真实冷执行＋observe | 预算预演、Python 输出、五阶段清理和关闭残留检查通过 |
| 真实冷执行＋enforce | 启动预算上界、Python 输出、五阶段清理和关闭残留检查通过 |

日志中同一采样序号 6 记录了压力门控关闭和 CPU 预算下降；后续采样记录了低压后的接单/补池恢复及预算增加。它验证真实 PSI 经策略到执行器的功能通路；受保护组自身的低配额是测试压力源，不构成沙箱噪声因果或业务延迟改善的证据。没有运行长时间性能压测、更新宿主服务或执行 Git 写命令。
