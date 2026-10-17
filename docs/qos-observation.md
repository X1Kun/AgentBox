# QoS 第一阶段：观察资源争用

当前实现读取 PSI 与 cgroup 统计，提供固定工作量的 Mock Core Service，并记录其请求延迟。它回答“沙箱负载出现时，核心服务是否在等待资源，以及采集信号是否可信”。自动调整 `cpu.max`、AIMD、内存准入与补池联动留到后续。

已运行的检查、真实平台验证和数据见 [本阶段验收记录](qos-observation-validation.md)。

## 模式与部署配置

| CLI 参数 | systemd 环境变量 | 默认值 |
| --- | --- | --- |
| `-qos` | `AGENTBOX_QOS` | `off` |
| `-qos-interval` | `AGENTBOX_QOS_INTERVAL` | `100ms` |
| `-qos-protected-cgroup` | `AGENTBOX_QOS_PROTECTED_CGROUP` | `none` |

`off` 不启动采样 goroutine，不读取观测文件，状态响应省略 `qos` 字段。`observe` 启动一个采样 goroutine，只读取资源文件；现有静态配额和预热池调度继续生效。`enforce` 尚未实现，配置时拒绝启动。

采样间隔范围为 50ms～1s。受保护 cgroup 必须显式配置为 `/sys/fs/cgroup/` 下的绝对路径，并与沙箱父级子树分开，不能相互包含。服务尚未启动、指标文件缺失或读取失败时，观测报告降级，执行服务继续接单；不能把该状态解释成零压力。

更新固定服务时沿用 [部署说明](systemd-deployment.md) 的停止、更新和启动流程，更新后的 unit 支持以下 `/etc/agentbox.env` 配置。路径只是示例，必须对应实际独立服务的 cgroup：

```ini
AGENTBOX_QOS=observe
AGENTBOX_QOS_INTERVAL=100ms
AGENTBOX_QOS_PROTECTED_CGROUP=/sys/fs/cgroup/system.slice/agentbox-mockcore.service
```

Mock Core 是实验工具，`make build` 生成 `bin/mockcore`；现有安装脚本只安装 AgentBox，不会自动部署或启动 Mock Core。下文的容器实验会建立独立 cgroup 并启动两个进程，不修改宿主固定服务。

## 信号与数据有效性

鉴权后的 `GET /v1/status` 在观察模式下增加 `qos`，包含三个来源：

| 来源 | 文件 | 用途 |
| --- | --- | --- |
| `host` | `/proc/pressure/{cpu,memory}` | 系统争用背景 |
| `sandboxes` | 沙箱父级的压力及资源统计文件 | 沙箱成本、CPU 节流、内存水位与事件 |
| `protected` | 指定核心服务 cgroup 的相同文件 | 被保护服务的资源等待 |

cgroup 读取 `cpu.pressure`、`memory.pressure`、`cpu.stat`、`cpu.max`、`memory.current`、`memory.high`、`memory.max` 和 `memory.events`。保留原始累计值与当前限制；`max` 使用 `{"unlimited":true,"value":0}` 表达，数值零是 `{"unlimited":false,"value":0}`。

短窗口压力计算为 `100 × Δtotal_usec / 实际间隔微秒数`，间隔由 Go 单调时间计算，不假定每次采样准时发生。`avg10/60/300` 只用于趋势展示。CPU 用量为 `Δusage_usec / 实际间隔微秒数`，单位为 CPU 核等效用量，可以超过 1；节流周期数、节流时间和内存事件报告窗口增量，不能把节流时间直接当作业务延迟。

以下情况不会产生有效窗口增量：

- 首次采样或读取失败后的首次恢复：`baseline=initial`。
- 目录设备号或 inode 变化：`source_changed`；读取前后目录变化则整次来源采集失败。
- 累计计数器回退：`counter_reset`。
- 非正时间间隔：`invalid_interval`；间隔超过配置周期的五倍：`sample_gap`。
- 最新快照年龄或整轮读取耗时超过配置周期的三倍，或者采样已停止：`stale=true`，可用来源标记 `baseline=stale`。

这些情况下 `interval_ms`、`stall_percent` 和 cgroup `delta` 为 `null`。来源不可用时 `available=false`、`sample=null`，并包含错误信息；有效的零压力则明确返回数字 `0`。系统级 CPU `full` 无有效定义，始终返回 `null`。`state=observing` 表示来源可读取，首次采样仍可能处于建立基线阶段；消费方还需要检查 `stale`、来源可用性和窗口字段。

多个文件顺序读取，不构成原子快照；短窗口比例是近似值，采样偏差可能造成瞬时异常比例，当前保留原值供分析。每个文件最多读取 64KiB，HTTP 只复制缓存快照，不触发同步采集。观察器在 Engine 启动后运行，关闭时取消并等待其退出。

接口来源： [Linux 6.8 PSI](https://docs.kernel.org/6.8/accounting/psi.html)、[cgroups v2](https://docs.kernel.org/6.8/admin-guide/cgroup-v2.html)。

## 固定工作量与实验入口

Mock Core 默认监听 `127.0.0.1:8081`。`GET /work` 执行固定 50,000 次 SHA-256 链计算，返回工作次数、确定性的 checksum 和处理耗时；`GET /healthz` 检查存活。工作量由启动参数固定，请求不能修改。默认最多四个并发，多余请求返回 503，单次计算有五秒截止时间。它是无鉴权的本机实验服务，只允许绑定 loopback IP。

已经准备 runsc、rootfs 和 Docker 后：

```bash
make build
bash scripts/observe-container.sh kvm
bash scripts/observe-container.sh systrap
```

每次生成新的 `artifacts/observe-<platform>-*/`。测试使用私有 PID/cgroup/网络命名空间、只读项目挂载、受限的结果目录写入；外层限制 2 CPU、5GiB 内存、2304 个任务。测试控制器创建并配置 cgroup，AgentBox 观察器只读。容器使用 privileged 模式，原因及适用边界与 [已有集成测试](../README.md#独立容器内验收) 相同。

容器依次运行两组，每组五秒、每秒提交 20 个核心服务请求：

1. `idle-pool`：预热库存与观察器运行，没有沙箱任务。
2. `sandbox-load`：两个客户端持续提交两秒超时的 Python 死循环任务，同时记录核心服务响应。

核心服务负载按计划时间发送，记录计划和实际发送时刻；客户端队列与运行总数最多 32，超过时记录丢弃。每次请求使用新 HTTP 连接。成功延迟、503 拒绝、客户端丢弃、传输错误分别统计。沙箱的预期结果是 `timeout` 且清理完成；拒绝和异常结果单独计数，不自动重放不确定请求。

停止提交后会等待已发出的请求完成；状态记录持续到最后一个沙箱请求收尾，因此记录时间轴可能超过配置的负载发送窗口。

输出包括：

- `*-core.csv`：逐请求端到端耗时、服务端工作耗时、checksum、拒绝与错误。
- `*-sandboxes.csv`：沙箱执行 ID、任务状态、清理状态和耗时。
- `*-observations.jsonl`：每 100ms 轮询的完整状态快照及读取错误；这是最新快照的轮询记录，不保证保存每个内部采样点。
- `summary.json`：配置、初始状态、成功请求分位数、错误/拒绝/丢弃计数、快照过期/缺失/基线数量。
- `validation.json`：内核、平台、二进制哈希、前后资源限制与验收项；另有组件日志。

验收还会移除并重建核心服务 cgroup，检查“明确不可用 → 恢复采样”，检查观测前后静态限制一致，以及退出后没有遗留任务资源。systemd 集成测试另验证 unit 能传递 observe 参数，并验证受保护服务缺失时仍可执行任务。

已有独立服务环境也可运行记录器，token 从环境读取，不写入结果文件：

```bash
python3 scripts/observe.py --output artifacts/observe-manual --duration 30 --rate 20 --sandbox-workers 2
```

此命令要求已设置 `AGENTBOX_API_TOKEN`，AgentBox 已处于 observe 模式，两个服务监听默认端口；可用 `--agent-url` 和 `--core-url` 指定其他 loopback 端口。

## 结果边界与下一步

这轮实验用于验收采集链路和负载工具。五秒、每组约一百个请求的 P99 只是样本统计，不能据此承诺稳定尾延迟。`idle-pool` 包含库存和观察开销，不能当作核心服务独占基线；两个阶段均处于 observe 模式，也没有测量观察器开销差值。容器内 `/proc/pressure` 反映宿主系统背景，不能看作私有容器压力。外层 2 CPU 限制也会影响各子 cgroup，结果不能直接推广为裸机表现。

下一步先补“核心服务独占、沙箱仅硬上限、静态保护”的可重复对照与更长窗口，再实现有上下界、冷却时间和审计记录的独立 CPU 配额控制。准入和补池联动随后分别验证。
