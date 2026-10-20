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

observe/enforce 都需显式指定受保护服务：`-qos-protected-cgroup=/sys/fs/cgroup/...`。该路径不得与沙箱子树相互包含。systemd 配置示例：

```ini
AGENTBOX_QOS=enforce
AGENTBOX_QOS_SCHEDULING=true
AGENTBOX_QOS_PROTECTED_CGROUP=/sys/fs/cgroup/system.slice/mockcore.service
```

预演可改为 observe，并设置 `AGENTBOX_QOS_CPU_CONTROL=true`。模式、两个开关默认分别为 off、false、false。自定义策略参数使用 daemon CLI；部署与更新见 [systemd](systemd-deployment.md)。

<a id="cpu"></a>
## CPU 预算控制

`internal/qos/policy.go` 决策，controller 协调采样和执行器，`internal/cgroup/cpu.go` 只修改 Manager 拥有的沙箱父级。

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

- cold 在取得执行名额前检查；warm 在入口与最终领取处检查。
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

```bash
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py qos --platform kvm
```

验收使用私有容器、6 CPU/5GiB/2304 tasks，区别于静态对照的外层 2 CPU。覆盖确定性压力下的配额写入/读回，以及真实 CPU PSI 下的准入、排队拒绝、补池暂停、缺测保持、稳定恢复和资源回收。tmpfs 准备不计入请求时延，不能证明普通磁盘冷启动已修复，也不证明动态 QoS 改善业务延迟。
