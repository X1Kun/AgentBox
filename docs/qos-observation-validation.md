# QoS 观察模式验收记录

日期：2026-10-16。范围：只读采集、Mock Core、记录工具，以及既有执行流程回归。本记录与历史预热池基准分开，不表示宿主固定服务已经更新。

## 构建与检查

- `make check build`：race 单元测试、脚本测试、基准汇总测试、vet、格式、diff、Shell/Python 语法检查及两个二进制构建均通过。
- 初次受限运行中，systemd 通知测试因沙盒禁止 Unix socket 失败；获准在沙盒外运行后通过。
- 初次还暴露已有池测试的竞态假设：`Total` 包括创建中的实例，不能据此断言补池结束。测试改为等待两个 READY 实例且没有在途创建，之后连续 50 次 race 运行通过；未修改该项生产调度行为。

观察实验使用同一构建：

```text
agentboxd SHA-256: a703db49acbb8084e90113880dc7fdaa8a0e3a46e6afe6cc22cb297c158b4f9f
mockcore  SHA-256: bca97969cf41dbde713d654b1611694fff74cae7b78add812927402e497da192
kernel: 6.8.0-111-generic
```

## 真实 runsc 观察实验

运行 `bash scripts/observe-container.sh kvm` 和 `bash scripts/observe-container.sh systrap`，均退出成功。使用独立容器、两 CPU 外层配额和 5GiB 内存上限；每平台顺序运行空闲池和沙箱负载两组，阶段配置五秒、核心服务每秒 20 次请求。两个平台的实验依次运行，没有与本次回归测试并行。

| 平台 | 阶段 | Core 成功/提交 | P50 / P99（ms） | 完成且已清理的超时沙箱 | 有效记录帧数 |
| --- | --- | --- | --- | --- | --- |
| KVM | idle-pool | 100/100 | 11.06 / 14.68 | 0 | 48 |
| KVM | sandbox-load | 100/100 | 15.54 / 90.64 | 6 | 53 |
| systrap | idle-pool | 100/100 | 10.81 / 15.76 | 0 | 41 |
| systrap | sandbox-load | 100/100 | 16.02 / 76.61 | 6 | 50 |

四组均无核心服务拒绝、客户端丢弃、传输错误、沙箱异常、观察请求错误，也没有过期、来源不可用或仅基线的记录帧。固定工作量 checksum 在四组间一致。

两个平台均通过：三个来源可用、固定工作量请求完成、沙箱负载实际执行、观测前后资源限制相同、受保护 cgroup 删除后明确报告缺失且执行服务保持 ready、重建后恢复采集、关闭后没有沙箱目录或 cgroup 残留。

沙箱父级前后保持 `cpu.max=80000 20000`、`memory.high=3221225472`、`memory.max=4294967296`、`memory.swap.max=0`、`pids.max=2048`。核心服务前后保持 `cpu.max=max 100000`、`memory.high=max`、`memory.max=268435456`、`pids.max=64`。外层配额为 `cpu.max=200000 100000`。

本地原始证据（artifacts 不纳入 Git）：

- `artifacts/observe-kvm-ry3oGpub/`，汇总时间 `2026-10-16T10:54:34.276369+00:00`。
- `artifacts/observe-systrap-XMexRhwp/`，汇总时间 `2026-10-16T10:55:02.618522+00:00`。

这些短窗口结果证明记录工具能观察到争用下的响应变化，不能证明控制收益，也不用于比较两种平台的性能优劣。完整口径及下一阶段对照设计见 [观察模式说明](qos-observation.md)。

## 执行回归

`bash scripts/integration-container.sh kvm` 成功：cold 17 个执行子用例、warm 17 个相同执行子用例以及 4 个池生命周期用例，共 38 个子用例通过。覆盖输出、超时、OOM、取消、失败清理、状态恢复和预热隔离。日志：`artifacts/qos-integration-kvm.log`。

## systemd 部署回归

`bash scripts/integration-systemd-container.sh kvm warm` 成功，在一次性 systemd 容器中验证：

- unit 的 observe 模式、采样周期和受保护路径能正确传入 daemon。
- 指定的受保护 cgroup 不存在时，QoS 报告 degraded、来源 sample 为 null，执行服务仍 ready，真实 Python 任务执行和清理成功。
- 原有执行、重复重启、执行中停止、崩溃恢复，以及非法配置拒绝、更新、回退、保留数据卸载、重装和显式清除全部通过。
- 八个空闲实例，每次 runsc delete 人为增加两秒延迟，关闭耗时 4.071 秒，保留四路并行清理行为。

日志：`artifacts/qos-systemd-kvm-warm.log`。最终容器服务状态为已卸载，容器已删除。本轮没有更新宿主机原有 `agentbox.service`，宿主 observe 模式验收仍待部署后单独执行。
