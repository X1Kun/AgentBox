# 运维指南

当前默认构建和产品调用统一走 Kubernetes 会话 API/MCP/Agent。旧宿主 Daemon/Gateway 及其构建、安装和实验入口已清退；历史版本见[归档](archive/legacy-runtime.md)。Kueue/批任务和健康准入仍待后续交付。

按任务跳转：[平台](#platform-lab) · [gVisor](#gvisor-runtime) · [网络](#network-policy) · [会话](#cold-sessions) · [API](#session-api) · [记录清理](#claim-retention) · [Agent 验收](#agent) · [旧安装退役](#retirement) · [排障](#diagnosis)。命令从仓库根目录执行。

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

### 自动安装与健康等待

准备好锁定 manifest、gVisor 包和 `platform-session-build` 生成的镜像 bundle 后，使用 `make platform-up`。入口复用本项目已有 Calico 集群，或在无活动集群时创建；依次等待控制面 → 准备/安装 Calico → 等待节点 → 安装 gVisor → 再次等待控制面 → 安装上游会话组件。整个流程共用归属锁，健康阶段按已有身份验证并复用，不重启已安装运行时；失败保留现场和已完成阶段，再次执行从已有状态继续。

默认 Calico 镜像前缀为 `docker.m.daocloud.io/calico`，可用 `CALICO_IMAGE_PREFIX=<mirror>/calico make platform-up` 指定；镜像 digest 始终固定，已有 CNI 的来源配置仍须一致。镜像在两个归属节点逐一准备并核对 CRI 摘要，每次缺失镜像准备最多 180 秒，完成后最多等 30 秒同步 CRI 缓存；Pod rollout 继续使用原 300 秒预算，镜像准备与组件启动分开留证。原单步安装入口继续保留用于排障。

`make platform-ready` 是有总期限的只读健康观察，默认 180 秒：检查 API `/readyz`（含 etcd）、四个原生控制面 Pod 和两个节点，连续三个健康样本后放行。CNI 安装前仅允许节点尚未 Ready，其余控制面检查保持。单次探测最多 5 秒，瞬时连接/服务端错误可在总预算内恢复；所有 GET/version 请求还可在其原命令总预算内重试，写入、执行、权限错误及证书错误不自动重试。不是增加固定 sleep 或放宽任务期限。

安装入口负责节点软件和配置的初始化；Kubernetes/kubelet 负责已部署 Pod 的持续运行与重建。这里不新增驻留监控进程去自动改写 containerd 配置。开发期先完成平台健康与安装，再运行产品验收，最后统一执行 `make check`，避免冷启动和全量回归竞争磁盘；成功后保留健康集群，按测试资源归属清理临时 namespace。

### 下载源与超时

工具可换国内下载源，但须匹配上游 SHA-256。create 的 --node-image-source 和 smoke 的 --smoke-image-source 仅允许改变 repository，必须保留锁定的 @sha256:digest；先以相同参数加 --dry-run 核对。canonical 镜像在锁文件，实际来源在报告中；不修改宿主 registry mirror 配置。

此功能 profile 的节点直接访问镜像仓库。创建 kind 时只对子进程清空 HTTP/HTTPS/ALL 代理变量并设置 NO_PROXY=*，避免把宿主 127.0.0.1 代理带入节点。宿主设置保持原值；若必须使用受控出口代理，需要另行明确节点可达配置，不能直接复用宿主回环地址。

节点镜像准备单独有界（pull 最多 600s），随后 kind 创建最多 420s、节点 Ready 检查最多 120s。共享磁盘冷启动曾超过 kubeadm 默认 1m API 引导预算，因此固定 InitConfiguration.timeouts.kubernetesAPICall=4m，并保留失败记录；这只影响集群引导，不改变工作负载时限。[kubeadm 超时定义](https://kubernetes.io/docs/reference/config-api/kubeadm-config.v1beta4/)

Job 固定 activeDeadlineSeconds=120、backoffLimit=0；--timeout（30～600s）仅改变观察等待预算，不改 Job 期限。kind --retain 保留失败节点，超时或中断由归属记录及诊断日志处理。普通可信 smoke 不证明沙箱安全、自动重试或 SLO 收益。

旧单机安装、容器验证与实验脚本已经移出活动代码；仅在[历史版本](archive/legacy-runtime.md)按原环境复现。当前平台由本节已有入口管理。

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

<a id="cold-sessions"></a>
## 上游冷会话与 Go SDK 验收

此入口依赖 **ready 的 Calico profile 和 gVisor 安装**。 安装和会话验收还核对原生 kube-controller-manager 与 kube-scheduler 的 Ready 状态；仅 Node/CNI Ready 不足以继续，失败记录为 `session_control_plane_not_ready`。普通 kindnet 集群需先按归属流程删除再新建。固定组件及校验值见 [session.lock.json](../deploy/platform/session.lock.json)：Agent Sandbox controller/CRD/Go SDK v1.0.8，WarmPool replicas=0，仅验证冷创建。项目 Go 最低版本随上游 SDK 提升为 1.26，固定工具链 go1.27.1；`go` 可按 go.mod 自动选择，无需替换系统 Go。

```bash
# 先准备前述 kind/kubectl、Calico 清单和 gVisor 完整包。
curl --fail --location --retry 2 --connect-timeout 15 --max-time 120 \
  https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.8/sandbox-with-extensions.yaml \
  -o .local/platform-tools/agent-sandbox-v1.0.8.yaml

make platform-network-create
make platform-network-install
make platform-gvisor-install
make platform-session-build
make platform-session-install
make platform-session-test
make platform-delete
```

若已有本项目 ready 的 Calico/gVisor 实验集群，从 build 开始。Go 模块可使用受信任的 GOPROXY 镜像，保留 go.sum 校验。镜像源只允许改变 repository 并保留 digest，例如：

```bash
GOPROXY=https://goproxy.cn,direct python3 scripts/platform_lab.py session-build \
  --session-controller-source k8s.m.daocloud.io/agent-sandbox/agent-sandbox-controller@sha256:e1d96312487eba7d8f001e848a9d4e644b7deea8c2a4564ccc318abb729be731 \
  --session-python-source docker.m.daocloud.io/library/python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258
```

build 核对上游模块版本/校验与工具链，用 `go build -mod=readonly` 构建上游 sandboxd 和本项目的 Go 验收客户端，再将二进制放入固定 Python 基镜像。上游 controller 使用官方发布 digest。Docker 构建只发送临时最小上下文，不发送仓库或凭据；本地镜像按输入哈希命名，bundle 记录来源、二进制 SHA-256、Docker daemon ID 和实际 image ID。变更 Go/镜像输入后必须重新 build，镜像别名被替换时拒绝继续。 仅客户端变化时，sandboxd 可复用前次同 Docker daemon、工具链、SDK 校验和、go.mod/go.sum、锁文件及 runtime Dockerfile 对应的缓存二进制；复用前核对其 SHA-256，来源不同则重编译，内容不符则拒绝。报告单列 reused_binaries。

install 校验官方 release YAML，再由固定 kubectl 解析；部署上游合并控制器及四个 CRD，不自研会话控制器。只增加归属、固定镜像/节点及有界资源配置；现有外来对象拒绝接管。控制器安装后改变版本/模板定义需新建实验环境，不能直接覆盖运行中的 profile。`--session-manifest PATH` 可指定同校验值的本地清单。三个 session 动作支持 `--dry-run`；build 只变更本地构建产物，不变更集群。 镜像准备分为 Docker 导出（最多 600s）和 kind 归档导入（最多 300s）；归档在本地按 bundle 命名，记录 Docker daemon、image ID 与 SHA-256，重试前重新校验。失败导出不发布缓存，内容或身份不匹配会拒绝复用。 每个节点都核对 tag 与实际 image ID；内容已齐全时跳过导入，存在未完成 ctr 操作时拒绝叠加。导入成功后为 CRI 镜像缓存同步保留独立 60s 收敛窗口，超时仍拒绝继续。安装预算与会话执行/清理预算分开。

会话 Pod 在 Worker 经 containerd/runsc 运行，sandboxd 与 Python 在同一容器；非 root、只读根、无 SA token，500m CPU/512MiB 内存，工作目录/tmp 使用独立内存卷。可信 Go 客户端是控制面节点上的一次性普通 Job，通过 headless Service 使用上游 REST 8080 和 gRPC 9090，不使用 gVisor 不支持的 Pod port-forward。该 Job 只是验收客户端，实际脚本在会话 Pod 内执行，其资源与客户端分开记录。

Template 使用上游 Managed 自定义 NetworkPolicy：仅允许本轮独立客户端 namespace **且** Pod 标签访问两个控制端口；会话出口只允许 CoreDNS UDP/TCP 53。客户端有单独策略，只允许 DNS、会话控制端口和精确 Kubernetes API 地址/端口。其 SA 仅能在本轮工作 namespace 创建/读取/删除 Claim 及只读关联资源，不能读 Secret、创建 Pod 或修改模板。任务和 observer 不挂载集群凭据，也不接收模型密钥。

Go 客户端通过官方 SDK 顺序执行：A 写入输入和脚本→计算→读取产物→后续命令读取同一文件；B 校验文件隔离；再验证非零退出后可继续执行、新解释器不保留变量、文件 API 越界拒绝、会话间/API 出口拒绝、关闭、30s 到期和运行中关闭。命令超时只证明调用截止，结果不确定时不重放，随后关闭整个会话并等待资源收敛。host 在检查点独立核对 Claim/Sandbox/Pod/Service UID、CRI handler、runsc/Sentry 和真实 cgroup 限制，并从第三个 namespace 验证非授权入口不可达。最后确认 API 对象、对应 CRI sandbox/运行时进程和本轮 namespace 清理。

输出位于独立 artifacts 目录，包括 JSONL Go 事件、控制器清单、Template/策略、资源身份与运行时证据。失败保留现场，成功清理本轮 namespace；控制器、CRD 和已安装 runtime 保留到 `platform-delete`。Claim 有硬期限，失败清理不能只依赖 Go 进程退出。重跑使用新的 run namespace，不接管旧会话。

本阶段只验证行为，不执行压测或大量测量实验。报告耗时用于诊断，不形成性能结论。

这是可信集群内验收，尚无对外执行 API 鉴权、用户身份关联、产品并发拒绝/输出截断、持久化产物或生产 SLO。sandboxd 的文件接口限制在工作目录，但命令仍可读取其镜像内可访问文件，不能把文件接口边界称为 Pod 内的第二层隔离；emptyDir sizeLimit 也未做精确容量验收。工作文件保留不等于 Python 内存状态保留，模型等待期间仍占用会话资源。暂不决定 idle、暂停或动态会话容量策略。

<a id="warm-pool"></a>
## 固定预热池行为验收

复用上节的构建与安装，新增 `warm` 测试 profile；默认 `platform-session-test` 仍是冷会话用例。组件版本、镜像与执行模板一致，无自研池控制器。

```bash
# 已完成 Calico/gVisor 和 session build/install 后执行。
python3 scripts/platform_lab.py session-test --session-profile warm --dry-run
make platform-session-warm-test
make platform-delete
```

当前实现及九项正向 warm 行为实测与清理已通过，失败和恢复过程见[本轮记录](validation.md#warm-pool)。应先在健康控制面完成 session-install，再运行此入口。

该固定行为夹具使用 `python-warm`，**replicas=2 是未领取库存目标，ResourceQuota pods=3 是工作 namespace 的总 Pod 上限**。两者分别约束库存和总量；已领取会话不占未领取库存，关闭后由上游销毁，再创建新库存。相同配额声明 requests CPU/内存 300m/384Mi、limits 1500m/1536Mi，与三个固定模板 Pod 对齐。它是功能测试预算，不是吞吐建议或物理资源预留。 `pods` 配额按 Kubernetes 口径计入非终态 Pod，Pending 和尚未终态的终止中 Pod 仍占预算；终态历史对象和残留运行时仍需单独清理。

流程先等两个预热成员真实 Ready，记录 Sandbox/Pod UID，再通过 Go SDK 并发领取两个会话，核对身份未换、没有重复领取及文件互不可见。上游在剩余总预算中补出第三个 Ready Pod，再领取它，使三个执行 Pod 占满总量。此时库存目标仍为 2，但可用库存为 0；待创建的 Sandbox CR 不等于已运行的 Pod。

第四次创建只提交一次，使用上游 SDK 的 20s 就绪等待预算。host 同时核对 Claim UID、它拥有的 Sandbox 当前 Ready=False/ResourceQuota 拒绝信息和已占满的 quota；单纯超时、鉴权失败或连接错误不能算耗尽行为通过。SDK 失败回滚后，分别等待该 Claim、Sandbox、Pod/Service 清理。等待预算不包含异步清理的全部时间，也不代表产品端已有容量拒绝接口。

随后关闭已领取会话并核对它们消失，等待库存恢复为两个全新成员；再领取一个，验证没有旧会话标记文件且命令可执行，关闭后再次恢复固定库存。host 在检查点核对真实 gVisor 身份和资源边界。最后先按 UID/归属删除 WarmPool，停止补充，再检查关联 API 对象、CRI sandbox 和捕获的运行时进程收敛，最后删除本轮 namespace。失败保留现场，可用 `platform-delete` 清理整个本项目实验集群；未领取库存不会因 SDK 客户端退出而自行消失。

结束握手的 exec 可能在 Go 主进程退出时返回 137。仅最后一个标记允许记录这种未知回执且不重放；仍须看到本轮完整成功事件、成功 Job 和独立资源清理，否则验收失败。其他标记、其他退出码仍立即失败。

客户端 RBAC 仅新增 ResourceQuota 只读权限，不能调配额、改副本数或创建 Pod。此 profile 不启用动态池、空闲回收、暂停、监控压测或性能测量。控制器重启、Pod 故障恢复、TTL/关闭竞争和产品 API/MCP 仍须后续独立验收。

<a id="session-faults"></a>
## 会话故障行为验收

在同一已安装的 Calico/gVisor/Agent Sandbox 实验集群中运行：

```bash
make platform-session-build
make platform-session-install
make platform-session-fault-test
```

此入口使用零预热副本和顺序创建的受控会话，仅做行为验证。 八项实际行为及清理已通过，范围见[验证记录](validation.md#session-faults)。构建输入改变后按现有安装归属规则准备新实验集群；下载镜像、版本、受限 RBAC 和网络策略仍沿用前文。不会运行付费模型或压测。

Go 客户端先在会话内启动等待文件标记的命令；host 核对本项目安装的 Deployment UID、ReplicaSet 归属及 Pod 镜像后，只删除该上游控制器 Pod，等待旧 Pod 消失、新 UID 的 Pod Ready。客户端再放行该命令，检查输出、文件及 Claim/Sandbox/Pod/Service UID 未变。这项故障不操作原生 kube-controller-manager 或 scheduler。

接着 host 删除本轮指定执行 Pod，DELETE 请求带 UID 前置条件，等待同一 Claim/Sandbox 对应的新 Pod Ready。客户端使用独立 SDK client 显式观察新环境，核对 Pod UID 已变、原 emptyDir 文件消失，并让原身份检查失败；不对旧句柄重放执行。随后关闭变化后的会话，通过新 Claim 验证干净环境可执行。删除 Pod 与容器终态失败不同，此轮不验证 failedPodPolicy=Retain/Recreate 的全部分支。

最后 host 通过 UID 条件补丁缩短本轮 Claim 的 shutdownTime，客户端在该到期点发起显式 Close，允许到期删除或 Close 任一方先完成；检查对象收敛、重复 Close 安全、关闭后拒绝执行。它验证边界处的终态语义，不统计或声称每次都发生相同并发交错。

host 独立核对三组执行 Pod 的运行时和资源归属，最后等待 API、CRI、捕获进程及本轮 namespace 回收。执行 Pod 不挂载集群凭据，Go 客户端没有删除 Pod、重启控制器或 patch Claim 的权限；故障注入仅由绑定专用 kubeconfig 的宿主验收器执行。失败保留现场，最终按现有 platform-delete 归属流程清理。

该 fault 用例本身只验证验收器的 Pod UID 检查；生命周期 API 的持久绑定与归属保护见下节，执行/文件接口另行验证原身份连接与并发边界。本轮不宣称对外多租户鉴权、持久化工作区或故障后透明续接。

<a id="session-api"></a>
## Go 会话服务部署与验收

Go 服务通过上游 Kubernetes API 管理会话，开启执行后访问绑定 Pod 内的 sandboxd，不访问 agentboxd、宿主 socket 或 cgroup。首版可使用显式的受限 kubeconfig 在 loopback 提供 HTTP；省略 kubeconfig 时只加载 in-cluster 配置，不隐式读取默认用户配置。对非 loopback 地址必须配置 TLS cert/key。

```bash
go build -o bin/agentbox-api ./cmd/agentbox-api
# principals.json 为私有文件（0600，或仅同进程组可读且无组写权限），内容是 id/token 对象数组。
bin/agentbox-api --listen 127.0.0.1:8090 \
  --namespace <workload-namespace> --pool <fixed-pool> --instance <stable-instance-name> \
  --kubeconfig <scoped-kubeconfig> --principals-file <private-principals.json>
```

namespace、pool、instance 在安装时固定，重启保持一致；改变 Pool/实例标识需要明确迁移，不能把旧记录当成新配置资源。token 至少 32 个可见 ASCII 字符，启动日志不包含 token 或 kubeconfig 内容。HTTP 有独立头/体/写入期限、4KiB 创建 body 上限及有限在途请求数，满载直接拒绝，不建立等待队列。

[api-rbac.json](../deploy/platform/api-rbac.json) 提供独立 ServiceAccount 和工作 namespace Role：Claim create/get/patch、Pool get，以及关联 Sandbox/Pod/Service 只读。没有 Secret 读取、Pod 创建/删除、模板更改或控制器管理权限。[api-quota.json](../deploy/platform/api-quota.json) 是实验预算示例，约束计算 Pod、声明资源和保留 Claim 数；实际部署须按资源域配置，API readiness 不是容量许可。

生命周期持久状态复用 Claim：request 摘要、归属、首次 UID 绑定、失效和关闭意图使用 API 专属注解，修改携带 UID/resourceVersion 条件；上游控制器仍管理 Sandbox/Pod/Service。关闭使用 Retain，记录不会随服务进程退出丢失。管理员清理元数据前必须确认终止和依赖回收；记录配额耗尽会拒绝新 Claim，不能把保留记录当作无成本。

在完成现有 Calico/gVisor/session 安装后运行 `make platform-api-test`。该验收构建普通 Go API 进程，临时签发仅限工作 namespace 的凭据，并配置两个不同调用身份。验证异步幂等创建、参数冲突、跨身份拒绝、服务重启、Pod 替换后的持久失效、关闭收敛和创建记录保留；host 独立验证实际 gVisor 与运行时回收。凭据只放私有临时目录，不进入镜像、请求日志、报告或执行 Pod，失败保留的测试 namespace 由 platform-delete 限定清理。

执行/文件需显式 `--enable-execution` 并让 API 运行在可达 Pod 网络的可信环境；普通宿主进程默认只提供生命周期。`api.Dockerfile` 与 `api-deployment.json` 提供实验集群的原生部署模板：普通 API Pod 访问受限的上游端口，实际代码仍在 Worker gVisor Pod 中执行。API Pod 独有 ServiceAccount 和 TLS/调用方 Secret，执行 Pod 不挂载这些凭据。模板固定实验控制面节点、imagePullPolicy=Never、单副本，生产部署须适配镜像分发、节点选择、证书和 Secret 管理。Secret 的 principal 文件只允许 owner 或同进程组读取，不允许其他用户访问或组写入。

`make platform-api-exec-test` 构建 Go API 镜像并在原有健康实验集群验证一条 CSV/脚本/产物/后续执行闭环，以及隔离、非零退出、路径边界、并发冲突、运行中关闭、命令超时和输出上限。宿主仅向普通 API Pod 建立 TLS port-forward，不向 gVisor Pod port-forward；执行连接由 API 在集群内建立。临时 TLS/调用身份不进入镜像或报告。成功后清理本次 namespace/运行时，保留开发集群、上游组件和镜像缓存供下次复用，最终释放仍使用 platform-delete。

`/readyz` 由单个后台循环检查 Pool 可访问性，探针只读快照；检查每 2 秒触发，超时不超过 2 秒，5 秒未刷新即返回 503。初始检查完成前也返回 503；`/healthz` 不查询后端。服务退出会取消后台检查。

结构化 `session operation` 日志按 session_id 和 operation_id 关联 acquire、execute/read/write、validate、release、close 阶段；operation_id 是操作标记的摘要，不输出原标记、代码、文件内容或原生错误消息。close 的 pending 仅表示等待回收，complete 才表示已确认回收，request_failed 表示关闭未获确认，需查询并重试关闭。release 的 unknown 表示释放未确认，会在剩余独立清理预算内尝试关闭；不能手工解锁后重放。

控制面请求计数通过 `go test -run TestSessionRequestBudgets -v ./internal/session` 复现，按客户端动作分类。无重试且会话已绑定时，每次执行/文件读写为 23 次调用，包含建连前后两次校验和结果确认；首次就绪查询为 5 次。每轮仍完整检查重复资源、Service selector、运行边界和上游 UID，关闭后仍扫描关联残留。此计数不包含后台 readiness、传输重试或轮询，不是生产延迟/吞吐指标。

操作标记不会因 API 重启自动释放。发现遗留 `operation_active=true` 时先关闭该会话并查询回收，不手工清除标记后继续执行；没有命令结果账本，不自动重放未知命令。MCP/Agent 使用同一 v2 API，接口见[使用指南](usage.md#mcp)。

<a id="claim-retention"></a>
### Claim 记录保留与管理员清理

`agentbox-cleanup` 是一次性管理工具，独立于产品 API；显式要求受限 kubeconfig，不加载默认用户或 in-cluster 凭据。产品 API 的 Role 继续没有 Claim list/delete 权限。[cleanup-rbac.json](../deploy/platform/cleanup-rbac.json) 提供单独 ServiceAccount、工作 namespace Role 和 RoleBinding 模板；按实际 namespace 替换占位符后由管理员安装。清理身份能读取关联资源、patch/delete Claim，但不能创建 Claim、读 Secret 或修改 Pod；它是可信管理员权限，代码归属检查不等于针对恶意管理员的 IAM 隔离。

```bash
go build -o bin/agentbox-cleanup ./cmd/agentbox-cleanup
bin/agentbox-cleanup --kubeconfig /path/to/cleanup.kubeconfig \
  --namespace <workload-namespace> --pool <fixed-pool> \
  --instance <stable-instance-name> --principal <owner-id> \
  --retention 24h --report /tmp/agentbox-claim-preview.json
```

默认无 Kubernetes 写入。检查报告后，以相同范围和保留期增加 `--apply` 并使用新的 `--report` 路径执行。每次报告路径必须不存在，文件为 0600；每次 mutation 前先以原子替换、文件与目录 fsync 保存身份、绑定、摘要和操作意图，再重新读取 Claim 并复核关闭/残留/保留期。删除同时携带 UID 与 resourceVersion 前置条件；不能把同名新对象作为原记录删除。报告写入失败、读权限/传输失败或写结果不确定时停止后续操作，不主动重放不确定写入。

默认保留期 24h，可显式设置整数秒 1s～8760h；判断使用工具 UTC 时钟，部署须保持时间同步；首版不新增常驻 GC 控制器。默认最多扫描 100 条同归属记录，`--max-records` 范围 1～1000；`truncated=true` 表示未扫描完整范围。总预算默认 2m，单次 Kubernetes 请求默认 5s，均有上限。首次完整关闭确认持久化 `closed_at`；旧记录无时间时仅执行 `finalized`，从此次观察开始保留，不能按已过去的 TTL 立即删除。尚未关闭、资源仍存在、时间无效/未来或身份变化的记录均跳过。

预览中的 `would_finalize` 表示尚未持久化的关闭观察，`retain` 表示保留期未到，`would_delete` 表示符合清理资格。实际 `deleted`/`original_absent` 只证明原 Claim 已消失；`deleting` 表示删除已请求但对象仍在，不能当作配额已恢复。`delete_outcome_unknown`/`finalize_outcome_unknown` 需按报告中的原 UID 重新核对，不换对象补删。报告在结果不确定时也保留先前写入的意图和资源关联，供运行时排障使用。此工具只确认 API 对象状态，没有 CRI 权限；此前执行验收中的运行时回收超时仍需独立补验。

`--quota-name` 默认 `agentbox-api-budget`，报告 `quota_before` 为操作前 namespace 总记录配额状态，含其他调用方记录。达到 80% 输出 warning，达到上限输出 exhausted；状态缺失或未观察本代配额时为 unknown，不补零。检查可显式用空名称关闭；自定义配额名需同步调整 Role 的 resourceNames。清理后配额统计可能延迟，应重新查询配额并用受控创建确认恢复。

`make platform-api-retention-test` 使用两个临时 namespace 和独立清理凭据，创建 32 条在提交时就已过期的 Retain Claim，等待上游真实终止确认；验证第 33 条被配额拒绝、默认预览不写、旧记录补时间、保留期保护、显式短保留策略清理后重新创建及 UID 变化。夹具不启动执行 Pod；1h/1s 只用于保留与删除两阶段验收，生产默认仍为 24h。报告和 namespace 清理独立记录，开发集群保留。运行时完整验收仍使用 `make platform-api-exec-test`。

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

<a id="agent"></a>
## Agent/MCP 部署与真实验收

`make build` 只构建 agentbox-api、agentbox-cleanup、agentbox-mcp、agentbox-agent。管理员先安装受控平台与 API，再向调用方提供 API URL、可信 CA 和一个配置身份；不把 Kubernetes 或模型凭据放进执行 Pod。`make tools` 另行构建上游 SDK 验收客户端和可选 mockcore，不参与默认产品路径。

`make platform-agent-test` 或 `make integration` 在当前专用实验集群创建临时 API/执行 namespace，以确定性模型 HTTP 替身启动真实 Agent 与 stdio MCP 子进程。验证 CSV 写入、程序失败修正、报告导出、跨命令文件和新解释器；再验证命令超时，以及经可信 loopback 故障夹具转发一次成功执行后丢弃响应，确认 Agent 不重放、不执行批次后续命令，并在独立预算内关闭会话。API 事件核对实际执行次数，宿主确认三个 gVisor 环境的 CRI/进程回收，最后清理本轮 namespace，保留集群。该入口不调用付费模型。

仅配置本项目开发集群的隔离 kubeconfig，继续使用既有安装归属/版本/UID 检查；不重建健康集群，也不清理其他项目。若真实验收失败，保留原始报告和本轮资源身份，不能改超时或删除失败记录来宣告通过。

<a id="retirement"></a>
## 旧系统安装的一次性退役

仓库内旧运行代码、systemd 模板、直接 runsc 后端及专用 FIFO/Warm/AIMD 控制已删除。历史源代码固定于 c6c2e48；移除文件哈希见[源码退役清单](data/legacy-source-retirement.json)。旧 HTTP/MCP 入口没有兼容别名或自动回落。

`scripts/retire_legacy.py` 只处理固定 `/opt/agentbox`、`/var/lib/agentbox`、`/etc/agentbox.env` 和 agentbox.service；它不是新平台安装器。默认检查，读取私有状态需要 sudo。它核对旧安装 manifest 和 binary/runsc 校验和、固定路径、状态归属、空 bundles/runsc 目录、进程、unit、cgroup 和挂载；未知内容、未回收记录、共享目录或变化后的安装都会阻止操作。apply 持有部署/状态锁，先保存 root 私有的配置/单元/manifest 备份和删除意图，再移除已验证旧安装和凭据入口、禁用并重载 systemd。备份目录仅 root 可读，报告不含凭据值。

```bash
sudo python3 scripts/retire_legacy.py --report /tmp/agentbox-retirement-inspect.json
# 仅在报告 inspected 且核对计划后，使用新的报告路径执行。
sudo python3 scripts/retire_legacy.py --apply --report /tmp/agentbox-retirement-apply.json
```

本机旧 service 已由管理员执行退役，配置备份在 `/var/backups/agentbox-retired-j5l3hhpd`，随后只读复核两项旧 unit 为 not-found/ExecMainPID=0，旧 app/unit/config 路径消失。此前脚本漏列 runsc 目录的阻断轮次及修正后报告分别保存，不把阻断当作已执行删除。新 Kubernetes、宿主共享运行时和工作区缓存均未由该脚本处理。其他机器仍须各自核对迁移，不能以本机记录代验。

<a id="diagnosis"></a>
## 当前排障与升级边界

优先保留 API/MCP/Agent 的 session_id、request_id、identity_revision、错误类别与关闭确认；从上游 Claim/Sandbox/Pod 的 UID、generation、条件、事件及 API operation 日志关联。执行/写入结果未知时停止数据调用并关闭原会话，不手工清除操作标记后重放。关闭未确认和实际运行时回收分别记录；普通磁盘竞争仍可能使 kubelet/containerd 回收延迟。

API 日志不输出代码、文件内容、原始传输错误或凭据。运行时身份只能由拥有专用集群凭据的宿主验收器核对；产品 API/MCP 没有 CRI socket/cgroup 权限。生产部署还需独立评估容量、升级兼容、持久化策略、证书/凭据轮换和监控，当前 kind 行为证据不代表生产 SLO。
