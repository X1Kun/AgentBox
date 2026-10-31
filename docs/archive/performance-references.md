# AgentBox 性能理论：引用与证据索引

当前运行入口已清退；本页仅为历史记录，源码和命令按[归档版本](legacy-runtime.md)复现。

配套正文：[性能理论与瓶颈判断](performance-theory.md)。本索引记录实际选读页面，不将未读章节作为依据。

本页索引对应历史单机分析与证据，不代表 Kueue/Koordinator 的实现或验收。新架构官方依据集中在 [DESIGN](../../DESIGN.md)，开发顺序见[实施计划](../roadmap.md)。

## 书籍及页码

Brendan Gregg, *Systems Performance: Enterprise and the Cloud*, 2nd edition, Addison-Wesley/Pearson。用户提供文件名含 2020.12；版权页标注 ©2021，ISBN 978-0-13-682015-4。

- 本地文件：[Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf)。
- 共 929 个 PDF 页面；正文印刷页 1 对应 PDF 第 40 页。本次所引正文使用 `PDF 页 = 印刷页 + 39`，已对照正文页首/页尾核实。
- 首次整理先读目录，再选读正文 79 页（约占文件页数 8.5%）；2026-10-20 的动态库存复核追加以下八页引用。没有通读全书。引用所用页包含跨页上下文，部分同页非相关段落不作为论据。
- 引文仅保留便于核查的短句/短语；原理说明为中文转述。公式在正文区分书中模型和针对本项目的推导。
- PDF SHA-256：`18bc406ccb9c71ac3fedb016df8b0c85ccf55d7d86f724c3abb3ddb05a823e17`。

选读印刷页：23–24、33–35、44–45、47–54、56–57、64–69、74–77、167、173、177–180、189–194、225–227、309–310、324、330–331、585、613–624、631–633、643–650、657–659、662–666。

2026-10-20 新增引用页：72–73、317–318、321–322、582–583；用于[动态库存理论复核](https://github.com/X1Kun/AgentBox/blob/c6c2e48/docs/runtime.md#inventory-history)。本轮重核 PDF SHA-256 与原记录一致。

## 书中依据

<a id="b01"></a>

### B01 · §2.2.1–2.2.2：系统边界与干扰

印刷页：23, 24；PDF 页：62, 63。
下列引文位于印刷页 23 / PDF 第 62 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=62)。

> perturbations (interference) can affect results

先画系统边界和数据路径，把测试端、后台工作和邻居负载纳入分析。

<a id="b02"></a>

### B02 · §5.1.1：性能目标

印刷页：173；PDF 页：212。
下列引文位于印刷页 173 / PDF 第 212 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=212)。

> A performance goal provides direction for your performance analysis work

先定义要优化的业务目标，区分延迟、吞吐、资源效率和成本。

<a id="b03"></a>

### B03 · §2.3.11–2.3.12：使用率与饱和

印刷页：33, 34；PDF 页：72, 73。
下列引文位于印刷页 34 / PDF 第 73 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=73)。

> 100% busy does not mean 100% capacity.

忙碌时间、容量占用和等待程度不同，不能用一项使用率代替瓶颈判断。

<a id="b04"></a>

### B04 · §2.5.9：USE 与软件资源

印刷页：47, 48, 49, 50, 51, 52；PDF 页：86, 87, 88, 89, 90, 91。
下列引文位于印刷页 47 / PDF 第 86 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=86)。

> For every resource, check utilization, saturation, and errors.

逐项检查物理资源、配额和软件池的使用、积压及错误，并记录观测盲区。

<a id="b05"></a>

### B05 · §2.5.10：RED

印刷页：53；PDF 页：92。
下列引文位于印刷页 53 / PDF 第 92 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=92)。

> For every service, check the request rate, errors, and duration.

从调用者角度同时报告请求率、失败和耗时分布。

<a id="b06"></a>

### B06 · §2.5.13–2.5.15：延迟分解与事件

印刷页：56, 57；PDF 页：95, 96。
下列引文位于印刷页 56 / PDF 第 95 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=95)。

> breaks it into smaller components

从总耗时逐层拆分，优先解释最大组成部分；必要时保留单事件信息。

<a id="b07"></a>

### B07 · §2.6.5：排队论

印刷页：66, 67, 68, 69；PDF 页：105, 106, 107, 108。
下列引文位于印刷页 66 / PDF 第 105 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=105)。

> L = λW

用平均队长、到达率与平均停留时间建立模型；到达过程、服务分布、并行服务中心数是前提。

2026-10-21 重读补充：Little 定律连接的是稳定系统的长期平均量，不能代入 P95/P99 直接预测尾延迟。书中单盘模型的 60%/80% 利用率例子依赖其排队模型，不是本项目 CPU 或并发的通用报警阈值。

<a id="b08"></a>

### B08 · §2.6.3–2.6.4：Amdahl 与扩展性

印刷页：64, 65, 66；PDF 页：103, 104, 105。
下列引文位于印刷页 64 / PDF 第 103 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=103)。

> accounting for serial components of workloads that do not scale in parallel

局部或并行化优化有上限，需要测量串行部分，并用实际数据检验模型。

2026-10-21 重读第 64 页补充：扩展曲线可能先出现性能拐点，再触及硬上限；软件配置限制也能先于硬件限制生效。因此执行并发 4、创建并发 2 不能被解释为机器只能承受这些。

<a id="b09"></a>

### B09 · §2.8.2–2.8.6：时间窗、分位数、分布

印刷页：74, 75, 76, 77；PDF 页：113, 114, 115, 116。
下列引文位于印刷页 75 / PDF 第 114 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=114)。

> Averages are a summary statistic that hides details.

声明统计窗口，保留尾部与分布；不要用均值掩盖混合路径或异常值。

<a id="b10"></a>

### B10 · §2.3.10、§4.6：观察开销与测量可信性

印刷页：33, 167；PDF 页：72, 206。
下列引文位于印刷页 33 / PDF 第 72 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=72)。

> Performance metrics are not free

观测会消耗资源，测量和解析也可能出错；用已知负载或独立来源校验。

<a id="b11"></a>

### B11 · §5.2.3–5.2.4：缓冲与轮询

印刷页：177；PDF 页：216。
下列引文位于印刷页 177 / PDF 第 216 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=216)。

> High latency between the occurrence of the event and the next polled check

轮询频率在响应时延和 CPU 开销之间取舍，事件通知也必须符合具体场景。

<a id="b12"></a>

### B12 · §5.2.5：并发与同步

印刷页：177, 178, 179, 180；PDF 页：216, 217, 218, 219。
下列引文位于印刷页 177 / PDF 第 216 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=216)。

> they do not necessarily execute on-CPU at the same instant

并发不等于并行；锁、线程池和调度本身也会成为等待来源。

<a id="b13"></a>

### B13 · §5.4.2–5.4.5：Off-CPU、系统调用与线程状态

印刷页：189, 190, 191, 192, 193, 194；PDF 页：228, 229, 230, 231, 232, 233。
下列引文位于印刷页 194 / PDF 第 233 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=233)。

> Performance for an application request is improved by reducing the time in every state except idle.

区分运行、可运行但等 CPU、I/O、锁和真正空闲；空闲等待不能算业务性能损失。

<a id="b14"></a>

### B14 · §6.3.6–6.3.11：CPU 解释边界

印刷页：225, 226, 227；PDF 页：264, 265, 266。
下列引文位于印刷页 225 / PDF 第 264 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=264)。

> The performance of each hardware thread is not the same as a separate CPU core

SMT 线程不是独立物理核；CPU 忙还可能包含内存停顿；配额也会造成可运行线程等待。

<a id="b15"></a>

### B15 · §7.2.6–7.2.10：缓存、内存容量和工作集

印刷页：309, 310；PDF 页：348, 349。
下列引文位于印刷页 309 / PDF 第 348 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=348)。

> Memory used by the file system cache can be treated as unused

书中用可回收文件缓存说明不能仅看 used/free；不能把该简化表述外推成所有缓存或 tmpfs 都能无代价回收。

<a id="b16"></a>

### B16 · §7.4.2、§7.5.2：内存饱和与 PSI

印刷页：324, 330, 331；PDF 页：363, 369, 370。
下列引文位于印刷页 331 / PDF 第 370 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=370)。

> PSI statistics are also tracked per cgroup2

PSI 衡量任务受资源压力影响的时间；结合回收、内存事件、作用域和变化趋势分析。

2026-10-22 重读第 330～331 页：原文示例是内存 PSI，说明等待时间比例和 cgroup2 作用域。项目用于 CPU 的 `some.total` 短窗差分另按 K02 核对；暂停低优先级补池是工程应用，不是书中给出的具体控制器。20%/10%、确认 1 秒、恢复 2 秒均为本项目起点，不能当作书中证明的 SLO 阈值。

<a id="b17"></a>

### B17 · §11.1.5：多租户与邻居干扰

印刷页：585；PDF 页：624。
下列引文位于印刷页 585 / PDF 第 624 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=624)。

> performance issues may be caused by noisy neighbors

共享硬件会形成跨工作负载干扰，隔离身份不等于隔离性能。

<a id="b18"></a>

### B18 · §11.3.3：优先级、上限、CPU 带宽和内存控制

印刷页：613, 614, 615, 616；PDF 页：652, 653, 654, 655。
下列引文位于印刷页 615 / PDF 第 654 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=654)。

> a container gets a quota of CPU microseconds every period

配额表达带宽上限；不能当作独占物理核或吞吐保证。本段书籍接口以 cgroup v1 为主。

2026-10-22 重读第 613～615 页：优先级决定竞争时的相对分配，上限约束突发可消耗的资源，两者用途不同。项目据此保留可部署的父组硬预算，再单独比较按 PSI 启停补池；动态反馈不能取消已耗费的 CPU 时间，也不能把“可借用额度”当作业务保底。CPU 带宽周期与业务时延目标的关系仍需用当前内核实测，不能仅靠平均 CPU 数推断 P99。

<a id="b19"></a>

### B19 · §11.3.4：容器观测范围与资源控制统计

印刷页：617, 618, 619, 620, 621, 622, 623, 624；PDF 页：656, 657, 658, 659, 660, 661, 662, 663。
下列引文位于印刷页 618 / PDF 第 657 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=657)。

> these often show the host rather than just the container

先核对指标属于宿主、容器还是某个 cgroup；再检查限制产生的影响。v1/v2 的路径、字段和单位需重新核对。

<a id="b20"></a>

### B20 · §11.4.1–11.4.4：gVisor 的适用边界

印刷页：631, 632, 633；PDF 页：670, 671, 672。
下列引文位于印刷页 632 / PDF 第 671 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=671)。

> gVisor is a different approach that implements its own lightweight kernel

书中明确区分 gVisor 与轻量硬件虚拟机；不能把 Firecracker/Kata 示例数字当作 gVisor 成本。

<a id="b21"></a>

### B21 · §2.5.5–2.5.6：问题陈述与科学方法

印刷页：44, 45；PDF 页：83, 84。
下列引文位于印刷页 45 / PDF 第 84 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=84)。

> changing multiple factors at once makes it harder to identify which one mattered

写出假设、可检验预测、反例和实验，尽量单变量比较。

<a id="b22"></a>

### B22 · §2.5.11：工作负载特征

印刷页：54；PDF 页：93。
下列引文位于印刷页 54 / PDF 第 93 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=93)。

> It focuses on the input to the system, rather than the resulting performance.

先声明谁在发起、工作量、到达模式和变化，区分负载问题与架构问题。

<a id="b23"></a>

### B23 · §12.1.2：有效基准测试

印刷页：643, 644；PDF 页：682, 683。
下列引文位于印刷页 644 / PDF 第 683 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=683)。

> What the limiting factor or factors are

结果需要说明测试对象、限制因素、干扰和能推出的结论，而非只报告一个数字。

<a id="b24"></a>

### B24 · §12.1.3：压测工具、错误和干扰

印刷页：645, 646, 647, 648, 649, 650；PDF 页：684, 685, 686, 687, 688, 689。
下列引文位于印刷页 646 / PDF 第 685 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=685)。

> the result reported is limited by the benchmark software itself

排除压测端瓶颈，保留错误与拒绝，控制环境变化，避免只挑最好的结果。

2026-10-21 重读第 648 页补充：短至几秒的测试容易受干扰支配；更长的窗口与重复测量用于确认稳定性。当前每档 200 个请求只用于粗定位，边界附近应延长、重复；开放式发起滞后必须单列。

2026-10-22 重读第 648 页的“Changing Multiple Factors”：保护对照固定采样成本、轨迹、并发和业务工作量；idle 与 PSI 在同一配额下比较补池条件，1/2 CPU 在同一规则下比较预算。AIMD＋准入＋补池作为整体组合单列，不把结果归因给其中任一算法。

<a id="b25"></a>

### B25 · §12.3.2：主动基准测试

印刷页：657, 658, 659；PDF 页：696, 697, 698。
下列引文位于印刷页 657 / PDF 第 696 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=696)。

> you analyze performance while the benchmark is running

在负载运行时同步观测和归因；侵入式分析工具的结果不直接当正式成绩。

<a id="b26"></a>

### B26 · §12.3.5–12.3.10：递增负载、校验、重复统计

印刷页：662, 663, 664, 665, 666；PDF 页：701, 702, 703, 704, 705。
下列引文位于印刷页 664 / PDF 第 703 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=703)。

> measure latency as well as the throughput

扫负载找容量拐点，同时考察延迟；检查物理上限、重复性、数据质量和误差。

2026-10-21 重读补充：§12.3.7（第 662～664 页）逐级增加负载，同时看实际交付与延迟分布；排队把延迟推到不可接受时，峰值吞吐不再是有用容量。§12.3.8～12.3.10 要求检查结果是否合理、分析波动，并追问为何结果不能再翻倍。项目将其实现为固定配额下的开放式阶梯扫描，使用可修改的延迟与按期成功目标；具体阈值、停止守卫是工程选择，不是书中给定参数。

<a id="b27"></a>

### B27 · §2.3.13–2.3.14：采样与缓存

印刷页：35；PDF 页：74。
下列引文位于印刷页 35 / PDF 第 74 页：[打开引文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=74)。

> How coarse depends on the rate of sampling.

采样有分辨率；预热库存只能类比提前准备成本，不等同于可重复命中的结果缓存。

<a id="b28"></a>

### B28 · §2.7.3、§11.1.3：动态规模与过量供给

印刷页：72–73、582–583；PDF 页：111–112、621–622。
[打开扩缩容页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=111)、[打开风险讨论页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=622)。

书中讨论按性能指标调整服务实例数，同时指出异常流量、应用回归等可能造成过量供给，需要监控增量是否合理。它支持动态资源管理的方向，不证明 AgentBox 的触发指标合理；READY 库存增加也不等同于服务实例并行度增加。本项为中文转述，无新增逐字引文。

<a id="b29"></a>

### B29 · §7.3.2、§7.3.3：空闲资源、回收、滞回和对象分配

印刷页：317–318、321–322；PDF 页：356–357、360–361。
[打开空闲列表页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=356)、[打开滞回页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=357)、[打开 slab 页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=360)。

空闲列表便于快速分配，回收可将空闲缓存归还系统；书中以双水位讨论滞回，并介绍 slab 对象缓存及局部库存。用于 AgentBox 时只类比提前准备、保留与回收的取舍；一次性沙箱不能复用已执行实例，时间滞回也不是照搬内存水位算法，不提供 30 秒参数或稳定性证明。本项为中文转述。

<a id="b30"></a>

### B30 · §2.8.1：基于观测和对照量化收益

印刷页：73；PDF 页：112。[打开原文页](../../../Systems.Performance.Enterprise.and.the.Cloud.2nd.Edition.2020.12.pdf#page=112)。

书中允许依据可靠观测分解耗时并估计局部收益，也可通过前后对照量化；需区分请求同步路径与后台异步事件。理论模型给出条件化判断，功能测试不能代替收益比较；并不要求先完成大规模实验才能继续开发。本项为中文转述。

## 项目代码依据

审查时 HEAD 为 `d4ccb8def4e65b019e4cfc1d21606ab21726e81c`，工作区另含静态 CPU 实验未提交改动。链接行号仅定位本次版本，以函数名为稳定检索依据。

<a id="c01"></a>

### C01 · 请求处理与端到端入口

- [internal/httpapi/server.go:24](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/httpapi/server.go#L24)：`func Handler`。
- [cmd/agentboxd/main.go:30](https://github.com/X1Kun/AgentBox/blob/c6c2e48/cmd/agentboxd/main.go#L30)：`func run`。

<a id="c02"></a>

### C02 · cold/warm 执行、计时、创建和销毁

- [internal/sandbox/engine.go:323](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/engine.go#L323)：`func (e *Engine) Execute`。
- [internal/sandbox/engine.go:377](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/engine.go#L377)：`func (e *Engine) executeWarm`。
- [internal/sandbox/engine.go:492](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/engine.go#L492)：`func (e *Engine) destroyInstanceResources`。
- [internal/sandbox/engine.go:532](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/engine.go#L532)：`func (e *Engine) runAndCollectResult`。

<a id="c03"></a>

### C03 · FIFO 准入、库存补充、总容量与清理

- [internal/sandbox/scheduler.go:24](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/scheduler.go#L24)：`func (p *warmPool) acquire`。
- [internal/sandbox/pool.go:118](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/pool.go#L118)：`func (p *warmPool) refill`。
- [internal/sandbox/pool.go:204](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/pool.go#L204)：`func (p *warmPool) release`。
- [internal/sandbox/pool.go:227](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/pool.go#L227)：`func (p *warmPool) drain`。
- [internal/sandbox/config.go:8](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/config.go#L8)：`func normalizePoolConfig`。

<a id="c04"></a>

### C04 · runsc create/start/exec/wait 与管道收尾

- [internal/sandbox/runsc.go:110](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/runsc.go#L110)：`func (s *session) create`。
- [internal/sandbox/runsc.go:136](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/runsc.go#L136)：`func (s *session) start`。
- [internal/sandbox/runsc.go:154](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/runsc.go#L154)：`func (s *session) wait`。
- [internal/sandbox/runsc.go:186](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/runsc.go#L186)：`func (s *session) finishStreams`。

<a id="c05"></a>

### C05 · 父级/单实例 cgroup 配额与退出统计

- [internal/cgroup/cgroup_linux.go:43](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/cgroup/cgroup_linux.go#L43)：`func New`。
- [internal/cgroup/cgroup_linux.go:96](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/cgroup/cgroup_linux.go#L96)：`func (m *Manager) NewGroup`。
- [internal/cgroup/cgroup_linux.go:191](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/cgroup/cgroup_linux.go#L191)：`func (g *Group) Usage`。
- [internal/cgroup/stats.go:33](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/cgroup/stats.go#L33)：`func ReadStats`。

<a id="c06"></a>

### C06 · PSI、采样增量、缓存有效性

- [internal/qos/reader.go:30](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/qos/reader.go#L30)：`func (FileReader) Read`。
- [internal/qos/observer.go:145](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/qos/observer.go#L145)：`func (o *Observer) collect`。
- [internal/qos/observer.go:178](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/qos/observer.go#L178)：`func (o *Observer) Snapshot`。
- [internal/qos/observer.go:223](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/qos/observer.go#L223)：`func derive`。
- [internal/sandbox/observation.go:11](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/observation.go#L11)：`func (e *Engine) observationReport`。

<a id="c07"></a>

### C07 · Python 引导、只读根、tmpfs、输出预算

- [internal/sandbox/spec.go:74](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/spec.go#L74)：`func makeTaskProcess`。
- [internal/sandbox/spec.go:84](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/spec.go#L84)：`func makeSpec`。
- [internal/sandbox/output.go:32](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/output.go#L32)：`func (w outputWriter) Write`。

<a id="c08"></a>

### C08 · 固定 Core 工作量与并发准入

- [internal/mockcore/server.go:28](../../internal/mockcore/server.go#L28)：`func Work`。
- [internal/mockcore/server.go:40](../../internal/mockcore/server.go#L40)：`func Handler`。

<a id="c09"></a>

### C09 · 负载生成和逐请求统计

- [scripts/lib/observe.py:51](https://github.com/X1Kun/AgentBox/blob/c6c2e48/scripts/lib/observe.py#L51)：`def sandbox_workload`。
- [scripts/lib/observe.py:64](https://github.com/X1Kun/AgentBox/blob/c6c2e48/scripts/lib/observe.py#L64)：`def core_metrics`。
- [scripts/lib/observe.py:77](https://github.com/X1Kun/AgentBox/blob/c6c2e48/scripts/lib/observe.py#L77)：`def summarize_sandboxes`。
- [scripts/lib/observe.py:91](https://github.com/X1Kun/AgentBox/blob/c6c2e48/scripts/lib/observe.py#L91)：`def run_phase`。

<a id="c10"></a>

### C10 · 静态实验环境、分组与质量检查

- [tests/observation/static_container.py:38](https://github.com/X1Kun/AgentBox/blob/c6c2e48/tests/observation/static_container.py#L38)：`def main`。
- [scripts/lib/static_experiment.py:6](https://github.com/X1Kun/AgentBox/blob/c6c2e48/scripts/lib/static_experiment.py#L6)：`def experiment_plan`。
- [scripts/lib/static_experiment.py:13](https://github.com/X1Kun/AgentBox/blob/c6c2e48/scripts/lib/static_experiment.py#L13)：`def check_phase`。

<a id="c11"></a>

### C11 · 图表口径与拒绝错误实验

- [scripts/lib/plot_static.py:24](https://github.com/X1Kun/AgentBox/blob/c6c2e48/scripts/lib/plot_static.py#L24)：`def window_latencies`。
- [scripts/lib/plot_static.py:30](https://github.com/X1Kun/AgentBox/blob/c6c2e48/scripts/lib/plot_static.py#L30)：`def observation_series`。
- [scripts/lib/plot_static.py:49](https://github.com/X1Kun/AgentBox/blob/c6c2e48/scripts/lib/plot_static.py#L49)：`def validate_readiness`。
- [scripts/lib/plot_static.py:102](https://github.com/X1Kun/AgentBox/blob/c6c2e48/scripts/lib/plot_static.py#L102)：`def comparison`。

<a id="c12"></a>

### C12 · 历史冷暖基准的库存和持续场景

- [tests/benchmarks/latency_test.go:177](https://github.com/X1Kun/AgentBox/blob/c6c2e48/tests/benchmarks/latency_test.go#L177)：`func TestColdWarmHTTPBenchmark`。

<a id="c13"></a>

### C13 · 2026-10-20 当前动态库存与触发路径

- [internal/sandbox/inventory.go](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/inventory.go)：当前 v2 的 `adjustInventoryLocked`、`reserveSurplusLocked`；[inventory_policy.go](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/inventory_policy.go) 提供纯策略与推荐最大值。
- [internal/sandbox/scheduler.go](https://github.com/X1Kun/AgentBox/blob/c6c2e48/internal/sandbox/scheduler.go)：当前 `observeWaiterLocked` 只确认真实库存阻塞，已移除 v1 的普通领取/任意入队 Demand。
- [动态库存说明](https://github.com/X1Kun/AgentBox/blob/c6c2e48/docs/runtime.md#inventory)：原 dynamic-inventory 专题的机制与理论边界；现行配置见[运维指南](https://github.com/X1Kun/AgentBox/blob/c6c2e48/docs/operations.md#inventory-config)，功能证据见[验收归档](local-validation.md#inventory-validation)。
- 上述代码针对 `cc2d381` 加动态库存 diff，与此前 C01–C12 的历史版本区分；E04 保留的是修正前 v1 的源码哈希与反例，不对应当前 v2 源码。

## 实验及本次重算依据

<a id="e01"></a>

### E01 · 静态 CPU 三轮对照

- [实验设计](local-validation.md#static-method)、[验收说明](local-validation.md#static)。
- [九组逐轮汇总](../data/static-cpu-experiment.json)、[完整实验元数据](../data/static-cpu-experiment.json)。
- 这批是 2026-10-17 已存在的 KVM 实验；本次没有重跑负载、更新服务或修改资源配置。

<a id="e02"></a>

### E02 · 历史预热池基准

- [冷暖历史对比](local-validation.md#warm)。
- 内核、版本、工作负载和外层预算与 E01 不同；只能用来提出补池假设，不能混成一组当前性能成绩。

<a id="e03"></a>

### E03 · 本次只读重算

- [performance-theory-evidence.json](../data/performance-theory-evidence.json) 记录来源哈希、过滤窗口、统计方法、九组多口径 P99 和成对变化。
- 从既有 `artifacts/static-kvm-XCmQHtIX/rN-*-core.csv` 重算；每组只选计划发送时刻在 `[5000,65000)` ms、HTTP 200 且 error 为空的 1,200 行。
- 每列独立使用 nearest-rank P99：排序后取第 `ceil(0.99*n)` 项，不对各列 P99 做相加或相减归因。
- 阶段耗时中位数来自 E01 已记录的日志分析，本次没有把不同阶段的中位数相加。

<a id="e04"></a>

### E04 · 2026-10-20 动态库存理论复核

- [分析正文](https://github.com/X1Kun/AgentBox/blob/c6c2e48/docs/runtime.md#inventory-history)、[来源哈希、历史数据重算与反例检查源码](../data/performance-theory-evidence.json)的 inventory_v1_review 字段。
- 在 `/tmp` 源码副本调用实际生产方法，构造“已有 READY、仅缺执行名额”和“每 10 秒请求、补池已完成、从未排队”两种状态。记录到目标仍增加；该检查是逻辑时间状态分析，不是真实负载或性能对照。
- 历史冷暖数据从原 `requests.csv` 只读重算，没有更新运行时、启动压力负载或执行 Git 写操作。

## 版本语义补充（不是本书引文）

<a id="k01"></a>

### K01 · Linux 6.8 cgroup v2

[内核接口文档](https://docs.kernel.org/6.8/admin-guide/cgroup-v2.html)：只用于核对层级约束、`cpu.max`、`memory.high/max`、`memory.events` 的现行语义。
书籍 B18/B19 主要展示 v1 文件和部分纳秒计数；本项目使用 v2 的 `usage_usec`、`throttled_usec`，不能直接复制文件名和单位。

<a id="k02"></a>

### K02 · Linux 6.8 PSI

[内核接口文档](https://docs.kernel.org/6.8/accounting/psi.html)：只用于核对 `some/full/total` 的精确定义、系统级 CPU full 不适用、cgroup 压力来源。
书中第 331 页对 full 使用简化描述；本文采用当前接口的“所有非空闲任务”口径，且不把 PSI 当成 CPU 指令停顿或任意锁等待的完整计量。
