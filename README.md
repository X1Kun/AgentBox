# AgentBox

面向 AI Agent 的 Python 代码执行沙箱，使用 gVisor 隔离代码，以 cgroups v2 限制资源。已实现 cold/一次性 warm 池、有界 FIFO、动态 READY 库存、单机 CPU/压力 QoS、普通用户 Gateway、MCP 及 DeepSeek Tool Calls 示例。多 Worker 调度和 kind 跨节点验证是下一阶段。

每个任务使用全新实例，用后销毁；Gateway 仅绑定 loopback，经受权限保护的 Unix socket 调用 root daemon。任务默认无外网、只读根文件系统、UID 1000。取消后由 daemon 独立清理，结果不确定时不自动重放。

## 开发与使用

需要 Linux、Go 1.25+；真实运行另需 cgroups v2 委派、runsc、可信 Python rootfs 和 Docker。KVM 验收需可访问 /dev/kvm，也可显式使用 systrap。执行引擎和 Gateway 使用标准库，MCP 依赖固定版本的官方 Go SDK。

```bash
make build
make check

# 准备项目本地依赖，不覆盖现有 .local 安装。
bash scripts/setup.sh runsc
bash scripts/setup.sh rootfs
./bin/agentboxd -doctor -runsc .local/gvisor/runsc -platform kvm

# 在独立容器内验证，不操作已有 kind 节点。
python3 scripts/container.py runtime --platform systrap

# MCP 短流程；API 使用本地模拟服务，不产生模型费用。
AGENTBOX_TEST_DEPENDENCIES_TMPFS=1 AGENTBOX_TEST_IMAGE_ROOTFS=1 \
  python3 scripts/container.py mcp --platform systrap
```

MCP/DeepSeek 的环境变量、实际 API 联调和远端隧道见 [接入说明](docs/mcp-deepseek.md)。真实 deepseek-flash 已完成除零错误后修正为 42 的闭环；普通回归不依赖付费模型。

## 四个脚本入口

| 入口 | 职责 |
| --- | --- |
| `bash scripts/setup.sh runsc\|rootfs` | 下载校验 runsc、导出可信 rootfs |
| `python3 scripts/deploy.py base\|gateway\|smoke` | 安装/更新/回退/卸载与宿主验收；宿主操作需 sudo |
| `python3 scripts/container.py --help` | runtime/qos/mcp/gateway/live/systemd/benchmark/observe/static 的隔离容器入口 |
| `python3 scripts/experiment.py record\|analyze\|plot` | 记录已运行服务、离线分析与绘图 |

`--help` 可查看各子命令；container 的 `--dry-run` 只显示计划，不构建、不操作 Docker、不调用 API。`scripts/lib` 为内部模块，不再保留旧包装脚本。Makefile 的 benchmark/observe/static-experiment/qos-integration/systemd-integration 可通过 PLATFORM=systrap 切换平台。

依赖版本和 digest 保存在 .local；API 凭据文件也只保存在该忽略目录。完整旧产物已备份，artifacts 保留关键日志和压缩原始数据，恢复方式见 [验收汇总](docs/validation.md#evidence)。

## 文档

| 主题 | 入口 |
| --- | --- |
| 架构、隔离与组件边界 | [DESIGN](DESIGN.md) |
| 单机执行与 FIFO/预热池 | [warm-pool](docs/warm-pool.md) |
| 动态库存、开源机制和单调队列 | [dynamic-inventory](docs/dynamic-inventory.md) |
| CPU 控制与压力准入 | [qos](docs/qos.md) |
| 采样、诊断流程与测量边界 | [observability](docs/observability.md) |
| daemon 部署 | [systemd-deployment](docs/systemd-deployment.md) |
| 普通用户入口与配对部署 | [gateway](docs/gateway.md) |
| MCP 与 DeepSeek | [mcp-deepseek](docs/mcp-deepseek.md) |
| 功能验收与性能结果 | [validation](docs/validation.md) |
| 静态 CPU 实验方法 | [static-cpu-experiment](docs/static-cpu-experiment.md) |
| 性能模型与依据 | [performance-theory](docs/performance-theory.md)、[引用索引](docs/performance-theory-references.md) |
| 下一阶段 | [implementation-plan](docs/implementation-plan.md) |

功能通过、设计依据与性能收益分别陈述：固定 warm 的历史数据支持命中等待降低，动态库存和动态 QoS 的收益仍待对照；tmpfs 功能通过不代表普通磁盘冷启动问题已修复；kind 节点共享宿主资源，不能代替物理跨机性能证据。
