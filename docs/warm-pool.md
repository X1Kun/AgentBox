# 一次性预热池与本地调度

AgentBox 支持显式 `cold` 和 `warm` 两种模式，默认保持 `cold`，不自动降级或切换平台。两种模式共用 Python 引导、只读 rootfs、无外部网络、cgroup 配额、结果判定和销毁规则。

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
| `-concurrency` | `AGENTBOX_CONCURRENCY` | 1 | 任务并发，1～4，直到清理完成才释放 |
| `-warm-size` | `AGENTBOX_WARM_SIZE` | 3 | 固定 READY 目标；也作为动态最大值的默认来源 |
| `-warm-dynamic` | `AGENTBOX_WARM_DYNAMIC` | false | 按需求在上下界间调整 READY 目标，参数见[动态库存](dynamic-inventory.md) |
| `-max-sandboxes` | `AGENTBOX_MAX_SANDBOXES` | 6 | 所有创建、空闲、领取、运行、销毁实例的总上限，最多 8 |
| `-create-concurrency` | `AGENTBOX_CREATE_CONCURRENCY` | 1 | 后台创建并发，1～2，不能超过 READY 目标上界 |
| `-queue-size` | `AGENTBOX_QUEUE_SIZE` | 16 | 最多排队请求数，1～128 |
| `-queue-bytes` | `AGENTBOX_QUEUE_BYTES` | 8388608 | 排队请求的解码后文本字节预算，最多 64 MiB |
| `-queue-timeout` | `AGENTBOX_QUEUE_TIMEOUT` | `100ms` | 最大排队时间，1ms～5s |

库存目标和执行并发分别不得超过总实例上限。CLI 接受 Go duration；固定服务配置解析器接受整数 `ms` 或 `s`。以上池与队列参数只影响 warm 模式，cold 保持满载立即拒绝；cold 显式启用动态库存会被拒绝。

字节预算计算 code、stdin、request_id、runtime 和 env 键值的字节长度，不是对 Go 堆内存的精确计量。HTTP 原始 body 另有既有上限。

父级静态约束仍为 4 CPU 等效带宽、memory.high 3 GiB、memory.max 4 GiB；每个实例仍为 1 CPU、memory.high 384 MiB、memory.max 512 MiB、swap 0 和 host pids.max 256。池容量上限不会替代内核配额，也不保证所有实例同时接近单实例硬上限时不触发父级内存限制。

## 调度与故障

领取 READY 实例和取得执行名额在同一把池锁下完成。已有等待者时新请求不能插队；队列限制同时检查数量和字节。取消和到期会移除对应等待者、释放预算并唤醒后继请求。

队列满或到期返回 503；暂时没有 READY 实例时有界等待，不额外启动冷沙箱。排队取消不创建执行记录、不启动任务，返回取消结果时没有 execution_id、cleanup_complete 为 false，表示未取得需要清理的资源。

领取后先检查 runsc state 和 cgroup populated，再注入任务。空闲实例已死亡时，本次请求返回基础设施错误，销毁该实例并补充库存；不自动重放用户请求。当前不做空闲实例周期巡检。

创建失败采用 200ms 退避，连续三次失败后停止补池并使 readiness 失败。清理失败立即停止接单和补池，保留 intent 和已占用容量，等待管理员解决后重启回收。已在创建中的操作允许完成并进入清理。

## 启动、关闭与恢复

`New` 完成检查和历史回收，`Start` 启动后台创建并等待至少一个 READY 实例。主程序给预热启动最多 20 秒；达到最低库存、绑定 HTTP listener 后才发送 systemd READY，剩余库存后台补齐。

readiness 表示初始化完成且未关闭或降级，不表示当前有空闲库存。临时耗尽但可补池时 readiness 仍为 true，接单结果由队列决定。

关闭时 HTTP drain 与 Engine.Close 同时开始。Close 先关接单门、取消引擎生命周期和后续补池，等待创建者收尾与所有已接单请求退出，再以最多 4 路并行销毁未使用的 READY 库存，最后释放状态锁。每个实例只在清理成功后释放容量；个别失败不会中止其他实例的清理，全部错误会汇总返回。运行中的请求即使由直接调用者提供 Background context，也会因引擎关闭而取消。所有资源销毁使用独立有界上下文，主程序会报告库存清理错误；systemd mixed 终止策略在主进程退出或停止超时后终止剩余服务进程，继续提供最后兜底。

runsc 的状态文件使用截断后写入，参见其 [StateFile.SaveLocked 实现](https://github.com/google/gvisor/blob/master/runsc/container/state_file.go)。已经开始的 create/start 操作因此使用独立的 10 秒总预算：普通请求取消或服务关闭时，等待当前操作完成，不启动后续操作或用户代码，再进入销毁。这样避免主动取消在写入中途杀死 CLI。硬超时或外部 SIGKILL 仍可能破坏元数据；无法确认清理时保留记录并拒绝继续接单，不通过直接删除损坏状态伪装成功。

HTTP drain 预算为 20 秒；创建者/运行任务并行收尾按约 18 秒预留，随后最多两批库存清理按约 14 秒预留。systemd `TimeoutStopSec=45` 提供调度余量和最终强制终止边界。这是正常可中断操作的工程预算，不是对不可中断内核 I/O 的实时保证。

进程崩溃后，以 bundles 下的 intent 为恢复依据，清理未用 READY、部分创建和运行中的实例，再建立全新库存。配置所有权不绑定 cold/warm 模式，允许清理后在同一个状态目录切换模式。

## 代码职责

- `api.StatusReport` 定义对外状态响应；HTTP 的 `Executor` 接口明确要求 `Status()`，缺失能力在编译期发现。JSON 字段保持兼容。
- 池提供内部 `poolSnapshot`，不依赖 API 类型；Engine 将快照转换为状态报告，并合并引擎健康状态。
- `destroyInstanceResources` 统一回收容器、cgroup、输出流和实例目录，支持空闲实例及部分创建的实例；不生成任务报告，也不修改池状态。资源清理失败仍使引擎停止接单并保留 intent。
- `finalizeExecution` 调用资源回收，再填写任务输出、耗时、截断标志和最终状态。已领取任务进入 DESTROYING 的时机由执行流程协调；后台库存的状态由池管理。
- session 通过 `outputResult()` 和只读的 `outputLimitExceeded()` 通知提供输出能力，Engine 不直接访问收集器内部字段。

## 观测与时延口径

鉴权的 `GET /v1/status` 返回模式、readiness、库存目标、总上限，以及 creating/idle/leased/running/destroying、队列数量与字节数、创建次数/失败次数/累计耗时和清理失败次数。启用[动态库存](dynamic-inventory.md)时增加目标边界与动作统计，target 为实时目标。cold 不提供池统计。

执行结果新增 `queue_ms`，原有字段保持：

- `setup_ms`：cold 是创建运行环境和准备任务；warm 是领取后的健康检查和任务文件准备。
- `execution_ms`：启动任务并等待结果，仍受 timeout_ms 约束。
- `cleanup_ms`：销毁运行环境、收集最终输出、删除记录。
- `total_ms`：本次引擎调用的排队、准备、执行和清理；不含提前预热与空闲等待，也不含 HTTP 传输和解码。

Usage 覆盖该实例整个 cgroup 生命周期，包括运行时创建及 warm 空闲期，不能据此直接比较纯 Python 用量。宿主 runsc CLI 位于 manager，继续不计入单实例 Usage。

## 系统服务启用

已经安装固定服务时，由管理员更新二进制和 unit，再用编辑器在 `/etc/agentbox.env` 中添加 `AGENTBOX_MODE=warm`，保留原 token、平台和其他配置：

```bash
make build
sudo systemctl stop agentbox.service
sudo bash scripts/install-systemd.sh --update
sudoedit /etc/agentbox.env
sudo systemctl start agentbox.service
mkdir -p artifacts
sudo bash scripts/smoke-host.sh --lifecycle --output "$PWD/artifacts/host-warm-systemd.json"
```

证据路径必须不存在。切回 cold 时只需修改模式并重启。预热实例是正常库存，smoke 只要求已完成任务的 ID 无残留，并检查总容量；停止服务后仍要求进程完全回收。功能用例先等待可领取库存，满载拒绝用例直接发送且不重试；不会把库存耗尽时合法的 503 当成用户代码执行错误。

## 可复现实验

```bash
make check
bash scripts/integration-container.sh kvm
bash scripts/integration-container.sh systrap
make build
bash scripts/integration-systemd-container.sh kvm warm
bash scripts/integration-systemd-container.sh systrap warm
bash scripts/benchmark-container.sh kvm 60 10
```

运行时脚本依次跑 cold/warm 原有执行套件，warm 另测创建过程中关闭、并发独占与新工作目录、排队取消、引擎关闭取消及空闲实例死亡后的回收补池。systemd 脚本的第二参数为 cold/warm，默认 cold。warm 生命周期额外填满 8 个实例，将每次 runsc delete 延迟 2 秒，要求正常停止在 12 秒内完成且无记录残留，直接覆盖串行清理需要至少 16 秒的回归场景。

对比脚本在同一个独立容器中顺序运行两种模式。客户端与使用实际 HTTP Handler 的服务位于同一测试进程，日志写入丢弃设备；测量不含 systemd 启动与外部网络。固定相同 rootfs、runsc、平台、执行并发 4、配额和 Hello World 任务；warm 库存目标为 3，总上限为 6；排除两次未计入分位数的首次探测，但探测失败会使实验失败。每种模式包含：

1. inventory：顺序请求，warm 每次等待库存补到目标再发起；等待补池不计入单次 HTTP 时延。
2. burst：同时发起 32 个请求，保留全部拒绝与失败结果。
3. steady：按固定速率开放式发起请求，不等待上一次完成后才发送。

第二参数是 inventory/steady 各自的请求数，第三参数是 steady 每秒发起数。结果写入新的 `artifacts/benchmark-<platform>-<随机后缀>/`：requests.csv 含每个请求的计划/实际发起时间、HTTP 状态、任务结果、wall 时延和引擎各阶段；summary.json 含版本、校验值、配额、配置、父 cgroup 空闲内存观测、每毫秒采样的库存最低值/总实例最高值/队列最高值和各场景统计，不包含 API token。

成功分位数仅统计成功且确认清理的响应，HTTP 拒绝、用户/基础设施失败单独计数。小样本的 P99 接近最大值，不能据此声称生产尾延迟；inventory 的整体完成速率含补池等待，持续吞吐应看 steady。

持续接单能力同时受执行和创建约束。设执行并发 C、领取至清理完成平均时间 S、后台创建并发 B、平均创建时间 Tc，则稳定接单率需同时满足 λS < C 和 λTc < B；这些是容量约束，不是负载分布、CPU 争抢和资源回收条件下的精确吞吐预测。有限库存只能吸收短时突发。

容器基准包含外层 CPU/内存限制，并使用容器 PID/cgroup 命名空间；不能把其数值直接当作裸机 systemd 服务性能。需要裸机数据时，可在专门委派的独立测试环境中设置 AGENTBOX_BENCHMARK、AGENTBOX_BENCH_OUTPUT、AGENTBOX_RUNSC、AGENTBOX_ROOTFS、AGENTBOX_CGROUP_ROOT、AGENTBOX_PLATFORM 等变量后运行 `go test -tags=integration -run TestColdWarmHTTPBenchmark -count=1 -v ./tests/benchmarks`。不能与正在运行的服务共用委派子树或状态目录。

systemd mixed 的终止顺序依据 [systemd 249 官方说明](https://github.com/systemd/systemd/blob/v249/man/systemd.kill.xml)。真实服务测试要求正常停止结果为 success 且无执行记录残留；主进程崩溃时仍验证整个服务进程集合被回收。测试容器将 `/opt` 放在可执行 tmpfs 内，安装、更新和卸载仍执行完整文件复制和校验。

实际功能验收与冷暖数据见 [本阶段验收记录](warm-pool-validation.md)。

本阶段未加入 PSI/AIMD、解释器常驻注入、自动伸缩、快照恢复或多节点调度，也不承诺固定时延。
