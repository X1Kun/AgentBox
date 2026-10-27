# 运维指南

状态：agentboxd 已决定退役，但代码移除和服务切换尚未实施。当前提供普通 Job、gVisor 运行时及 Calico NetworkPolicy 实验入口；Agent Sandbox 会话、Kueue 与新产品接口仍待交付。下文旧服务配置只供已有部署收尾和复现。阶段状态见[路线图](roadmap.md)。

按任务跳转：[新平台入口](#platform-lab) · [gVisor 验收](#gvisor-runtime) · [网络策略验收](#network-policy) · [平台部署计划](#platform-plan) · [退役流程](#retirement) · [旧安装](#deployment) · [旧配置](#config) · [生命周期](#lifecycle) · [更新与卸载](#base-update) · [旧 Gateway 配对](#gateway) · [检查](#smoke) · [排障](#diagnosis)。命令从仓库根目录执行。

<a id="platform-lab"></a>
## 独立 Kubernetes 功能实验

`scripts/platform_lab.py` 编排 kind 和 kubectl，固定一个 control-plane、一个专用 worker，可信 Python Job 在 worker 计算确定结果并校验终态、Pod 归属、输出与清理。它不依赖 agentboxd，不包含 gVisor、Agent Sandbox 会话、Kueue、网络隔离或性能验收。

要求 Linux/amd64、Python 3.10+、Docker Engine 24+ 和 cgroup v2；Docker 可见至少 4 个逻辑 CPU、4GiB 内存只是启动最低条件，不是空闲资源保证。宿主 containerd、KVM、helm 和 k6 不是本入口的必需项。

工具与镜像以 [versions.lock.json](../deploy/platform/versions.lock.json) 为准。kind/kubectl 固定版本及上游二进制 SHA-256，node/任务镜像固定 digest；containerd 随 node 镜像固定，实际版本从 Node status 记录。kind/node 组合来源见[官方发布](https://github.com/kubernetes-sigs/kind/releases/tag/v0.33.0)。本配置采用 Kubernetes 1.36 的 kubeadm v1beta4；锁定的 kubectl 1.35 属于[官方允许的一小版本偏差](https://kubernetes.io/releases/version-skew-policy/#kubectl)。

### 工具准备与执行

从锁文件对应的官方资产下载 Linux/amd64 二进制，核对 SHA-256 后放到 `.local/platform-tools/kind`、`.local/platform-tools/kubectl` 并设置执行权限。入口优先使用这些本地工具，其次使用 PATH，也接受 AGENTBOX_KIND/AGENTBOX_KUBECTL/AGENTBOX_DOCKER 指定路径。版本正确但校验不匹配仍会拒绝。create 不隐式下载或安装工具。

```bash
# 离线计划：不调用外部工具、不写状态、不连接 Docker。
python3 scripts/platform_lab.py create --dry-run

make platform-preflight
make platform-create
make platform-smoke
make platform-delete
```

preflight 只读主机/Docker 条件并保存结构化报告，区分缺失、超时、不可达和版本/校验错误。create 只创建 `agentbox-platform-lab`，拒绝接管已有同名且无归属记录的集群。所有 kubectl 显式使用 `.local/platform/kubeconfig` 和 `kind-agentbox-platform-lab` context，不读写默认 kubeconfig。

操作使用本地文件锁；归属绑定仓库、Docker daemon ID、节点容器 ID 和 kubeconfig 哈希，变更时停止。凭据 0600，不进入报告。每次输出到新的 `artifacts/platform-动作-随机值/`，--output 也要求目录不存在；报告记录命令、耗时、实际来源和身份。smoke 另保存模板、Job/Pod UID、Node 版本与校验结果。

kind 删除改用独立临时 kubeconfig，避免部分节点删除失败时清除真实凭据；确认节点容器消失后才移除原专用配置。失败保留 delete_failed 状态与原始节点身份，允许再次显式 delete，不放宽外来 kubeconfig/节点的归属检查。Docker 因内核等待暂时无法终止节点时报告失败，不返回虚假的清理成功。

失败保留状态，并尽力导出本集群 kind 日志，不自动删现场。create 部分失败可按记录显式 delete；崩溃导致归属无法确认时人工核对，不盲目接管。成功 smoke 使用前台级联删除，随后在独立 60s 收敛窗口核对 Pod 消失；失败 Job 留待排障。集群删除后可再次创建，旧证据不覆盖。全局镜像缓存与共享网络不属于清理范围。

### 下载源与超时

工具可换国内下载源，但须匹配上游 SHA-256。create 的 --node-image-source 和 smoke 的 --smoke-image-source 仅允许改变 repository，必须保留锁定的 @sha256:digest；先以相同参数加 --dry-run 核对。canonical 镜像在锁文件，实际来源在报告中；不修改宿主 registry mirror 配置。

此功能 profile 的节点直接访问镜像仓库。创建 kind 时只对子进程清空 HTTP/HTTPS/ALL 代理变量并设置 NO_PROXY=*，避免把宿主 127.0.0.1 代理带入节点。宿主设置保持原值；若必须使用受控出口代理，需要另行明确节点可达配置，不能直接复用宿主回环地址。

节点镜像准备单独有界（pull 最多 600s），随后 kind 创建最多 420s、节点 Ready 检查最多 120s。共享磁盘冷启动曾超过 kubeadm 默认 1m API 引导预算，因此固定 InitConfiguration.timeouts.kubernetesAPICall=4m，并保留失败记录；这只影响集群引导，不改变工作负载时限。[kubeadm 超时定义](https://kubernetes.io/docs/reference/config-api/kubeadm-config.v1beta4/)

Job 固定 activeDeadlineSeconds=120、backoffLimit=0；--timeout（30～600s）仅改变观察等待预算，不改 Job 期限。kind --retain 保留失败节点，超时或中断由归属记录及诊断日志处理。普通可信 smoke 不证明沙箱安全、自动重试或 SLO 收益。

源码当前仍保留旧脚本入口：`scripts/setup.sh` 准备依赖；`scripts/deploy.py` 维护旧服务；`scripts/container.py` 运行旧验证；`scripts/experiment.py` 记录、分析和绘图。它们不是新平台安装器，专用入口将随 P2c 从活动发布清退。使用 --help 查看参数；container 的 --dry-run 只显示计划，不构建、不操作 Docker、不调用模型 API。

Makefile 的 benchmark、observe、static-experiment、qos-integration、systemd-integration 是对应入口的快捷方式，可用 PLATFORM=systrap 显式切换。环境准备和测试条件见[验证指南](validation.md)。

<a id="gvisor-runtime"></a>
## 独立 Worker 的 gVisor 运行时验收

这是 P1 的运行时子步骤，仍只执行可信测试代码。`gvisor-install` 在已确认归属的 kind Worker 内安装完整发布包、配置官方 shim/handler 并创建 RuntimeClass；`gvisor-test` 验证真实运行与回收。两者不调用 agentboxd，不修改宿主 Docker/containerd，也不部署 Agent Sandbox 或 Kueue。

版本与官方 SHA-512 见 [gvisor.lock.json](../deploy/platform/gvisor.lock.json)。当前固定 release-20261005.0、linux/x86_64、systrap、节点 containerd 2.3.4。runsc、shim 和 gvisor-bin 辅助文件来自同一完整包；仅复制 runsc 不满足安装要求。依据：[官方安装](https://gvisor.dev/docs/user_guide/install/)、[shim 配置](https://gvisor.dev/docs/user_guide/containerd/configuration/)。

先准备普通平台工具，再下载固定资产到本地缓存。下载可替换传输源，安装器始终按锁定校验值验证内容；它本身不自动联网下载。

```bash
mkdir -p .local/platform-tools
curl --fail --location --retry 2 --connect-timeout 15 --max-time 300 \
  https://storage.googleapis.com/gvisor/releases/release/20261005.0/x86_64/gvisor.tar.bz2 \
  -o .local/platform-tools/gvisor-20261005.0-x86_64.tar.bz2

make platform-create
make platform-gvisor-install
make platform-gvisor-test
make platform-smoke
make platform-delete
```

已有本项目完整 ready 集群时从 install 开始，不重复 create。安装失败保留 `cluster.json` 中的 gvisor 安装阶段及原/目标配置哈希，修正原因后可重试同一版本；已就绪安装只验证，不重启 containerd。外部配置改动、RuntimeClass 归属/身份变化或版本定义变化会拒绝继续，不能靠覆盖状态强行接管；更换运行时版本使用新建实验集群。

`--gvisor-archive PATH` 可为 install 指定包位置。镜像网络受限时，可以在 test 使用与普通 smoke 相同的 digest 镜像源：

```bash
python3 scripts/platform_lab.py gvisor-install --dry-run
python3 scripts/platform_lab.py gvisor-test --smoke-image-source \
  docker.m.daocloud.io/library/python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258
```

安装器在宿主用 Python 标准库校验完整包、拒绝链接/越界或不完整成员，并解压成 tar 后复制至 Worker；kind 节点无需额外安装 bzip2。完整文件安装到 `/opt/agentbox-gvisor`，runsc 使用明确链接；保留节点原配置 schema 和 runc 默认 handler，通过 containerd 自身解析验证，再核对 CRI 生效配置。该 profile 通过 systemd 为实验 Worker 的 `kubelet.slice` 设置 `MemorySwapMax=0`，禁止这个 Worker 全部 Pod 使用 swap；不改变宿主或其他节点/集群，不直接写 cgroup。仅声明 Pod memory limit 不限制 swap，而 OCI base spec 的资源字段会被 CRI 覆盖，因此不用基础 spec 注入该策略。重启仅作用于该 Worker 内的 containerd，不能在有业务负载的共享生产节点照搬此入口。

验收包括正常输出、用户退出码、运行中取消、Job deadline、内存超限和不存在的 handler。每个已运行用例先等待受控 ready 标记，核对 CRI sandbox、runsc state、Sentry PID、Pod cgroup 的 500m CPU/256MiB 限制，以及有效的零 swap 上限，再触发计算/故障。取消用例包含忽略 TERM 的父子进程；完成后除 API 对象外，还检查 CRI sandbox 与匹配运行时进程消失。缺失 handler 必须明确启动失败，不能回落 runc。`--gvisor-case success|failure|cancel|deadline|oom|missing` 可单项排障；单项通过不等于六项整套验收通过。

探针 Job 默认 activeDeadlineSeconds=180，deadline 用例为 45；`--timeout` 仅改变每个启动/终态观察阶段的等待预算，不修改 Job 期限。Job deadline 用例的 Pod 可能由 Job controller 主动删除，此时以 `DeadlineExceeded` 和独立资源收敛证据判定，不编造已丢失的退出码。命令/观察超时和清理未确认分别报错。失败 Job 留在本实验集群供排障，最终由 `platform-delete` 限定范围清理。

根文件系统只读由挂载标志与负向写入共同验证；`/tmp` 和 `/work` 显式使用独立 Pod 的内存卷。声明的 emptyDir sizeLimit 尚不等于已验证的精确文件容量界限，本轮内存约束以 Pod cgroup 为依据。部分 shim/辅助进程可能处于 containerd 服务组，证据单列，不能把 Pod 指标称为全部运行时成本。

每次输出到独立 artifacts 目录，报告包含源码/配置哈希、版本、Job/Pod/CRI 身份、进程与资源证据及逐项清理结果。运行时功能与范围见[验证指南](validation.md#gvisor-runtime)。NetworkPolicy 执行、对外不可信代码服务、会话 API、预热池和生产 SLO 尚未验收。

<a id="network-policy"></a>
## Calico NetworkPolicy 实验 profile

默认 `platform-create` 仍使用原 kindnet 功能 profile。网络策略实验必须新建 Calico profile，不能向已有 kindnet 集群叠加 CNI；若本项目已有实验集群，先按归属保护流程清理，再创建。所有入口复用独立 context/kubeconfig、节点身份和本地操作锁，不接管其他集群。

Calico v3.33.0 的官方 VXLAN manifest、SHA-256、三个镜像 digest 和来源见 [calico.lock.json](../deploy/platform/calico.lock.json)。所选版本官方测试覆盖 Kubernetes 1.36，[kind 安装说明](https://docs.tigera.io/calico/latest/getting-started/kubernetes/kind)要求禁用默认 CNI。本 profile 固定 IPv4 Pod CIDR 10.244.0.0/16、VXLAN Always、无 IPIP/BGP、非 eBPF 数据面；保留原生 kube-proxy。

```bash
curl --fail --location --retry 2 --connect-timeout 15 --max-time 120 \
  https://raw.githubusercontent.com/projectcalico/calico/v3.33.0/manifests/calico-vxlan.yaml \
  -o .local/platform-tools/calico-vxlan-v3.33.0.yaml

make platform-network-create
make platform-network-install
make platform-gvisor-install
make platform-network-test
make platform-delete
```

先按前文准备固定工具和 gVisor 完整发布包。创建完成的 `network_pending` 表示控制面已启动，尚不允许普通任务或 gVisor 验收；安装并验证 Calico、CoreDNS、节点 Ready 后才进入 ready。创建与安装可分开执行，CoreDNS 在无 CNI 时发生的历史 rollout 进度超时不等同于当前不可用，安装器检查其实际 Available 状态，不重启或放宽其期限。

`network-install --calico-manifest PATH` 可指定本地原始 manifest，校验内容后由固定 kubectl 解析 YAML；Python 无新增第三方运行依赖。原始清单和渲染结果都留证，渲染仅增加归属标签、固定镜像 digest 及明确的 VXLAN/IPv4 参数。清单、live 对象归属或已记录 CNI 定义不匹配时拒绝覆盖。ready 状态重复安装只核对，不触发 rollout。

初次安装可使用 `--calico-image-prefix REGISTRY/REPOSITORY` 替换镜像传输前缀，所有组件 digest 保持锁定；已记录的来源变化需重建实验 profile，不在线替换运行中的 CNI。测试镜像可继续使用固定 digest 的镜像源：

```bash
python3 scripts/platform_lab.py create --network-profile calico --dry-run
python3 scripts/platform_lab.py network-test --smoke-image-source \
  docker.m.daocloud.io/library/python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258
```

验收在四个按本次 run 标识创建的 namespace 中部署可信探针。普通和 gVisor 目标放在 Worker，调用方与数据端点放在控制面节点的普通 Pod 中，验证跨节点流量。每个响应携带 run、Pod UID 和监听端口，所有监听在 baseline 先确认可达。

随后按 baseline → 默认拒绝 → 控制/数据/DNS 定向放行 → 撤销放行 → 恢复基线验证。namespaceSelector 与 podSelector 在同一 peer 中组合，覆盖错误 namespace、标签和端口；DNS 单独验证 UDP/TCP，对 Kubernetes API Service 只做无凭据 TCP 连接探测。策略异步生效采用有限收敛窗口与连续两次完整匹配，使用新连接；拒绝场景只认可相应连接/DNS 超时，服务拒绝连接、读取超时、错误响应或 Pod 故障不能算通过。

成功后按 namespace UID/run/归属核对并清理探针，检查 gVisor 进程回收；失败保留现场，最终 `platform-delete` 清理整个本项目实验集群。Calico 系统对象与测试对象分别记录，不能使用此入口卸载已有生产集群的网络组件。结果见[网络验证](validation.md#network-policy)。探针策略随测试 namespace 清理，不自动覆盖普通任务 namespace；真实会话使用的控制端口、namespace 与策略需在接入上游执行协议时另行配置并验收。

本轮只验证受控 IPv4 TCP/HTTP、DNS 与所列 Kubernetes NetworkPolicy 路径，不构成 ICMP/IPv6、hostNetwork、节点/云元数据、任意公网目的地或对抗性多租户安全证明。NetworkPolicy 不代替 Kubernetes RBAC、执行 API 鉴权或产物权限；后续会话层仍需独立验收。

<a id="platform-plan"></a>
## 会话与批任务平台部署计划

运行时与 Calico 网络入口见上节，以下列出完整会话/批任务平台的后续交付要求；不要把运行时 smoke 当作完整平台安装器。顺序与验收门槛以[路线图](roadmap.md)为准。

| 顺序 | 部署内容 | 运维必须核对 |
| --- | --- | --- |
| P1 运行时 | containerd 官方 runsc handler、RuntimeClass、支持策略的 CNI | 实际 gVisor 路径、资源归属和真实网络拒绝；kind 仅作功能环境 |
| P1 会话 | Agent Sandbox CRD/控制器、必要执行服务与路由、单一模板 | 控制器/CRD/SDK/执行镜像版本匹配，先冷创建再固定 WarmPool |
| P2 产品 | Go API/MCP、受限身份与会话访问 | 无宿主 runtime socket/cgroup 权限；密钥不进入任务；未知执行不重放 |
| P3 批处理 | Kueue、固定图片 Job 模板、受控输入与产物 | 会话与批预算分开，Job attempt 输出隔离，提交/取消/TTL 可收敛 |
| P4 策略 | 健康 AdmissionCheck | 最小 RBAC、指标新鲜度、恢复节奏、升级状态兼容 |

Prometheus/Grafana 与最小会话指标从 P1/P2 配套引入，P3 补齐批任务和压测；不等到策略开发后才建立观测。Python 入口负责可重复安装/验收和记录，Go 服务与上游控制器负责长期运行。

### 固定预热池与资源预算

首版按管理员配置固定目标，上游控制器负责补充供给。记录空闲、已领取、创建中、终止中的 Pod 与实际资源；空闲池也消耗预算，不能只统计活动会话。会话 namespace 的 ResourceQuota、Pod requests/limits、批任务 ClusterQueue 配额和节点容量分别核对，配额上限不提供物理资源独占。

池耗尽须有有限等待或明确失败；就绪探针不等于一定有可领取库存。使用过的会话关闭后销毁，不归还干净池。过期关闭与显式关闭按同一身份收敛；回收失败记录并告警，不能通过删除产品记录掩盖残留。总量上限与补池需要预留在途余量，并测试达到 namespace 配额时的行为。

### 输入、产物与控制通道

首版选一种有界存储机制，输入只读、工作目录按会话或 Job/attempt 隔离；镜像预装工具链，不在执行热路径联网安装。会话关闭前导出需保留产物；批任务验证并保存结果引用后再清理 Job，产物保留期单独执行。

默认拒绝非必要网络，保留受限执行控制与必要输入/产物通道。NetworkPolicy 必须由所选 CNI 真实执行；同 Pod sidecar 不自动形成独立网络边界。控制器、产品层、执行 Pod 的 ServiceAccount/凭据分开，执行 Pod 不自动挂载 API token。按资源归属限制文件和结果访问，禁止任意宿主路径。

### 版本、故障与卸载

组件清单记录版本、镜像 digest、CRD API/storage 版本、内核/cgroup/CNI 及可用特性。上游 API 迁移按固定版本说明执行；已有对象不得在不兼容降级后丢失身份或权限。缺少必要能力时停留在已验收阶段，不叠加另一套沙箱控制面规避问题。

故障现场保存 Claim/Sandbox/Pod 或 Job/Workload 的 UID、条件、事件和执行结果边界。命令超时/断线不表示终止成功，无法确认时关闭会话并核对 Pod/执行进程收敛；控制器重启与网络故障不自动重放命令。

卸载先停止新领取/提交并处理活动会话/批任务，再按上游固定版本流程停止补池、清理对象与控制器。共享 CRD、RuntimeClass、节点组件和存储必须确认无其他使用者才移除；输入、产物、凭据与日志按各自保留要求处理。真实操作命令在对应实现验收后补入本页。

<a id="retirement"></a>
## Daemon 退役流程（待执行）

执行前置为 P1 真实沙箱会话和 P2 新 API/MCP 验收完成、调用方已具备切换条件；不等待 P3 批任务、P4 策略或 Koordinator。详细交付门槛见[路线图 P2c](roadmap.md#retirement)。

1. **盘点与留证。**确认旧服务、监听、调用方、配对配置、实例记录及依赖的实际归属，保存源码/构建版本和必要恢复信息；凭据不进入普通日志和公开归档。
2. **停止旧提交。**先停止或切换所有受控调用方，阻止向旧入口新增请求。等待已接收任务结束，或明确取消并确认回收；未知执行结果不自动重放或迁入新会话/Job。
3. **核对清理。**检查任务进程、bundle、cgroup 及残留状态。无法确认时保持旧记录并处理故障，不直接删除 owner/intent 宣告完成。
4. **停止与移除服务。**按下文既有工具顺序先停配对服务、移除 Gateway，再卸载 base；不执行重新配对或重装步骤。不存在配对时只处理本部署的 base。
5. **清退访问与依赖。**撤销旧端点访问和凭据分发，核对服务停止、无残留进程和监听；配置/状态的永久删除单独按保留要求处理，不默认 purge。不得删除新平台或其他服务共用的 runsc、镜像、容器存储及 cgroup。
6. **发布收敛。**旧 Daemon、专用 Gateway 转发和本地运行时从活动构建/部署退出；恢复旧版本只作为显式人工发布操作，不做自动故障回退或任务重放。

退役完成须同时证明：新 API/MCP 在 agentboxd 未安装或未运行时工作；旧入口不再接收新执行；旧工作已回收或有明确未决处置；新平台只由 Kubernetes/containerd 管理运行时。历史文档和证据保留，不要求旧服务继续在线。

目标 API/MCP 使用最小必要 Kubernetes RBAC 与受限上游执行接口，不挂载 containerd/CRI socket、不申请宿主 cgroup 控制权。上游沙箱内执行服务和节点 kubelet/containerd 继续运行，不等于保留 agentboxd。新平台的安装、回退和卸载命令待实现后替代本页默认操作。

<a id="deployment"></a>
## 旧环境与安装参考

- Linux cgroups v2，systemd 249 或更新版本，Python 3.10+。
- 普通用户已执行 `make build`，准备 `.local/gvisor`、sidecar 和 Python rootfs；安装阶段不下载依赖、不构建 Go。
- KVM 模式下，宿主提供可用的 `/dev/kvm`；systrap 需要显式选择，没有自动降级。
- 固定宿主安装需要管理员权限。免密 sudo 为可选配置，不是前置要求。

项目本地准备由普通用户执行；不会覆盖现有 .local 安装：

```bash
make build
bash scripts/setup.sh runsc
bash scripts/setup.sh rootfs
./bin/agentboxd -doctor -runsc .local/gvisor/runsc -platform kvm
```

没有 KVM 时显式使用 systrap；不要自动切换已写入状态所有权的平台。安装程序不下载、不编译，也不自动启动服务。

### 路径与权限

| 路径 | 用途 |
|---|---|
| `/opt/agentbox/bin/agentboxd` | root 拥有的部署二进制 |
| `/opt/agentbox/.local/gvisor` | 固定 runsc 与 sidecar |
| `/opt/agentbox/.local/rootfs` | 基础 Python 文件系统，保留镜像访问权限 |
| `/opt/agentbox/INSTALL.json` | 程序与 runsc 校验值、镜像 digest 和平台 |
| `/opt/agentbox/previous` | 更新前的程序、unit 和 manifest，目录 0700 |
| `/etc/agentbox.env` | 私有配置，0600；不打印 token |
| `/etc/systemd/system/agentbox.service` | 固定服务 unit |
| `/var/lib/agentbox` | 0700 状态目录，包含 ownership、锁和执行记录 |

安装程序和依赖由 root 复制到固定目录；服务不执行工作区中的二进制。镜像目录保留所需访问权限，输入权限显式建立，不依赖进程 umask。

### 历史安装与启动

在项目根目录，由普通用户完成构建，然后执行管理员操作：

```bash
make build
sudo python3 scripts/deploy.py base
sudo systemctl start agentbox.service
sudo python3 scripts/deploy.py smoke
```

首次使用 systrap 时给安装脚本传入 `--platform systrap`。安装脚本不会自动启动服务或设置开机自启；需要开机自启时单独执行 `sudo systemctl enable agentbox.service`。

<a id="config"></a>
## 服务配置

/etc/agentbox.env 使用无引号 KEY=VALUE；解析器不执行 shell，修改后重启生效。AGENTBOX_API_TOKEN 首次随机生成、后续保留，至少 32 字符；AGENTBOX_PLATFORM 为 kvm/systrap；AGENTBOX_LISTEN 默认 127.0.0.1:8080，仅显式 loopback。状态所有权包含平台/rootfs/cgroup，不能删除 owner.json 绕过不匹配。

<a id="pool-config"></a>
### 容量与资源

| CLI | systemd 环境变量 | 默认值 | 含义 |
|---|---|---|---|
| `-mode` | `AGENTBOX_MODE` | `cold` | cold 或 warm |
| `-concurrency` | `AGENTBOX_CONCURRENCY` | 1 | 任务并发，1～8，直到清理完成才释放 |
| `-warm-size` | `AGENTBOX_WARM_SIZE` | 3 | 固定 READY 目标；也作为动态最大值的默认来源 |
| `-warm-refill-policy` | `AGENTBOX_WARM_REFILL_POLICY` | `slots` | slots 在槽满或有排队时退让；idle 在任一前台任务在途时退让，仅适用 warm |
| `-warm-dynamic` | `AGENTBOX_WARM_DYNAMIC` | false | 启用有界 READY 调整，参数见[动态库存配置](#inventory-config) |
| `-max-sandboxes` | `AGENTBOX_MAX_SANDBOXES` | 6 | 所有创建、空闲、领取、运行、销毁实例的总上限，最多 8 |
| `-create-concurrency` | `AGENTBOX_CREATE_CONCURRENCY` | 1 | 按需与后台共享的创建并发，1～4；不受 READY 目标限制 |
| `-queue-size` | `AGENTBOX_QUEUE_SIZE` | 16 | 最多排队请求数，1～128 |
| `-queue-bytes` | `AGENTBOX_QUEUE_BYTES` | 8388608 | 排队请求的解码后文本字节预算，最多 64 MiB |
| `-queue-timeout` | `AGENTBOX_QUEUE_TIMEOUT` | `100ms` | 最大排队时间，1ms～5s |
| `-sandbox-cpu-milli` | `AGENTBOX_SANDBOX_CPU_MILLI` | 1000 | 每实例 CPU 带宽上限，50～4000；1000 表示 1 CPU |
| `-parent-cpu-milli` | `AGENTBOX_PARENT_CPU_MILLI` | 4000 | 沙箱父组静态 CPU 上限，50～4000；AIMD 最大值不得超过它 |

库存目标、执行并发、创建并发分别不得超过总实例上限；例如执行并发 8 需同时显式设置 max-sandboxes=8。扩大可配置范围不增加 CPU/内存配额，也不说明上限配置更快。CLI 接受 Go duration；固定服务配置解析器接受整数 `ms` 或 `s`。创建与排队参数对 Cold/Warm 同时生效；cold 显式启用动态库存会被拒绝。排队预算仅覆盖等待资源名额，预留成功后的创建计入 setup，不占用 queue-timeout。

字节预算计算 code、stdin、request_id、runtime 和 env 键值的字节长度，不是对 Go 堆内存的精确计量。HTTP 原始 body 另有既有上限。

CPU 默认父级 4 CPU 等效带宽、每实例 1 CPU，实例 swap=0、host pids.max=256。内存预算改为启动校验：读取宿主 MemTotal 与可见祖先 memory.max 的最小值，扣除外部保留量；父组默认取剩余额度与 4GiB 的较小值，单实例默认 512MiB，各自 memory.high 为硬上限的 75%。模式下可能同时存在的实例上限乘以单实例内存上限不得超过父组预算，否则拒绝启动：Warm 按 max-sandboxes 校验，Cold 因不存库存且清理前不释放执行槽，按 concurrency 校验。

单实例 CPU 是服务端统一模板，Cold 创建、Warm 初始库存与后续补池都在创建时写入子 cgroup。`500` 对应 `cpu.max=10000 20000`，`1500` 对应 `30000 20000`；整数 milli-CPU 避免浮点换算误差，周期固定 20ms。它是上限，不是绑核或算力预留，也不抬高父组预算。各子组额度可以合计超过父组，竞争时仍共同受父组约束；CPU 超配与内存预算校验的语义不同。

鉴权 status 的 `sandbox_cpu_milli` 报告配置值，不冒充外部修改后的实时读回。修改部署配置后需重启；启动回收旧实例再创建新实例。不支持请求携带 CPU/内存额度，也不在运行中动态调整单实例。需要差异化规格时再处理规格匹配、容量与公平性，当前不拆分多套预热库存。

`parent_cpu_milli` 报告配置的父组上限，默认仍 4000；例如 1000 对应父组 `cpu.max=20000 20000`。动态 CPU 控制可在此上限以下运行，实时值见 QoS 的 `actual_cpu`；配置的 `qos-max-cpu` 超出父组上限会拒绝启动。父组预算通过 CLI/systemd 正式部署，不再仅依赖实验直接修改 cgroup。

| CLI | systemd 环境变量 | 默认 MiB |
|---|---|---|
| `-parent-memory-mib` | `AGENTBOX_PARENT_MEMORY_MIB` | 0（按部署容量计算） |
| `-sandbox-memory-mib` | `AGENTBOX_SANDBOX_MEMORY_MIB` | 512 |
| `-memory-reserve-mib` | `AGENTBOX_MEMORY_RESERVE_MIB` | 512 |

保留量是规划额度，并不为其他服务建立独占内存保护，也不等同于当前可用内存。2GiB 部署可显式使用 parent=1024、sandbox=256、reserve=768、max-sandboxes=4、warm-size=1、concurrency=2；仍须结合业务实际内存调整。宿主共享依赖、管理进程与可见层级之外的竞争需要纳入部署评估。

<a id="inventory-config"></a>
### 动态库存配置

| CLI | systemd 环境变量 | 默认值 | 含义 |
| --- | --- | --- | --- |
| `-warm-dynamic` | `AGENTBOX_WARM_DYNAMIC` | false | 仅 warm 模式可启用 |
| `-warm-min-size` | `AGENTBOX_WARM_MIN_SIZE` | 1 | 最小 READY 目标，至少 1 |
| `-warm-max-size` | `AGENTBOX_WARM_MAX_SIZE` | 0 | 0 沿用 warm-size；有效最大值不超过 max-sandboxes |
| `-warm-adjust-interval` | `AGENTBOX_WARM_ADJUST_INTERVAL` | 1s | 最小目标动作间隔，100ms～60s |
| `-warm-downscale-window` | `AGENTBOX_WARM_DOWNSCALE_WINDOW` | 30s | 缩容推荐稳定窗口，不短于动作间隔，最多 3600s |

满足 `1 <= min <= max <= max-sandboxes <= 8`；创建并发受独立上限和总容量约束，不受 READY 目标限制。CLI 接受 Go duration，固定服务配置使用整数 ms 或 s。100ms 短窗与控制器节拍是内部常量，状态变更还会触发观察和唤醒；动作间隔独立限制目标变化频率。启动仍在第一个 READY 后就绪，目标从最小值起步。

稳定窗口针对缩容建议，不表示“整池无任务”的连续时长。早期实验参数 `-warm-idle-for` / `AGENTBOX_WARM_IDLE_FOR` 不再支持，也没有别名。更新程序与 unit 后，可在 `/etc/agentbox.env` 中设置：

```text
AGENTBOX_MODE=warm
AGENTBOX_WARM_DYNAMIC=true
AGENTBOX_WARM_MIN_SIZE=1
AGENTBOX_WARM_MAX_SIZE=3
AGENTBOX_WARM_ADJUST_INTERVAL=1s
AGENTBOX_WARM_DOWNSCALE_WINDOW=30s
```

配置修改按[更新流程](#base-update)及服务重启生效。

<a id="qos-config"></a>
### QoS 模式

| 配置 | 实际行为 |
| --- | --- |
| `-qos=off` | 静态资源限制，不启动采样 |
| `-qos=observe` | 只读采样 |
| `-qos=observe -qos-cpu-control` | 预演 CPU 预算，不写配额 |
| `-qos=observe -qos-scheduling` | 预演压力门控，不改变接单或补池 |
| `-qos=enforce` | 执行 CPU 控制并读回确认；调度门控仍默认关闭 |
| `-qos=enforce -qos-scheduling` | 同时执行 CPU 控制、压力准入和补池门控 |
| `-qos=enforce -qos-refill-only` | 仅按 PSI 暂停/恢复新的 Warm 后台补池，不修改 CPU 配额或准入 |

observe/enforce 都需显式指定受保护服务：`-qos-protected-cgroup=/sys/fs/cgroup/...`。该路径不得与沙箱子树相互包含。systemd 配置示例：

```ini
AGENTBOX_QOS=enforce
AGENTBOX_QOS_SCHEDULING=true
AGENTBOX_QOS_PROTECTED_CGROUP=/sys/fs/cgroup/system.slice/mockcore.service
```

预演可改为 observe，并设置 `AGENTBOX_QOS_CPU_CONTROL=true`。模式默认 off，CPU 预演、完整压力调度和仅补池三个开关默认 false。父组预算及 CPU 上下界支持固定服务配置，其余策略参数通过 daemon CLI 设置；部署与更新见 [systemd](#deployment)。

| 参数 | 默认值 | CLI |
| --- | --- | --- |
| CPU 下界 / 上界 | 0.5 / 4 核等效带宽 | qos-min-cpu / qos-max-cpu |
| protected CPU PSI 低 / 高阈值 | 10% / 20% | qos-cpu-low / qos-cpu-high |
| 压力确认与最小动作间隔 | 1s | qos-control-interval |
| 恢复前持续低压 | 2s | qos-stable-for |
| 乘法下降系数 | 0.8 | qos-decrease-factor |
| 加法恢复速率 | 0.25 核/秒 | qos-increase-cpu |
| CPU bandwidth 周期 | 固定 20ms | 不动态修改 |

CPU 上下界的 systemd 变量为 AGENTBOX_QOS_MIN_CPU/MAX_CPU；最大值不得超过 AGENTBOX_PARENT_CPU_MILLI。其余 CPU 策略参数使用 daemon CLI。周期固定 20ms，控制参数不表示最优或保证 SLO。策略与故障动作见[CPU 控制](runtime.md#cpu)。

### 仅控制补池

Warm/slots 可设置 AGENTBOX_QOS_REFILL_ONLY=true；与 CPU 控制、完整压力准入、业务借用互斥。observe 只预演，enforce 也不启用 CPU 写入；前台按需创建仍可进行。示例：

```ini
AGENTBOX_MODE=warm
AGENTBOX_WARM_REFILL_POLICY=slots
AGENTBOX_PARENT_CPU_MILLI=1000
AGENTBOX_QOS=enforce
AGENTBOX_QOS_REFILL_ONLY=true
AGENTBOX_QOS_PROTECTED_CGROUP=/sys/fs/cgroup/system.slice/core.service
```

<a id="business-config"></a>
### 业务健康约束配置

-qos-base-cpu 与 -qos-business-file 必须成对设置；systemd 对应 AGENTBOX_QOS_BASE_CPU（默认 0）、AGENTBOX_QOS_BUSINESS_FILE（默认 none）。样本由可信采集器在受保护目录原子发布，不接受模型或任务指定路径。

```text
-qos=enforce -qos-min-cpu=0.5 -qos-base-cpu=1 -qos-max-cpu=2
-qos-protected-cgroup=/sys/fs/cgroup/.../core
-qos-business-file=/run/core/agentbox-health.json
```

```json
{"sampled_at":"2026-10-21T01:00:00Z","window_ms":1000,"requests":100,"rejected":2,"slo_violations":3}
```

JSON 是格式示例，时间须持续更新；五字段必需，窗口 500～5000ms，最多保鲜两个窗口。拒绝/超标允许重叠，但各自不能超过请求数。文件最多 4KiB，禁止最终路径符号链接、非普通文件和全员可写文件；缺文件、缺字段、null、未来/陈旧时间或计数异常都不按健康处理。新鲜的零请求窗口可以表示空闲，但不能用永久全零文件替代采集。这是本地借用规则，与规划中的 AdmissionCheck 最低样本量要求分别定义。

建议放在 /run；采集器覆盖实际业务错误、拒绝及 SLO 超标。策略解释见[业务反馈](runtime.md#business)。

<a id="lifecycle"></a>
## 服务生命周期

unit 使用 `Type=notify` 与 `NotifyAccess=main`。daemon 完成配置检查、状态加锁、cgroup 初始化、残留回收、可信 Python 创建/执行/销毁探测、Warm 最低库存初始化和 HTTP listener 创建后发送 `READY=1`。初始化错误会使 `systemctl start` 失败。手动运行时没有 `NOTIFY_SOCKET`，通知自动跳过。

当前保留 `Restart=no`，运行期可用性以 `/readyz` 为准；清理故障会停止接单，service 的 active 状态不代表仍然可用。

```text
agentbox.service
├── manager
│   └── agentboxd / 控制命令
└── sandboxes
    └── ab-...
        └── Sentry / Gofer / 沙箱任务
```

`Delegate=cpu memory pids` 明确委派范围。daemon 移到 manager 叶节点后才启用子树控制器。停止时先传播请求取消、等待清理；unit 使用 `KillMode=mixed`，先向主进程发送 SIGTERM，让 daemon 完成 runsc 清理；主进程退出或停止超时后对剩余服务进程执行 SIGKILL，保留整个服务范围的兜底。`TimeoutStartSec=30`、`TimeoutStopSec=45`。HTTP drain 和引擎关闭并行开始；空闲库存最多四路清理，停止预算覆盖正在创建的实例完成当前操作、运行任务退出和库存回收。

<a id="warm-enable"></a>
### 切换 Warm

已经安装固定服务时，由管理员更新二进制和 unit，再用编辑器在 `/etc/agentbox.env` 中添加 `AGENTBOX_MODE=warm`，保留原 token、平台和其他配置：

下面适用于独立 base 服务；已配对 Gateway 的部署须先按[配对更新](#gateway)移除组件，再更新和重新配对。

```bash
make build
sudo systemctl stop agentbox.service
sudo python3 scripts/deploy.py base --update
sudoedit /etc/agentbox.env
sudo systemctl start agentbox.service
mkdir -p artifacts
sudo python3 scripts/deploy.py smoke --lifecycle --output "$PWD/artifacts/host-warm-systemd.json"
```

证据路径必须不存在。切回 cold 时修改模式、将 warm-refill-policy 恢复为 slots，并关闭动态库存和仅补池 QoS 等 Warm 专用开关后重启。预热实例是正常库存，smoke 只要求已完成任务的 ID 无残留，并检查总容量；停止服务后仍要求进程完全回收。功能用例先等待可领取库存，满载拒绝用例直接发送且不重试；不会把库存耗尽时合法的 503 当成用户代码执行错误。

<a id="base-update"></a>
## 更新、回退与卸载

更新只替换程序和 unit，保留配置、状态、runsc 与 rootfs：

```bash
sudo systemctl stop agentbox.service
sudo python3 scripts/deploy.py base --update
sudo systemctl start agentbox.service
sudo python3 scripts/deploy.py smoke
```

更新会检查服务已停止、状态锁已释放、执行记录已回收，以及依赖信息未改变。存在残留时先用原服务启动恢复，再停止更新；不会自动删除 owner.json 或执行记录。

单文件使用临时文件与原子替换。保留一份 previous 快照，更新异常时尝试恢复；需要回退时：

```bash
sudo systemctl stop agentbox.service
sudo python3 scripts/deploy.py base --rollback
sudo systemctl start agentbox.service
sudo python3 scripts/deploy.py smoke
```

本阶段不提供 runsc、镜像或平台的自动迁移。此类变化需要先确认实例完全回收，再明确决定状态归档或清除。

### 卸载与重装

```bash
sudo python3 scripts/deploy.py base --uninstall
```

默认停止并禁用服务，删除程序和 unit，保留配置与状态。相同固定配置可重新安装；如果状态所有权不匹配或执行记录未回收，则拒绝安装。

需要一次性完整移除配置和状态时显式选择：

```bash
sudo python3 scripts/deploy.py base --uninstall --purge
```

该操作仅对仍具有有效 INSTALL.json 的托管部署执行，且要求没有未回收记录。已默认卸载后，不能对缺少托管标记的目录盲目执行 purge。

<a id="gateway"></a>
## 旧 Gateway 配对部署

独立 daemon 可直接监听 loopback；配对时改为 Unix socket，由普通用户 Gateway 占用外部端口。配对期间 base 更新/回退/卸载会拒绝操作，必须先按下述步骤移除组件。

先完成普通构建，以及现有 daemon 的安装/更新。安装工具不编译、不下载依赖，也不自动启用或启动服务。已有部署的示例：

```bash
make build
sudo systemctl stop agentbox.service
sudo python3 scripts/deploy.py base --update
sudo python3 scripts/deploy.py gateway
sudo systemctl start agentbox-gateway.service
sudo python3 scripts/deploy.py smoke
```

首次安装 base daemon 使用 `python3 scripts/deploy.py base`，不传 `--update`。启动 Gateway 会通过 Requires/After 启动并等待 daemon。安装前两个服务必须停止、状态锁释放且无未回收任务记录；首次创建专用系统账号，不删除或修改其他账号。

附加安装文件：

| 路径 | 用途 |
| --- | --- |
| `/opt/agentbox/bin/agentbox-gateway` | 普通用户服务二进制 |
| `/opt/agentbox/GATEWAY_INSTALL.json` | 组件归属和二进制哈希，不含凭据 |
| `/etc/systemd/system/agentbox-gateway.service` | Gateway unit |
| `/etc/systemd/system/agentbox.service.d/gateway.conf` | daemon 组、RuntimeDirectory 和附加环境文件 |
| `/etc/agentbox-gateway.env` | 外部与内部 token、Gateway 参数，0600 |
| `/etc/agentbox-gateway-daemon.env` | daemon Unix 地址与内部 token，0600 |

base 的 `/etc/agentbox.env`、INSTALL.json、依赖和任务状态不变。首次安装默认沿用 base API token 作为外部 token，另生成内部 token，保留现有客户端认证方式。附加环境文件排在 base EnvironmentFile 后覆盖 daemon 地址和 token；不会把内部 token 传给外部客户端。

Gateway 环境配置采用无引号 KEY=VALUE：

| 配置 | 默认值/约束 |
| --- | --- |
| `AGENTBOX_GATEWAY_LISTEN` | base 的 loopback 地址，通常 127.0.0.1:8080；首次也可传 `--listen` |
| `AGENTBOX_GATEWAY_TOKEN` | 外部 token，32～4096 可见 ASCII 字符 |
| `AGENTBOX_DAEMON_TOKEN` | 内部 token，同上，且不能等于外部 token |
| `AGENTBOX_GATEWAY_MAX_INFLIGHT` | 8；1～64，增加时同步考虑 unit 的 MemoryMax |
| `AGENTBOX_GATEWAY_TIMEOUT` | 50s；整数 ms/s，1ms～60s |

unit 默认无 capabilities、NoNewPrivileges、只读系统路径、512MiB MemoryMax 和 64 TasksMax。它仍是本机单 worker 功能部署，不提供外网入口或生产高可用承诺。

<a id="unix"></a>
### Unix socket 与凭据

daemon 的 `-listen` 接受显式 loopback IP，或 `unix:/绝对路径`。Unix 父目录须已存在、由 daemon 用户所有、权限为 0700 或 0750，且不能通过符号链接访问。socket 固定为 0660，组由 daemon 的有效主组决定。

监听器持有私有旁路锁，拒绝替换活跃 socket、普通文件、符号链接或其他用户的 socket。仅能确认连接被拒绝、归属正确且检查期间 inode 未变化的残留 socket 才回收。关闭时只删除自身 inode；私有锁文件可留在目录中，systemd RuntimeDirectory 会在服务停止后清理运行目录。

配对模式下：

- daemon 仍为 root，主组为 `agentbox-gateway`；运行目录为 root:agentbox-gateway、0750。
- Gateway 使用同名专用非 root 系统用户/组，通过组权限连接 socket，不能在运行目录中替换端点。
- 外部 token 与内部 token 不同。两份凭据文件均 root:root、0600，由 systemd 管理器读取后传入相应进程；Gateway 不需要直接读取 root 的配置文件。
- `/var/lib/agentbox` 保留 0700，Gateway 不访问 bundle、rootfs、cgroup 或 Docker socket。任务环境继续由 daemon 显式构造，不继承两个服务的 token。

### 配对更新与移除

只更新 Gateway：

```bash
sudo systemctl stop agentbox-gateway.service agentbox.service
sudo python3 scripts/deploy.py gateway --update
sudo systemctl start agentbox-gateway.service
```

凭据保留，文件写入异常会恢复写入前的组件文件。首版不提供独立的 Gateway 二进制历史回退命令。

base 更新/回退/卸载会拒绝仍装有 Gateway 的部署，避免旧 daemon 或目录删除破坏配对关系。先移除组件，保留 Gateway 配置，再更新 base 并重新配对：

```bash
sudo systemctl stop agentbox-gateway.service agentbox.service
sudo python3 scripts/deploy.py gateway --uninstall
sudo python3 scripts/deploy.py base --update
sudo python3 scripts/deploy.py gateway
sudo systemctl start agentbox-gateway.service
```

仅移除组件时执行上面的 stop 和 `--uninstall`，然后 `systemctl start agentbox.service` 即恢复原 base 配置。`--uninstall --purge` 额外删除保留的 Gateway 配置；专用系统账号保留。不会删除 base token、任务状态、runsc 或 rootfs。

`--destdir` 支持在已有暂存 base 安装上生成组件文件，不创建主机账号或操作 systemd。root smoke 在配对模式读取附加凭据，经 Unix socket 直接检查 daemon，生命周期操作不依赖 Gateway 进程存活。

<a id="smoke"></a>
## 服务检查与 staging

`python3 scripts/deploy.py smoke` 默认检查实际服务的就绪、鉴权、manager 归属、父级配额、Python 模块语义、用户退出、超时、输出限额、后台进程和工作目录隔离。

显式生命周期检查会启停服务、注入一次主进程 SIGKILL，并在结束后恢复服务：

```bash
mkdir -p artifacts
sudo python3 scripts/deploy.py smoke --lifecycle --output "$PWD/artifacts/host-systemd.json"
```

它额外检查单实例配额、Sentry/Gofer 归属、满载拒绝、三轮重启、执行中停止、进程回收和崩溃后的状态恢复。输出包含结果和 cgroup 路径，不包含 token；证据文件要求不存在，避免覆盖旧记录。

服务停止或崩溃时，已有 HTTP 请求可能收到结果或连接关闭，不自动重试不确定结果。

无管理员权限时可以先进行文件部署 staging，不管理宿主服务：

```bash
python3 scripts/deploy.py base --destdir /tmp/agentbox-stage
```

staging 是文件验证，不代表 systemd、KVM 或宿主验收通过。再次测试需使用新的目录或通过该 staging 部署的卸载流程清理。

<a id="observability"></a>
<a id="diagnosis"></a>
## 观测与排障

| 顺序 | 问题 | 证据 |
| --- | --- | --- |
| 1. 业务结果 | 成功、拒绝、超时或客户端丢弃发生了什么？ | offered/成功/拒绝/失败分开统计，禁止仅看成功请求分位数 |
| 2. 时间分解 | 请求在排队、准备、执行还是清理？ | 单请求 timings、客户端计划发送与实际发送时间 |
| 3. 供给约束 | 缺 READY、创建产能还是执行名额？ | idle/creating/execution_in_flight、队首等待原因和库存目标 |
| 4. 资源归因 | 谁在消耗或受约束？ | 同一来源的 CPU、PSI、节流、内存与祖先预算 |
| 5. 生命周期 | 创建和清理能否继续收敛？ | 失败计数、cleanup.active/recent、intent 与就绪状态 |
| 6. 对照 | 变更与业务结果是否有可复现关联？ | 固定其余条件的重复对照，同时报告业务保护与沙箱产出 |

CPU 高使用率不等于饱和，PSI 也不直接证明邻居是根因。更多 READY 不能解除执行名额或长期创建产能限制。计算资源之间、阶段分位数之间都不能简单相加减。完整推导见 [性能理论](archive/performance-theory.md)与[引用索引](archive/performance-references.md)。

<a id="sampling"></a>
### 数据有效性

`-qos=observe|enforce` 显式配置受保护 cgroup，采样周期默认 100ms、范围 50ms～1s；off 不启动采样。计数器差分使用实际单调时间间隔。

| 来源 | 范围 |
| --- | --- |
| host | `/proc/pressure`，反映宿主背景，不是容器独有压力 |
| sandboxes | 沙箱父级 cgroup |
| protected | 显式指定且与沙箱树分开的服务 cgroup |
| manager | daemon 实际所在组，可能还有其他进程 |
| ancestor_0 等 | 最多八级可见祖先，记录发现边界与原因 |

祖先包含后代，manager 也可能与某祖先重合，不能将用量相加。私有命名空间看不到的祖先不在报告中，不声称采集整个宿主层级。系统级只采 PSI，没有 `/proc/stat` 利用率。

- CPU 核等效用量为 Δusage_usec/实际间隔微秒；可超过 1。PSI 短窗百分比为 100×Δtotal_usec/实际间隔微秒；avg10/60/300 用于趋势。
- 首次/恢复、目录身份变化、计数回退、非正或超过五个采样周期的间隔都重建基线。
- 主快照年龄或采集耗时超过三个周期、或采样停止时为 stale。有效零为 0，缺测与无效差分为 null；系统 CPU full 始终无有效值。
- 每来源有时间戳；sequence 在进程内递增，HTTP 多次轮询可能返回同一采样，不能按查询次数累加事件。
- 原始文件最多 64KiB，读取不是跨文件原子快照。cpu.max 的 max 与数值 0 不混淆；所有速率都需检查来源与窗口有效性。

I/O/PID 每秒刷新一次，独立时间戳、年龄、读耗时和 stale。io.stat 最多 128 个设备，只提供累计次数/字节，不提供 await。辅助数据超过三秒或主快照过期时不报告速率；按辅助 sampled_at 去重。可选文件缺失不会使 CPU/内存失效，也不会为采集额外委派 I/O 控制器。

### 状态与清理指标

GET /v1/status 报告模式、readiness、目标、实例状态、排队和配置预算。Cold 的 target=0，Warm 的 target 为接受的库存目标。qos.pressure 是压力判断，qos.scheduling 是实际门控，qos.cpu_control 区分建议与实际预算；observe 不表示动作已生效。资源配置值与实际读回值不能混用。

- execution_in_flight 包含领取至清理结束；creation_limit/attempts 与成功、失败、中止创建分开，不能用旧 creation_count 和旧 total_ms 混算均值。
- queue_entered 和 assigned/canceled/timed_out/unavailable/pressure_rejected 分开；立即领取不计入队，数量或字节满也不混入排队耗时。
- 耗时直方图为非累计桶，边界为 1、5、10、25、50、100、250、500、1000、5000、10000ms，null 为溢出，不承诺精确分位数。
- 同一池快照满足 attempts=成功+失败+中止+creating，以及入队=各离队分类+queued。跨组件快照并非原子事务。
- 503 包含拒绝 reason，只有实际入队才有 queue_ms。request_id 用于关联，不提供去重。

清理阶段保持顺序：

```text
runtime_delete → cgroup_kill → cgroup_empty_and_remove
               → output_readers → bundle_remove
```

`cleanup.active` 在操作开始时出现；recent 保存最近 64 个结束阶段，dropped 表示淘汰数量。结果 cleanup_phases 包含实例、起止时间、耗时与错误。无法确认外部资源清理时保留 bundle/intent。事件缓存不持久化；下述运行时控制命令超时快照另行采集有界内核现场。

### 超时现场

create/start/state/delete 控制命令达到上下文截止时，在终止 CLI 前记录 `runtime failure snapshot`：操作名、实例 ID、CLI 与至多三个 cgroup 进程的 status/wchan/stack/io、宿主 PSI、实例内存与 CPU 计数、父组 dying cgroup 数。每个文件最多读取 4KiB，不采集 cmdline、环境、用户代码或任务输出；字段读取失败保留 unavailable，不补成零。现场进入现有结构化日志，不在可能阻塞的运行时磁盘路径额外写诊断文件。

这是截止时的一次采样，不是完整因果追踪；特别是进程空组不代表内核对象已经全部释放。不可中断内核 I/O 仍可能超过用户态超时。

故障快照新增 `storage`，关联 runsc、rootfs 和状态目录的挂载点、文件系统、设备号及设备累计计数。create/delete 开始时采集去重后的设备计数，超时快照保留 `storage_before` 与采样时间，可离线对齐 I/O 增量和命令耗时。设备计数是设备整体活动，不能归因于单个沙箱；tmpfs/overlay 等可能没有可读块设备计数，缺测保留 unavailable。挂载匹配使用路径边界和最长挂载点，不在超时现场遍历或读取可能阻塞的数据目录。

日志不记录用户代码、任务输出或凭据。创建/删除不可中断 I/O 可能超过用户态超时；先保存现场，清理不确定时停止接单并保留记录，不删除 intent 伪装恢复。普通磁盘问题尚未根治；已知限制与实验依据见[验证指南](validation.md#limits)。
