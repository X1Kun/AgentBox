# AgentBox

面向 AI Agent 的隔离会话项目。当前产品链路为 **Agent → stdio MCP v2 → Go Session API → 上游 Agent Sandbox/sandboxd → containerd/gVisor**。会话支持多次 Python 命令和文件读写；每次是新解释器，工作目录跨调用保存。

旧 agentboxd、专用 Gateway、直接宿主 runsc 后端及旧调度/QoS 控制已清退。默认构建、文档和测试统一走新路径。Job/Kueue 固定配额批任务已增加真实图片处理、独立 PVC 产物及 Go 提交/查询/取消/下载接口；健康感知 AdmissionCheck 已增加独立实现与显式部署入口，功能验收与 R/B/A 性能资格分别记录。

## 构建与使用

```bash
make build
make check
```

构建产物为 agentbox-api、agentbox-cleanup、agentbox-mcp、agentbox-agent、agentbox-admission。需要 Linux、Go 1.26+；项目固定 toolchain go1.27.1，依赖和工具链需提前准备。普通检查使用本地模型/API 替身与离线 Kustomize 渲染，不调用付费模型或连接 Kubernetes；平台检查需可用 kubectl 和 scripts/requirements-platform.txt 中的 YAML 依赖。部署已有 API 后，由调用方配置独立 API 身份和模型凭据，按[使用指南](docs/usage.md#agent)运行 Agent。

Agent 自动创建并绑定一个会话，仅向模型开放查询/执行/文件工具；结束或异常退出时在独立预算内请求关闭并确认。创建恢复使用相同 request_id；未知执行/写入不重放。MCP 子进程只获得 API token，模型和集群凭据不进入 MCP 或执行 Pod。

静态部署资源使用 [YAML＋Kustomize](docs/operations.md#manifests)：配置按组件集中，独立的 lab/session、lab/batch 组合环境，验收夹具位于 tests/fixtures/platform，实验参数位于 experiments/configs/session；七份版本锁使用 YAML。Python 保留实验编排和验证，JSON 用于运行报告、缓存元数据及历史证据。普通检查的 Python 依赖包含固定 Pillow，见 scripts/requirements-platform.txt。

宿主脚本按 core、manifests、platform、product、acceptance、experiments、observability 分类。图片容器入口位于 `workloads/images/`，Pod 探针位于 `tests/fixtures/probes/`；依赖边界与执行位置见[脚本组织](docs/operations.md#script-layout)。

## 开发平台与真实验收

先按[运维指南](docs/operations.md#platform-lab)准备 kind、工具、缓存、Calico/gVisor 与上游组件。已有健康开发集群可复用：

```bash
make platform-up
make platform-ready
make platform-agent-test
```

platform-up 依赖已准备的锁定归档和镜像 bundle；不隐式下载/更换不匹配版本。platform-agent-test 启动真实 Agent/MCP 子进程与确定性模型替身，验证计算、程序错误修正、文件导出、超时、成功响应丢失不重放，以及三个运行环境的独立回收；只清理本轮临时资源并保留集群。普通 Job、运行时、网络、冷/预热会话、故障、API 和 Claim 保留验收仍有独立入口，见[验证指南](docs/validation.md)。

`agentbox-cleanup` 为独立管理员工具，默认预览，按 closed_at 与保留期核对原 UID/resourceVersion 后清理 Claim，保存持久审计报告。产品 API 不获得元数据删除权限，操作见[记录清理](docs/operations.md#claim-retention)。

准备固定 Kueue 清单后，可运行 `make platform-batch-install`、`make platform-batch-test`。批任务使用独立 namespace/静态队列，在 Worker 的 gVisor Job 内直接计算；入口验证排队、准入、取消、失败、deadline 与实际运行时回收，不提供批任务产品 API。范围与失败夹具清理见[批任务运维](docs/operations.md#batch)。

图片产品闭环使用 `python3 scripts/prepare_batch_images.py --download`、`make platform-batch-image-build` 和 `make platform-batch-product-test`。输入为四张有许可和 SHA-256 的公开样例，每任务输出八张经过字节/像素校验的 PNG。`agentbox-api --mode=batch` 使用独立权限与 PVC，结果在 Job 删除后保留，原会话模式保持兼容。接口见[批任务 API](docs/usage.md#batch-api)，部署与临时数据清理见[图片任务运维](docs/operations.md#batch-images)。

P3c 增加 [Prometheus/Grafana 与只读观察器](docs/operations.md#observability)。预载锁定镜像后运行 `make observability-up`；`make platform-observability-test` 验证真实指标、采集中断、权限丢失及重启恢复。Grafana 仅监听 `127.0.0.1:13000`，密码位于忽略的 `.local/observability/grafana-password`。固定到达与 R/B 静态配额实验入口为 `make platform-static-baseline`，配置、账本和解释边界见[基线运维](docs/operations.md#static-baseline)；提供实验入口不代表已形成容量或收益结论。

`make platform-diagnostics-test` 独立验证分域权限故障、sandboxd 阶段关联和失败证据归档，包含宿主、控制面及清理时序，见[诊断细节](docs/operations.md#diagnostic-details)。诊断通过与 P3/P4 性能验收分别记录。

`make platform-maintenance-test` 进一步验证队列停收时的后台发布/取消恢复、就绪状态失效、自然到期观测及 Claim 分页清理，范围与证据见[维护验收](docs/validation.md#maintenance)。客户端协议和工具契约已独立于 Kubernetes 服务实现；批任务记录的长期容量回收仍需显式协议升级。

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

固定轨迹行为边界入口为 `make platform-session-boundary-test`，覆盖长短任务干扰、同会话冲突、突发池耗尽、低高低恢复、超时/关闭竞争及观察失联。逐请求结果与质量目标独立报告，见[实验记录](docs/validation.md#session-boundaries)。

多 Pod/真实用户/代码负载组合使用 `make platform-session-matrix-test`，支持 2/4 个执行会话、Alice/Bob 独立凭据、CPU/内存/文件/输出/预期失败，以及空闲、关闭和客户端截止。选择独立诊断批次、预算与证据见[矩阵实验](docs/operations.md#session-matrix)。

P4 准入扩展使用独立 `agentbox-admission`，健康窗口由可信的后续命令驱动校验并经 Prometheus 查询，控制新增批 Job 的许可。`make platform-admission-test` 验证独立队列中的等待、Retry、恢复、权限及 leader 故障，`make platform-admission-deployment-test` 独立验证真实 Controller Pod 的集群内 HTTPS/CA 和网络故障；部署和启停见[健康准入运维](docs/operations.md#health-admission)。默认会话/批任务组合不会自动启用它，性能收益仍待共同 R/B/A 评估。
