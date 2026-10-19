# 观测补充验收记录

日期：2026-10-19。代码基线为 `d4ccb8d`，工作区包含此前的静态实验改动及本轮新增观测。宿主固定服务没有安装、更新或启停；没有执行 Git 写命令，也没有读取性能书籍 PDF。

## 自动检查

`make check build` 通过 Go race 测试、Python 部署与实验测试、benchmark summary、vet、格式、diff、Shell/Python 语法检查，生成 agentboxd 和 mockcore。随后增加的测量有效性用例及池计数守恒断言，分别由 Python 十个测量测试和 sandbox race 回归再次验证。

本轮重点覆盖：

- 创建成功、失败、停止后中止的分母与耗时；排队分配、取消、超时和失效的驻留时间；数量/字节拒绝的分类。
- 清理动作执行过程中即可查询 active；结束后的错误记录、快照独立性和 64 阶段历史上限。
- I/O/PID 可选文件缺失不会使 CPU/内存失效；一秒缓存、恢复基线、单族计数回退、零与缺测的区别。
- 真实内核根和私有 cgroup 命名空间根的祖先发现差异；辅助数据过期时不报告有效速率。
- 503 原因与真实排队时长；正式任务 ID 关联、排除 warmup、重复完成事件检测、计数回退和无效资源帧。

## 只读真实内核采集

执行：

```bash
AGENTBOX_LIVE_OBSERVATION=1 \
AGENTBOX_LIVE_CGROUP=/sys/fs/cgroup/system.slice \
go test -race -tags=integration -run '^TestLiveKernelObservation$' \
  -count=1 -v ./internal/qos
```

测试通过。两次采集间隔约 1.1 秒，实际读取 `/proc/pressure` 和指定 cgroup；没有创建 cgroup、迁移进程、改变资源限制或启动沙箱。CPU/内存读取与差分有效；系统 I/O PSI、cgroup I/O PSI、io.stat 与 PID 文件在本环境可读，低频采集刷新并建立有效增量。

该次第二轮单来源读取耗时约为 host 0.162ms、cgroup 1.929ms。这只是两个读数，不能当作完整观察器开销基准或稳定上界；完整多来源采集、状态编码和记录器成本尚未单独测量。

## KVM 端到端短测未通过

两次执行 `bash scripts/observe-container.sh kvm`，使用私有 cgroup、PID、网络命名空间和 Docker init，外层限制为 2 CPU、5GiB、2304 个任务；源码/rootfs 只读，运行状态与宿主侧 `/tmp` 使用有界 tmpfs。本轮没有复制整套依赖到内存，也没有重跑三轮静态性能实验。

两次都在 daemon 预热启动阶段失败，尚未进入本轮观察链路的正式验收：

| 本地记录 | 现象 |
| --- | --- |
| `artifacts/observe-kvm-7r5tdjeI/` | 日志报告 `runsc delete: signal: killed`，清理失败后预热池不可用 |
| `artifacts/observe-kvm-yS3vHzix/` | daemon 报告 `context deadline exceeded`，脚本捕获启动退出 |

现场宿主 I/O PSI some avg10 约 31%～34%，full 约 28%～31%；没有取得足以确定等待点的内核栈，**不能认定 I/O 就是根因**，也没有通过旧新版本同条件对照排除回归。保留原有启动/清理超时预算，没有用延长超时把失败改成通过。

脚本增加了启动退出时立即报错、启动及负载后状态快照、采样序号推进、创建/队列计数守恒和五个清理阶段顺序的断言。这些完整运行时断言目前仍待成功启动后的端到端验证；单元测试和只读真实内核采集不替代它们。

## 既有数据的离线增强分析

输入为已验收的 `artifacts/static-kvm-XCmQHtIX/`，输出为 `artifacts/observability-analysis-20261019-final/`；没有启动负载。分析 JSON 记录输入文件 SHA-256，逐帧重新检查就绪和采样有效性。九组都符合历史对照条件，177 个正式任务完成事件全部关联，排除预热任务，未匹配为零。

新增图分别展示同一批 Core 请求的三种时间口径、正式沙箱任务的阶段分布，以及每轮 Core/发送滞后/库存/队列时间线。人工检查了代表图的坐标、单位和图例。纳入项目的图和分析数据见 [观测说明](observability.md)，原始逐请求数据及其余图仍保留在 artifacts。

历史记录不包含本轮新加的 manager、祖先、I/O/PID、采样序号和分类直方图，分析按缺测处理。旧实验能支持的性能结论仍限于旧代码和原实验条件；本轮新增逻辑尚无完整性能复测结果。
