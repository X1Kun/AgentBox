# AgentBox

面向 AI Agent 的隔离会话项目。当前产品链路为 **Agent → stdio MCP v2 → Go Session API → 上游 Agent Sandbox/sandboxd → containerd/gVisor**。会话支持多次 Python 命令和文件读写；每次是新解释器，工作目录跨调用保存。

旧 agentboxd、专用 Gateway、直接宿主 runsc 后端及旧调度/QoS 控制已清退。默认构建、文档和测试统一走新路径。后续批任务计划采用 Job/Kueue，再以静态基线评价健康感知 AdmissionCheck；这些仍不是当前已交付能力。

## 构建与使用

```bash
make build
make check
```

构建产物为 agentbox-api、agentbox-cleanup、agentbox-mcp、agentbox-agent。需要 Linux、Go 1.26+；项目固定 toolchain go1.27.1，依赖和工具链需提前准备。普通检查使用本地模型/API 替身，不调用付费模型或连接 Kubernetes。部署已有 API 后，由调用方配置独立 API 身份和模型凭据，按[使用指南](docs/usage.md#agent)运行 Agent。

Agent 自动创建并绑定一个会话，仅向模型开放查询/执行/文件工具；结束或异常退出时在独立预算内请求关闭并确认。创建恢复使用相同 request_id；未知执行/写入不重放。MCP 子进程只获得 API token，模型和集群凭据不进入 MCP 或执行 Pod。

## 开发平台与真实验收

先按[运维指南](docs/operations.md#platform-lab)准备 kind、工具、缓存、Calico/gVisor 与上游组件。已有健康开发集群可复用：

```bash
make platform-up
make platform-ready
make platform-agent-test
```

platform-up 依赖已准备的锁定归档和镜像 bundle；不隐式下载/更换不匹配版本。platform-agent-test 启动真实 Agent/MCP 子进程与确定性模型替身，验证计算、程序错误修正、文件导出、超时、成功响应丢失不重放，以及三个运行环境的独立回收；只清理本轮临时资源并保留集群。普通 Job、运行时、网络、冷/预热会话、故障、API 和 Claim 保留验收仍有独立入口，见[验证指南](docs/validation.md)。

`agentbox-cleanup` 为独立管理员工具，默认预览，按 closed_at 与保留期核对原 UID/resourceVersion 后清理 Claim，保存持久审计报告。产品 API 不获得元数据删除权限，操作见[记录清理](docs/operations.md#claim-retention)。

## 文档导航

| 内容 | 入口 |
| --- | --- |
| 当前架构与后续批任务/策略边界 | [DESIGN](DESIGN.md) |
| 已完成阶段与后续门槛 | [路线图](docs/roadmap.md) |
| API、MCP v2 与 Agent 参数 | [使用指南](docs/usage.md) |
| 平台部署、清理、升级与退役 | [运维指南](docs/operations.md) |
| 运行时、身份、资源及取消边界 | [运行时](docs/runtime.md) |
| 健康感知批任务准入设计（计划） | [AdmissionCheck](docs/admission-check.md) |
| 行为证据、失败记录与测量计划 | [验证指南](docs/validation.md) |

旧源码、安装与性能实验仅在[历史版本](docs/archive/legacy-runtime.md)复现。data/assets 和原始 artifacts 保持历史口径；不将旧 Warm 数字或 kind 功能结果当作新平台生产稳定性/性能结论。普通磁盘回收问题仍需独立排查。
