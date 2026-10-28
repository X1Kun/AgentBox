# AgentBox

面向 AI Agent 的隔离会话与受控批任务项目。目标在同一 Kubernetes 平台上，用 **Agent Sandbox 管理会话和固定预热池，Job＋Kueue 管理批任务，containerd＋gVisor 提供运行与隔离**。Go API/MCP 负责产品适配，首个研发点是保护交互体验的健康感知批任务 AdmissionCheck。

**继续演进现有项目，已决定退役 agentboxd。**会话可执行多次命令，批任务按有限输入完成；不要求每次工具调用都创建 Job。当前仍保留可构建的单机旧实现，现已提供上游冷会话、固定预热池与 Go SDK 验收入口，新产品接口和实际退役尚未完成。

## 当前代码与迁移状态

已实现独立 kind 功能实验：固定版本预检、两节点集群、可信普通 Python Job smoke、gVisor 运行时验收、Calico 网络策略 profile 和受归属限制的清理。

```bash
python3 scripts/platform_lab.py create --dry-run
make platform-preflight
make platform-create
make platform-smoke
make platform-delete
```

先按[环境与工具要求](docs/operations.md#platform-lab)准备工具。普通 smoke 不依赖 agentboxd，也不验证 gVisor。Agent Sandbox 冷会话另有独立入口；Kueue 与新产品 API 尚未接入。已有闭环证据见[验证指南](docs/validation.md#platform-bootstrap)。

gVisor 运行时使用独立的 `make platform-gvisor-install` / `make platform-gvisor-test` 入口；包准备、运行方式和验收边界见[运维指南](docs/operations.md#gvisor-runtime)。它只验证可信探针，不表示完整网络隔离或沙箱产品已经交付。

网络策略使用独立的 `platform-network-create/install/test` 入口，固定 Calico VXLAN，验证默认拒绝、定向放行及撤销；需要新建该 profile，不能覆盖原 kindnet 集群。步骤与边界见[运维指南](docs/operations.md#network-policy)。

上游冷会话通过 `platform-session-build/install/test` 构建、安装与验收，使用上游 sandboxd 和 Go SDK，同一会话支持多次命令及文件写读；13 项真实行为检查与清理已通过。步骤与边界见[运维指南](docs/operations.md#cold-sessions)。

固定预热池新增 `make platform-session-warm-test` 入口，用于验证并发领取、补充、配额耗尽和恢复；九项真实行为检查与资源清理已通过，见[验收记录](docs/validation.md#warm-pool)。库存目标与总 Pod 配额分别声明，见[操作说明](docs/operations.md#warm-pool)。当前只做行为验证，不进行压测。

后续顺序：补齐会话故障行为 → Go API/MCP 与 Daemon 退役 → 图片批任务/Kueue → 健康准入对照。只有在途竞争成为明确瓶颈时，再评价 Koordinator。阶段与门槛见[路线图](docs/roadmap.md)。

Go 用于产品服务和自研 Controller；Python 用于环境验收、实验分析及执行负载。当前检查仍覆盖尚未移除的旧代码，不能通过下列命令启动目标平台：

```bash
make build
make check
```

需要 Linux、Go 1.26+；项目固定 `toolchain go1.27.1`，首次构建需准备相应工具链和上游 SDK 依赖；旧运行时复现另需 runsc/rootfs、cgroup v2 与权限。旧 systemd 操作只用于已有部署收尾或历史复现，见[运维指南](docs/operations.md#retirement)。普通回归不调用付费模型。

## 文档导航

| 想了解什么 | 入口 |
| --- | --- |
| 目标架构、会话/批任务边界和选型 | [DESIGN](DESIGN.md) |
| 开发顺序、范围与验收门槛 | [路线图](docs/roadmap.md) |
| 拟议新契约与当前旧 HTTP/MCP 用法 | [使用指南](docs/usage.md) |
| 平台入口、部署计划、排障与退役 | [运维指南](docs/operations.md) |
| 待退役 Daemon 的机制与历史经验 | [旧运行时参考](docs/runtime.md) |
| 保护交互体验的批任务准入策略 | [AdmissionCheck](docs/admission-check.md) |
| 已有证据、会话验收与策略实验 | [验证指南](docs/validation.md) |

历史资料：[本地验收](docs/archive/local-validation.md)、[性能理论](docs/archive/performance-theory.md)、[引用索引](docs/archive/performance-references.md)。现有 data/assets 保留原口径，不转写为新平台成绩。

## 交付边界

首版一个 Worker 资源域、一种受控 CPU/Python 模板、固定池规模和一份图片数据集。会话保留文件不等于保留 Python 内存变量；批任务采用显式提交/查询/取消与产物校验。旧同步 API、Warm 时延和取消语义不作等价迁移，切换后不自动回落 agentboxd。

第一轮比较仅会话、会话＋静态批配额、相同预算＋健康准入；同时报告交互延迟、批任务有效产出、等待和预热成本。新运行路径、生产稳定性和策略收益均需独立验证；历史普通磁盘故障不因换平台就视为解决。
