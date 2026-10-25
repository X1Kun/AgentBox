# AgentBox

面向 AI Agent 的受控任务执行项目。目标由 Kubernetes Job、Kueue、containerd/gVisor 承担任务生命周期、队列和运行管理，AgentBox 聚焦 API/MCP 任务适配与在线健康感知 AdmissionCheck；后续评价 Koordinator 的节点压制。

**已决定退役 agentboxd，目标只保留成熟平台执行链路。**当前源码仍是可构建的单机 Python 旧实现，包含 Cold/Warm、FIFO、清理和 Gateway/MCP；新平台、新接口及实际退役尚未完成。旧实现功能冻结，保留迁移期收尾和历史复现，不再规划长期双后端。

## 当前代码与迁移状态

下列命令只构建和检查尚未移除的旧实现，不能启动目标平台。需要 Linux、Go 1.25+；旧运行时复现另需 runsc、可信 Python rootfs、cgroups v2 委派及对应权限。

```bash
make build
make check
```

新平台实施与旧链路退出条件见[路线图](docs/roadmap.md#retirement)。旧 systemd 操作只用于已有部署维护、收尾或复现，见[运维指南](docs/operations.md)；旧 HTTP/MCP 契约见[使用指南](docs/usage.md)。验证范围见[验证指南](docs/validation.md)。普通回归不调用付费模型。

## 文档导航

| 想了解什么 | 入口 |
| --- | --- |
| 总体架构、组件分工和关键取舍 | [DESIGN](DESIGN.md) |
| 当前阶段、下一步交付和退出条件 | [路线图](docs/roadmap.md) |
| 旧 HTTP/MCP 参考与新接口迁移边界 | [使用指南](docs/usage.md) |
| 退役操作、旧配置与部署排障 | [运维指南](docs/operations.md) |
| 待退役 Daemon 的原理与历史经验 | [旧运行时参考](docs/runtime.md) |
| 核心健康准入扩展的状态与故障设计 | [AdmissionCheck](docs/admission-check.md) |
| 测试入口、证据摘要和平台实验协议 | [验证指南](docs/validation.md) |

历史资料按需查阅：[详细本地验收](docs/archive/local-validation.md)、[性能理论](docs/archive/performance-theory.md)、[引用索引](docs/archive/performance-references.md)。结构化数据与图表保留在 docs/data 和 docs/assets。

## 状态与使用边界

旧同步任务采用一次性实例、默认禁网/只读根/非 root，结果不确定时不自动重放；其配置和运行历史仍可核对。新平台将任务实际放在 Job Pod 内执行，并独立验证隔离、重试、结果与清理，不承诺旧同步 API、Warm 时延或所有行为兼容。切换后不自动回落到 agentboxd。

现有结果支持限定场景下的预热等待改善与静态资源保护，不证明动态策略普遍更优。普通磁盘稳定性仍有缺口；Kubernetes 后端、多节点性能和生产就绪尚未验收。具体版本、数字与限制仅在验证文档维护。
