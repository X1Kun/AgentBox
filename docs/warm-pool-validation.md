# 预热池阶段验收与基准记录

最近更新：2026-10-16。实现语义和复现命令见 [预热池文档](warm-pool.md)。以下数据来自独立容器，不是宿主固定服务的裸机基准。

## 2026-10-16 职责边界重构验收

本轮完成状态协议与池统计分离、实例资源销毁与任务报告分离，以及 session 输出能力封装。外部 JSON 字段、退出状态、资源限制和关闭预算保持原有语义。

- `make check build` 通过，包括 race、vet、脚本与部署回归；基准统计代码也完成编译和单元回归，本轮未重跑性能基准。
- 新增回归检查完整状态 JSON 字段及 cold/warm 健康状态映射；没有 session 的部分实例可独立销毁；删除失败仍关闭输出流、保留输出与 intent 并使引擎降级；最终输出截断状态保持正确。
- KVM 与 systrap 的 cold/warm 真实执行套件均通过，包括创建中关闭、输出超限、OOM、取消、清理失败和重启恢复。
- systemd KVM warm 的 17 项执行检查和完整安装/更新/回退/卸载重装生命周期通过；8 个实例、每次删除延迟 2 秒的停止耗时为 4.42 秒，无记录残留。
- 本轮没有重复 systrap 的 systemd 验收，其历史结果保留在下一节；宿主固定服务未替换。

systemd 记录时间：2026-10-16T09:25:17.911610+00:00；安装 manifest 与当前构建核对一致的 SHA-256：

`d05553b0bbb6e3d0c189ad623c739b6ddfef3d91774c7d98419e95ba85189527`

原始证据：本机 `artifacts/runtime-kvm-boundaries.log`、`runtime-systrap-boundaries.log`、`systemd-kvm-warm-boundaries.log/.json`。这些文件由 Git 忽略。

## 2026-10-15 关闭修复验收

本节保留关闭修复时的通过记录。本轮职责重构的最新结果见上节；旧日志不能代替新版本验收。该轮构建与两组 systemd 容器安装 manifest 的 SHA-256 已核对一致：

`81142b18ebded21a165021e5e3723a4c6c349694853c056fc2b608c7aa6585ed`

- `make check` 通过：race、vet、Go 格式、脚本语法与部署工具回归均通过。
- 并行清理回归：8 个实例最多四路同时销毁；清理完成前仍计入容量；注入两次删除失败，其他实例继续回收，两个错误均返回，失败实例保留记录。
- 创建取消回归：把 create/start 的元数据写入暂停在半份 JSON 时取消父上下文，验证当前操作继续完成写入、取消仍返回给调用者，create 被取消后不再启动下一阶段。
- 真实 KVM 与 systrap 执行套件均通过：每个平台 cold 17 项、warm 原有 17 项，另加 4 项预热专用测试，其中创建过程中关闭连续执行三轮。
- 真实 systemd 252 / warm：KVM 与 systrap 各 17 项执行检查及完整部署生命周期通过，覆盖正常停止无残留、执行中停止、主进程 SIGKILL 后恢复、更新、回退、保留数据卸载、重装与显式 purge。

串行销毁的回归场景采用真实 runsc：填满 8 个 READY 实例，在包装命令中给每次 delete 增加 2 秒延迟。串行需要至少 16 秒，测试要求完整正常停止小于 12 秒，且 Result=success、bundles 无残留。

| 平台 | 生命周期记录时间（UTC） | 8 实例慢删除停止耗时 |
|---|---|---:|
| kvm | 2026-10-15T05:40:28.173077+00:00 | 4.19 秒 |
| systrap | 2026-10-15T05:45:58.086964+00:00 | 4.07 秒 |

原始证据使用单独的文件名，避免覆盖或误读旧结果：`artifacts/runtime-kvm-shutdown.log`、`runtime-systrap-shutdown.log`、`systemd-kvm-warm-shutdown.log/.json`、`systemd-systrap-warm-shutdown.log/.json`。这些本机文件由 Git 忽略，本节保留可提交摘要。

根因与修复：空闲库存改为四路清理；HTTP drain 与引擎关闭同时开始，分别保留 20 秒 HTTP 预算与 45 秒 systemd 停止预算。正常取消不再中断已经开始的 runsc create/start 写入。生命周期长任务先等待空闲库存，若请求提前返回则立即报告 HTTP 状态，避免把合法的 503 误报成“等待运行沙箱超时”。

本轮崩溃测试覆盖运行中主进程被 SIGKILL 后的恢复。任意时刻的硬终止仍可能损坏运行时元数据；无法确认清理时继续保留 intent 并拒绝接单，不自动删除损坏状态来绕过错误。宿主固定服务未替换；下文性能数据来自关闭修复前的初版，不代表此次重新运行了性能基准。

## 初版功能与生命周期（历史记录）

- `make check`、`make build` 通过；包含 race、vet、配置/部署回归、队列 FIFO/数量/字节预算/取消/到期、创建与销毁容量、关闭和失败收敛。
- 初版真实 runsc 执行套件：KVM、systrap 各自运行 cold 17 项、warm 原有 17 项，以及 warm 新增 3 项：并发独占与工作目录隔离、排队取消与直接关闭、空闲实例死亡后销毁补池。
- systemd 252 隔离容器：KVM cold、KVM warm、systrap warm 均完成执行、三轮重启、执行中停止、主进程 SIGKILL 后回收与恢复，以及更新/回退/保留数据卸载/重装/显式 purge。
- 正常停止必须 Result=success、进程全部回收且 bundles 无残留；崩溃分支允许失败状态和 intent 残留，但要求进程回收并在重启后修复。
- `KillMode=control-group` 的 SIGTERM 广播曾中断后台 runsc delete；改为 mixed 后，通过正常停止和崩溃兜底验收。
- 宿主现有服务未替换；本阶段新的 warm 模式没有在宿主固定服务上重新验收。原先 systemd 249 / KVM 的 cold 验收记录继续保留在部署文档。

原始功能日志保存在本机被 Git 忽略的 artifacts/runtime-kvm.log、runtime-systrap.log、systemd-kvm-cold.log、systemd-kvm-warm.log 和 systemd-systrap-warm.log。systemd JSON 摘要另存同名 .json；Git 中保留本页摘要。

## 初版冷暖 HTTP 对比（关闭修复前）

记录时间：2026-10-15T05:09:24Z；Linux 6.8.0-111-generic；go1.25.11；runsc release-20261005.0 / KVM。

复现：`bash scripts/benchmark-container.sh kvm 60 10`。两种模式使用相同 rootfs digest、实际 HTTP Handler、Hello World 代码、执行并发 4 和静态配额。warm 库存目标 3、创建并发 1、总上限 6、队列 16 / 8 MiB / 100ms。外层 Docker 限制为 6 CPU、5 GiB 内存、2304 host tasks，使用私有 PID/cgroup 与禁网命名空间。

客户端与 HTTP 服务位于同一测试进程，日志输出丢弃。每种模式先完成两次未计入统计的首次探测；inventory/steady 各 60 请求，burst 32 请求，steady 按 10 请求/秒发送。因此 steady 仅约 6 秒，是持续供给的初步观测，不是长时间稳态容量认证。

| 模式 | 场景 | 发起 / 完成 / 拒绝 | 成功 wall P50 | 成功 wall P99 | 错误 |
|---|---|---:|---:|---:|---:|
| cold | inventory | 60 / 60 / 0 | 287.78 ms | 387.23 ms | 0 |
| cold | burst | 32 / 4 / 28 | 418.92 ms | 442.47 ms | 0 |
| cold | steady | 60 / 49 / 11 | 405.68 ms | 497.72 ms | 0 |
| warm | inventory | 60 / 60 / 0 | 156.40 ms | 372.63 ms | 0 |
| warm | burst | 32 / 3 / 29 | 184.40 ms | 207.68 ms | 0 |
| warm | steady | 60 / 34 / 26 | 247.29 ms | 560.30 ms | 0 |

库存充足时，setup P50 从 72.57 ms 降至 10.24 ms；请求 wall P50 从 287.78 ms 降至 156.40 ms。任务执行阶段 P50 仍为 130.61 ms，主要请求路径仍包含 runsc exec、新 Python 进程启动和最终销毁，本轮没有实现几十毫秒的完整任务往返。

warm burst 的采样最低 READY 数为 0，队列最高为 16；warm steady 的最低 READY 数同样为 0。各场景采样总实例数最高为 4，未超过配置的 6。容量硬边界另由并发单元测试和真实运行时测试验证，1ms 采样不是对所有瞬时状态的证明。

当前单个创建 worker 和 100ms 等待预算下，warm steady 完成 34/60，低于 cold 的 49/60。该结果展示了提前创建降低命中延迟与持续补池供给之间的区别，不能据此宣称预热提高了稳态吞吐。后续可比较创建并发 1/2、执行并发及等待预算，并延长测试时间；不根据本轮短测自动修改默认配置。

3 个 READY 实例就绪后的父 cgroup memory.current 单次观测为 99.29 MiB，cold 空闲为 0。该值包含当时 cgroup 的整体收费，不是单实例固定内存成本。

P99 使用成功且确认清理请求的 nearest-rank 统计；拒绝与失败单列。burst 只有少量成功样本，其 P99 接近最大值，不具备生产尾延迟结论。每个请求均写入 CSV，包括 HTTP 拒绝；已核对全部 304 行与六组 summary 的发起、完成、拒绝和错误计数一致。

原始数据目录：本机 `artifacts/benchmark-kvm-jXmIUnDF/`，包含 requests.csv 和 summary.json。该目录被 Git 忽略，避免将本机实验文件混入代码提交。本页保留可提交的环境、口径和结果。

benchmark 二进制 SHA-256：`0892715e9e8c02b338a19a7c331a72cd31d7d3bcc1303671fdd73b924a077d6c`。
runsc SHA-256：`210b437a9cfae51e8f8c9074ed19b8b5e59477178e2e391a18117d8d6b924f7a`。
rootfs：`python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258`。

没有进行 PSI/AIMD 或核心服务延迟保护实验；这些仍属于下一阶段。
