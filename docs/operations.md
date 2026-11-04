# 运维指南

当前默认构建和产品调用统一走 Kubernetes 会话 API/MCP/Agent。旧宿主 Daemon/Gateway 及其构建、安装和实验入口已清退；历史版本见[归档](archive/legacy-runtime.md)。Kueue/图片批任务已提供功能入口，健康准入仍待后续交付。

按任务跳转：[平台](#platform-lab) · [gVisor](#gvisor-runtime) · [网络](#network-policy) · [会话](#cold-sessions) · [批任务](#batch) · [API](#session-api) · [记录清理](#claim-retention) · [Agent 验收](#agent) · [旧安装退役](#retirement) · [排障](#diagnosis)。命令从仓库根目录执行。

<a id="manifests"></a>
## YAML 与 Kustomize 资源组织

资源按组件职责归集，完整布局见[部署资源说明](../deploy/README.md)。`deploy/lab/` 保存 kind 与工具锁；`components/gvisor/`、`components/calico/`、`components/agent-sandbox/`、`components/kueue/` 保存各自配置、镜像文件和版本锁；`agentbox/api/` 与 `agentbox/cleanup/` 保存应用及独立管理员权限。`agentbox/overlays/lab/session/` 与 `batch/` 分别组合产品环境；smoke/gVisor/network/sessions 夹具位于 `tests/fixtures/platform/`。

```bash
python3 -m pip install -r scripts/requirements-platform.txt
make k8s-render
python3 scripts/render_manifests.py --component api --environment lab \
  --set WORK_NS=agentbox-work --set CLIENT_NS=agentbox-client --set RUN=preview
python3 scripts/render_manifests.py --fixture network
```

渲染只调用本地 Kustomize，不连接集群/Docker，不启用远端资源、Helm 或插件。Python 在实验中选择必要资源，并更新明确的动态 namespace/归属、镜像、参数、探针程序与发现的 API CIDR；静态 RBAC、资源预算及环境约束保存在 YAML 中。不引入每个调用一个 overlay、参数 ConfigMap 转发或整段模板替换。实际创建继续经过原 platform 入口的 UID/归属检查。

六份版本锁使用 YAML；`lab/format-compatibility.yaml` 仅在锁值、资源语义与受控补丁精确等价时，承接旧本地状态和镜像缓存身份，不强迫重建健康集群，也不授权版本升级。v1 bundle 的历史输入 ID 只是缓存协议标识，不读取旧文件路径；当前 YAML 字节哈希仍进入源文件证据。历史 JSON、运行报告和生成缓存元数据不回写。

Python 镜像默认通过 [image-sources.yaml](../deploy/lab/image-sources.yaml) 中的 `docker.m.daocloud.io/library/python` 下载，digest 仍取自工具版本锁。该来源已通过本机网络验收；`docker.io/library/python` 本轮曾拉取失败。smoke、gVisor/network 和 session-build 统一读取默认配置，API 构建沿用 session bundle 的已校验来源；不再需要重复传 Python 镜像源参数。显式参数仍可覆盖，且必须保留锁定 digest。其他组件仓库不进行未经验证的整体替换。

<a id="platform-lab"></a>
## 独立 Kubernetes 功能实验

`scripts/platform_lab.py` 编排 kind 和 kubectl，固定一个 control-plane、一个专用 worker，可信 Python Job 在 worker 计算确定结果并校验终态、Pod 归属、输出与清理。它不依赖 agentboxd，不包含 gVisor、Agent Sandbox 会话、Kueue、网络隔离或性能验收。

要求 Linux/amd64、Python 3.10+、Docker Engine 24+ 和 cgroup v2；Docker 可见至少 4 个逻辑 CPU、4GiB 内存只是启动最低条件，不是空闲资源保证。宿主 containerd、KVM、helm 和 k6 不是本入口的必需项。

工具与镜像以 [versions.lock.yaml](../deploy/lab/versions.lock.yaml) 为准。kind/kubectl 固定版本及上游二进制 SHA-256，node/任务镜像固定 digest；containerd 随 node 镜像固定，实际版本从 Node status 记录。kind/node 组合来源见[官方发布](https://github.com/kubernetes-sigs/kind/releases/tag/v0.33.0)。本配置采用 Kubernetes 1.36 的 kubeadm v1beta4；锁定的 kubectl 1.35 属于[官方允许的一小版本偏差](https://kubernetes.io/releases/version-skew-policy/#kubectl)。

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

版本与官方 SHA-512 见 [gvisor.lock.yaml](../deploy/components/gvisor/versions.lock.yaml)。当前固定 release-20261005.0、linux/x86_64、systrap、节点 containerd 2.3.4。runsc、shim 和 gvisor-bin 辅助文件来自同一完整包；仅复制 runsc 不满足安装要求。依据：[官方安装](https://gvisor.dev/docs/user_guide/install/)、[shim 配置](https://gvisor.dev/docs/user_guide/containerd/configuration/)。

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

`--gvisor-archive PATH` 可为 install 指定包位置。test 默认使用统一配置的 Python 源，也可显式指定相同 digest 的其他源：

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

Calico v3.33.0 的官方 VXLAN manifest、SHA-256、三个镜像 digest 和来源见 [calico.lock.yaml](../deploy/components/calico/versions.lock.yaml)。所选版本官方测试覆盖 Kubernetes 1.36，[kind 安装说明](https://docs.tigera.io/calico/latest/getting-started/kubernetes/kind)要求禁用默认 CNI。本 profile 固定 IPv4 Pod CIDR 10.244.0.0/16、VXLAN Always、无 IPIP/BGP、非 eBPF 数据面；保留原生 kube-proxy。

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

`network-install --calico-manifest PATH` 可指定本地原始 manifest，校验内容后由固定 kubectl 解析 YAML，再使用本地 Kustomize 适配；YAML 依赖见 scripts/requirements-platform.txt。原始清单和渲染结果都留证，渲染仅增加归属标签、固定镜像 digest 及明确的 VXLAN/IPv4 参数。清单、live 对象归属或已记录 CNI 定义不匹配时拒绝覆盖。ready 状态重复安装只核对，不触发 rollout。

初次安装可使用 `--calico-image-prefix REGISTRY/REPOSITORY` 替换镜像传输前缀，所有组件 digest 保持锁定；已记录的来源变化需重建实验 profile，不在线替换运行中的 CNI。测试镜像默认使用统一配置，以下无需额外镜像源参数：

```bash
python3 scripts/platform_lab.py create --network-profile calico --dry-run
python3 scripts/platform_lab.py network-test
```

验收在四个按本次 run 标识创建的 namespace 中部署可信探针。普通和 gVisor 目标放在 Worker，调用方与数据端点放在控制面节点的普通 Pod 中，验证跨节点流量。每个响应携带 run、Pod UID 和监听端口，所有监听在 baseline 先确认可达。

随后按 baseline → 默认拒绝 → 控制/数据/DNS 定向放行 → 撤销放行 → 恢复基线验证。namespaceSelector 与 podSelector 在同一 peer 中组合，覆盖错误 namespace、标签和端口；DNS 单独验证 UDP/TCP，对 Kubernetes API Service 只做无凭据 TCP 连接探测。策略异步生效采用有限收敛窗口与连续两次完整匹配，使用新连接；拒绝场景只认可相应连接/DNS 超时，服务拒绝连接、读取超时、错误响应或 Pod 故障不能算通过。

成功后按 namespace UID/run/归属核对并清理探针，检查 gVisor 进程回收；失败保留现场，最终 `platform-delete` 清理整个本项目实验集群。Calico 系统对象与测试对象分别记录，不能使用此入口卸载已有生产集群的网络组件。结果见[网络验证](validation.md#network-policy)。探针策略随测试 namespace 清理，不自动覆盖普通任务 namespace；真实会话使用的控制端口、namespace 与策略需在接入上游执行协议时另行配置并验收。

本轮只验证受控 IPv4 TCP/HTTP、DNS 与所列 Kubernetes NetworkPolicy 路径，不构成 ICMP/IPv6、hostNetwork、节点/云元数据、任意公网目的地或对抗性多租户安全证明。NetworkPolicy 不代替 Kubernetes RBAC、执行 API 鉴权或产物权限；后续会话层仍需独立验收。

<a id="cold-sessions"></a>
## 上游冷会话与 Go SDK 验收

此入口依赖 **ready 的 Calico profile 和 gVisor 安装**。 安装和会话验收还核对原生 kube-controller-manager 与 kube-scheduler 的 Ready 状态；仅 Node/CNI Ready 不足以继续，失败记录为 `session_control_plane_not_ready`。普通 kindnet 集群需先按归属流程删除再新建。固定组件及校验值见 [session.lock.yaml](../deploy/components/agent-sandbox/versions.lock.yaml)：Agent Sandbox controller/CRD/Go SDK v1.0.8，WarmPool replicas=0，仅验证冷创建。项目 Go 最低版本随上游 SDK 提升为 1.26，固定工具链 go1.27.1；`go` 可按 go.mod 自动选择，无需替换系统 Go。

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
  --session-controller-source k8s.m.daocloud.io/agent-sandbox/agent-sandbox-controller@sha256:e1d96312487eba7d8f001e848a9d4e644b7deea8c2a4564ccc318abb729be731
```

build 核对上游模块版本/校验与工具链，用 `go build -mod=readonly` 构建上游 sandboxd 和本项目的 Go 验收客户端，再将二进制放入固定 Python 基镜像。上游 controller 使用官方发布 digest。Docker 构建只发送临时最小上下文，不发送仓库或凭据；本地镜像按输入哈希命名，bundle 记录来源、二进制 SHA-256、Docker daemon ID 和实际 image ID。变更 Go/镜像输入后必须重新 build，镜像别名被替换时拒绝继续。 仅客户端变化时，sandboxd 可复用前次同 Docker daemon、工具链、SDK 校验和、go.mod/go.sum、锁文件及 runtime Dockerfile 对应的缓存二进制；复用前核对其 SHA-256，来源不同则重编译，内容不符则拒绝。报告单列 reused_binaries。

已有安装仅修改 SDK 验收源码时，直接运行 `platform-session-test`：先验证原运行时版本、依赖输入、镜像 ID 和安装 UID，再以当前 `cmd/agentbox-session-smoke/*.go` 的完整输入指纹构建本轮客户端，仅导入该客户端镜像。`session-client-build.json` 记录新源码指纹、二进制/镜像摘要及复用的原 runtime；原安装 bundle 不改写，控制器/runtime 不重装。API、Agent 等不消费 SDK 验收客户端的入口只要求运行时输入匹配，仍核对缓存镜像 ID。旧客户端不会代替改动后的 smoke 程序验收；运行时输入变化仍拒绝复用。完整 `session-build` 继续用于准备新的安装 bundle。

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

<a id="batch"></a>
## Kueue 与固定规格批任务验收

P3a 提供受控批任务的功能底座，依赖已有 ready 的 Calico/gVisor 开发集群和原 UID/归属记录。Kueue v0.20.3 使用 v1beta2 API，上游支持 Kubernetes 1.34+；固定发布清单、官方发布资产 SHA-256 和控制器 digest 见[版本锁](../deploy/components/kueue/versions.lock.yaml)。清单必须预先下载，安装器先校验内容，不在安装中自动下载或升级。

```bash
curl --fail --location --retry 2 --connect-timeout 15 --max-time 120 \
  https://github.com/kubernetes-sigs/kueue/releases/download/v0.20.3/manifests.yaml \
  -o .local/platform-tools/kueue-v0.20.3.yaml
python3 scripts/platform_lab.py batch-install --dry-run
make platform-batch-install
make platform-batch-test
```

安装器只在本项目原有集群中应用校验后的官方 92 个对象，以本地 Kustomize 限定控制器位置、预算、配置和 webhook。控制器在控制面节点运行，声明 100m/128Mi 请求和 1 CPU/512Mi 上限；仅启用 `batch/job`，仅管理带 `agentbox.dev/batch-managed=true` 的 namespace，未命名队列的 Job 不自动接管。非 Job 的第三方工作负载 webhook 已从实验配置中移除，Kueue 自身 API 的校验保留。TLS Secret 仅查询元数据，不把生成的私钥读入状态或报告。

控制器镜像默认从 `lab/image-sources.yaml` 的 `kueue_repository` 获取，保留官方 digest；可通过 `batch-install --kueue-image-source REPOSITORY@sha256:DIGEST` 显式覆盖相同 digest 的来源，`--kueue-manifest PATH` 指定同内容文件。重复安装只验证原资源 UID、实际配置和 Ready 状态，不 rollout；已有定义/镜像源不符时拒绝替换。失败安装保留阶段和已记录对象身份，再次执行同配置继续核对；升级与移除另行制定，不通过覆盖本地状态接管。

批任务资源位于 `agentbox/batch/`，单 Job 的 ResourceQuota 和计算夹具位于 `tests/fixtures/platform/batch/`，图片产品使用独立 lab/batch 入口。每轮验收创建独立 namespace、ResourceFlavor、ClusterQueue、LocalQueue 与预算，队列无 cohort、借用、抢占或健康 AdmissionCheck。500m CPU/256Mi 内存的队列、namespace 配额和 Job 请求/上限一致，最多一个计算 Pod。会话预算继续使用独立 namespace/固定池声明，运行前检查已有 Worker Pod 请求、完整会话上限与批预算之和不超过节点 allocatable；该保守检查不代表物理资源预留，kind 的两个节点仍共享宿主机。

Job 初始 suspend=true，仅由 Kueue 准入后启动。批计算在 Worker 的 gVisor Pod 内直接执行，不调用远端沙箱；非 root、只读根、无 SA token、内存临时卷、默认拒绝网络，backoffLimit=0，明确观察单次尝试。CPU 计算期间验证 Pod UID → CRI handler/sandbox → runsc 进程与 cgroup 的 500m/256Mi、零 swap 边界。Job Complete 后仍核对实际退出码、固定结果 333833500 和 run/Pod 身份。

验收依次覆盖配额占用后的排队/无 Pod、排队取消、忽略 TERM 的运行中父子计算取消、配额释放后下一任务准入与正确结果、单任务超过名义配额、显式退出 23 和 Job deadline。取消使用 API 的 UID 前置条件；清理独立核对 Job、Workload、Pod、CRI sandbox 和捕获的 gVisor 进程。Job 完成后的 TTL 为 600 秒，仅作为残留保底，不作为验收成功条件；夹具结果写在临时工作卷和报告中，尚无独立持久产物服务。

失败时保留本轮 `artifacts/platform-batch-test-*/batch-test.json`，先看原始报告和诊断，再将该实际文件路径传给 `python3 scripts/platform_lab.py batch-cleanup --batch-report PATH`。清理器验证 run、namespace/队列/flavor 名称、原 UID 和本集群归属；不清其他 namespace 或其他 run，也不删除 Kueue 安装。正常验收仅清本轮 Job、队列、配额和 namespace，保留健康开发集群与镜像缓存。

此阶段没有批任务 Go API/MCP、真实图片数据集、可重试产物发布、Prometheus/Grafana 或 R/B 性能实验；后续范围与门槛见[路线图](roadmap.md)。

<a id="batch-images"></a>
## 固定图片任务、Go API 与独立产物

P3b 合并交付固定图片处理、独立产物和 Go 批任务接口。原图来自 scikit-image v0.25.2：chelsea/coffee 为摄影者 CC0，astronaut/Hubble 为公共领域，来源、署名、尺寸、大小和 SHA-256 逐项保存在[数据清单](../deploy/agentbox/batch-images/dataset.yaml)，许可依据链接也随清单保留。库与 cp312/amd64 wheel 的固定版本/摘要见[处理库锁](../deploy/agentbox/batch-images/versions.lock.yaml)；实际像素契约见[contract.yaml](../deploy/agentbox/batch-images/contract.yaml)。

```bash
python3 -m pip install -r scripts/requirements-platform.txt
python3 scripts/prepare_batch_images.py --download
make platform-batch-image-build
make platform-batch-product-test
```

准备脚本只在显式 `--download` 时下载缺失文件，缓存于忽略的 `.local/batch-images/`；已有文件也检查大小、SHA-256、解码和尺寸。构建阶段使用已校验的 Python 基镜像、wheel、原图及固定程序，Docker build 关闭网络，任务热路径也不联网安装或取图。构建后在目标镜像内再次核对八个预期输出的像素摘要，再将镜像导入原 Worker。构建 bundle 同时绑定 Docker 配置 ID、该 CRI 镜像的不可变 manifest digest 和本地内容标签；Pod 上报的 import 日期仓库名是 kind 的导入引用，不把它误认为远程仓库或另一张镜像。

产品资源按职责放在 `agentbox/batch-api/` 与 `agentbox/batch-images/`，继续复用静态队列组件。`python3 scripts/render_manifests.py --component batch-api` 可离线审阅组合资源；默认镜像和零摘要是审阅占位值。实际部署由管理员提供相同 namespace 的 `agentbox-api-credentials` Secret，并设置经过验证的 `API_IMAGE`、`WORKER_IMAGE`、`WORKER_IMAGE_ID`、`WORKER_IMAGE_DIGEST`、namespace/instance 及队列引用；永久数据和凭据不能使用验收生成的临时值。环境入口、存储类和镜像分发按部署环境配置，当前清单固定于同一 lab Worker 的单副本模式。

`agentbox-api --mode=batch` 使用 `--batch-data-dir` 挂载 artifacts PVC，`--batch-template-file` 与 `--batch-contract-file` 指向只读配置，`--batch-image-id` / `--batch-image-digest` 指定同一已校验镜像的两种不可变身份。默认会话模式、原会话 Role 和六个 MCP 工具保持原契约；批任务模式使用单独的 SA/Role，能管理本 namespace 的 Job、读关联 Pod/Workload、固定 PVC/LocalQueue，没有 Secret 读取、Pod 创建/删除或会话 Claim 权限。请求只选择固定 dataset 和有限 passes，不能提交程序或 PodSpec。

独立 `batch-artifacts` PVC 声明 1Gi，kind 使用 standard/local-path 与 ReadWriteOnce，API 和计算 Pod 放在同一 Worker。API 预算 100m/128Mi 请求、1 CPU/512Mi 上限，与计算 Pod 的 500m/256Mi 预算分别计入 namespace 配额（最多两个 Pod）。实际入场前还保守核对已有 Worker 请求、完整会话预算和 API/计算上限。PVC 请求大小不等于物理磁盘硬限额，local-path 的底层存储及宿主 I/O 仍是实验边界；固定可信处理程序另行限制输入、单文件 256KiB、每次输出 2MiB 和记录数，不把小规模成功视为磁盘容量或生产 SLO 证明。

受信任的准备 init 容器只创建当前 Job UID/Pod UID 的目录；实际图片解码容器只挂载该尝试子目录。两者都使用固定镜像、非 root/只读根/无 SA token 与受限资源；计算 Pod 默认拒绝网络，API 仅允许必要 DNS 和发现的 Kubernetes API 地址。API 在尝试根目录内用 Go os.Root 限定读取，检查结果清单、实际 PNG、字节与像素摘要，再原子发布到独立目录。发布、取消和记录更新通过跨进程文件锁串行化；活跃的原子写暂存文件不计入提交目录，孤立暂存文件按相应锁回收。

成功产物默认 24h 后不可下载，后台在原对象回收后删除相应数据；记录保留以承接幂等，默认上限 128，满时明确拒绝。后台扫描按稳定顺序保留进程内进度，预算耗尽后从上一条尝试之后继续，避免前部记录反复占用预算；进程重启后重新开始扫描，持久任务和产物仍按原 UID 恢复。API 重启、客户断线不取消已提交任务。Job TTL 只在结果或失败摘要提交后启用；取消不发布半成品，成功后 DELETE 仅清运行资源。跨 Kubernetes/PVC 没有原子事务，未知提交或执行状态不声明 exactly-once，也不通过新建另一份 Job 掩盖不确定性。

真实验收每轮创建独立 `agentbox-batch-product-<run>` namespace、队列和临时 PVC，验证丢弃提交响应后的同对象恢复、冲突与跨身份拒绝、实际 PNG/像素、Job 删除及 API 重启后下载、损坏产物拒绝、排队/运行取消、Pod 故障后的尝试隔离及单份发布。故障期间的 API/客户端密钥只在私有临时目录与本轮 Secret，报告不读 Secret 数据。正常验收最终确认 Job/Workload/Pod/CRI/运行时、namespace/队列以及此临时 PV 回收，保留原平台与会话镜像 bundle。

失败时保留原始 `batch-product-test.json`，将实际路径传给 `python3 scripts/platform_lab.py batch-product-cleanup --batch-report PATH`。该命令会验证原 namespace UID、run/归属，再清理该验收夹具（包括其临时产物/PVC），不会清其他项目或永久产品数据。结果与范围见[图片批任务验收](validation.md#batch-images)。本轮不提供任意图片上传、批任务 MCP 工具、跨节点存储/调度或性能基线；观测与 R/B 仍在 P3c。

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

[API RBAC](../deploy/agentbox/api/rbac.yaml) 提供独立 ServiceAccount 和工作 namespace Role：Claim create/get/patch、Pool get，以及关联 Sandbox/Pod/Service 只读。没有 Secret 读取、Pod 创建/删除、模板更改或控制器管理权限。[API quota](../deploy/agentbox/overlays/lab/session/quota.yaml) 是实验预算示例，约束计算 Pod、声明资源和保留 Claim 数；实际部署须按资源域配置，API readiness 不是容量许可。

生命周期持久状态复用 Claim：request 摘要、归属、首次 UID 绑定、失效和关闭意图使用 API 专属注解，修改携带 UID/resourceVersion 条件；上游控制器仍管理 Sandbox/Pod/Service。关闭使用 Retain，记录不会随服务进程退出丢失。管理员清理元数据前必须确认终止和依赖回收；记录配额耗尽会拒绝新 Claim，不能把保留记录当作无成本。

在完成现有 Calico/gVisor/session 安装后运行 `make platform-api-test`。该验收构建普通 Go API 进程，临时签发仅限工作 namespace 的凭据，并配置两个不同调用身份。验证异步幂等创建、参数冲突、跨身份拒绝、服务重启、Pod 替换后的持久失效、关闭收敛和创建记录保留；host 独立验证实际 gVisor 与运行时回收。凭据只放私有临时目录，不进入镜像、请求日志、报告或执行 Pod，失败保留的测试 namespace 由 platform-delete 限定清理。

执行/文件需显式 `--enable-execution` 并让 API 运行在可达 Pod 网络的可信环境；普通宿主进程默认只提供生命周期。`agentbox/api/Dockerfile` 与 `agentbox/overlays/lab/session/` 提供实验集群的原生部署模板：普通 API Pod 访问受限的上游端口，实际代码仍在 Worker gVisor Pod 中执行。API Pod 独有 ServiceAccount 和 TLS/调用方 Secret，执行 Pod 不挂载这些凭据。模板固定实验控制面节点、imagePullPolicy=Never、单副本，生产部署须适配镜像分发、节点选择、证书和 Secret 管理。Secret 的 principal 文件只允许 owner 或同进程组读取，不允许其他用户访问或组写入。

`make platform-api-exec-test` 构建 Go API 镜像并在原有健康实验集群验证一条 CSV/脚本/产物/后续执行闭环，以及隔离、非零退出、路径边界、并发冲突、运行中关闭、命令超时和输出上限。宿主仅向普通 API Pod 建立 TLS port-forward，不向 gVisor Pod port-forward；执行连接由 API 在集群内建立。临时 TLS/调用身份不进入镜像或报告。成功后清理本次 namespace/运行时，保留开发集群、上游组件和镜像缓存供下次复用，最终释放仍使用 platform-delete。

`/readyz` 由单个后台循环检查 Pool 可访问性，探针只读快照；检查每 2 秒触发，超时不超过 2 秒，5 秒未刷新即返回 503。初始检查完成前也返回 503；`/healthz` 不查询后端。服务退出会取消后台检查。

结构化 `session operation` 日志按 session_id 和 operation_id 关联 acquire、execute/read/write、validate、release、close 阶段；operation_id 是操作标记的摘要，不输出原标记、代码、文件内容或原生错误消息。close 的 pending 仅表示等待回收，complete 才表示已确认回收，request_failed 表示关闭未获确认，需查询并重试关闭。release 的 unknown 表示释放未确认，会在剩余独立清理预算内尝试关闭；不能手工解锁后重放。

控制面请求计数通过 `go test -run TestSessionRequestBudgets -v ./internal/session` 复现，按客户端动作分类。无重试且会话已绑定时，每次执行/文件读写为 23 次调用，包含建连前后两次校验和结果确认；首次就绪查询为 5 次。每轮仍完整检查重复资源、Service selector、运行边界和上游 UID，关闭后仍扫描关联残留。此计数不包含后台 readiness、传输重试或轮询，不是生产延迟/吞吐指标。

操作标记不会因 API 重启自动释放。发现遗留 `operation_active=true` 时先关闭该会话并查询回收，不手工清除标记后继续执行；没有命令结果账本，不自动重放未知命令。MCP/Agent 使用同一 v2 API，接口见[使用指南](usage.md#mcp)。

<a id="claim-retention"></a>
### Claim 记录保留与管理员清理

`agentbox-cleanup` 是一次性管理工具，独立于产品 API；显式要求受限 kubeconfig，不加载默认用户或 in-cluster 凭据。产品 API 的 Role 继续没有 Claim list/delete 权限。[清理 RBAC](../deploy/agentbox/cleanup/rbac.yaml) 提供单独 ServiceAccount、工作 namespace Role 和 RoleBinding 模板；由编排层注入实际 namespace，再由受归属检查的编排入口安装。清理身份能读取关联资源、patch/delete Claim，但不能创建 Claim、读 Secret 或修改 Pod；它是可信管理员权限，代码归属检查不等于针对恶意管理员的 IAM 隔离。

```bash
go build -o bin/agentbox-cleanup ./cmd/agentbox-cleanup
bin/agentbox-cleanup --kubeconfig /path/to/cleanup.kubeconfig \
  --namespace <workload-namespace> --pool <fixed-pool> \
  --instance <stable-instance-name> --principal <owner-id> \
  --retention 24h --report /tmp/agentbox-claim-preview.json
```

默认无 Kubernetes 写入。检查报告后，以相同范围和保留期增加 `--apply` 并使用新的 `--report` 路径执行。每次报告路径必须不存在，文件为 0600；每次 mutation 前先以原子替换、文件与目录 fsync 保存身份、绑定、摘要和操作意图，再重新读取 Claim 并复核关闭/残留/保留期。删除同时携带 UID 与 resourceVersion 前置条件；不能把同名新对象作为原记录删除。报告写入失败、读权限/传输失败或写结果不确定时停止后续操作，不主动重放不确定写入。

默认保留期 24h，可显式设置整数秒 1s～8760h；判断使用工具 UTC 时钟，部署须保持时间同步；首版不新增常驻 GC 控制器。默认最多扫描 100 条同归属记录，`--max-records` 范围 1～1000；另由 `--max-deletes` 限制候选删除数，默认 100，范围 1～1000，预览与 apply 使用相同预算。保留或待确认的记录消耗扫描预算，不消耗删除预算；补充关闭时间受扫描预算限制。总预算默认 2m，单次 Kubernetes 请求默认 5s，均有上限。首次完整关闭确认持久化 `closed_at`；旧记录无时间时仅执行 `finalized`，从此次观察开始保留，不能按已过去的 TTL 立即删除。尚未关闭、资源仍存在、时间无效/未来或身份变化的记录均跳过。

工具在本轮预算内自动读取后续页，只在页边界停止。`truncated=true` 时报告 `next_continue`；下次使用新报告路径和 `--continue TOKEN` 继续，保持相同 namespace/pool/instance/principal、保留期及预览/apply 模式。游标是有期限的 Kubernetes 分页快照，不是永久任务身份；过期或读取失败会退出，不自动从第一页补删。中途失败的报告保留最后完整页的游标，重试可能重看部分记录，删除前仍复核 UID/resourceVersion。报告完成与否必须同时检查 `error` 和 `truncated`。

预览中的 `would_finalize` 表示尚未持久化的关闭观察，`retain` 表示保留期未到，`would_delete` 表示符合清理资格。实际 `deleted`/`original_absent` 只证明原 Claim 已消失；`deleting` 表示删除已请求但对象仍在，不能当作配额已恢复。`delete_outcome_unknown`/`finalize_outcome_unknown` 需按报告中的原 UID 重新核对，不换对象补删。报告在结果不确定时也保留先前写入的意图和资源关联，供运行时排障使用。此工具只确认 API 对象状态，没有 CRI 权限；此前执行验收中的运行时回收超时仍需独立补验。

`--quota-name` 默认 `agentbox-api-budget`，报告 `quota_before` 为操作前 namespace 总记录配额状态，含其他调用方记录。达到 80% 输出 warning，达到上限输出 exhausted；状态缺失或未观察本代配额时为 unknown，不补零。检查可显式用空名称关闭；自定义配额名需同步调整 Role 的 resourceNames。清理后配额统计可能延迟，应重新查询配额并用受控创建确认恢复。

`make platform-api-retention-test` 使用两个临时 namespace 和独立清理凭据，创建 32 条在提交时就已过期的 Retain Claim，等待上游真实终止确认；验证第 33 条被配额拒绝、默认预览不写、旧记录补时间、保留期保护、显式短保留策略清理后重新创建及 UID 变化。夹具不启动执行 Pod；1h/1s 只用于保留与删除两阶段验收，生产默认仍为 24h。报告和 namespace 清理独立记录，开发集群保留。运行时完整验收仍使用 `make platform-api-exec-test`。

该入口另以每轮扫描 7 条、删除候选预算 2 条进行分页预览，跨报告继续直至覆盖全部 32 条 UID，核对无遗漏、无重复、无预览写入。`make platform-maintenance-test` 在已有观测验收基础上，还验证 LocalQueue Hold 后不接受新任务、已有图片任务仍由后台发布并设置 TTL、被临时权限拒绝的持久取消可恢复，以及这 32 条自然到期 Claim 在产品确认前被观察为 closing。权限和队列故障只限本轮归属资源，并按原 UID 恢复；完成后清理临时资源，保留监控栈与开发集群。

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

P3c 首步提供下述连续观测，固定压测轨迹与 R/B 基线后续单独交付。Python 入口负责可重复安装/验收和记录，Go 服务与上游控制器负责长期运行。

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

<a id="observability"></a>
## 本地 Prometheus/Grafana 与只读观察器

需要原开发集群 ready，以及 Compose v2。监控组件的来源、版本、linux/amd64 manifest digest 固定在 `deploy/components/observability/versions.lock.yaml`。先显式预载其中三张镜像，再启动；入口不会隐式拉取或更换内容：

```bash
docker pull docker.m.daocloud.io/prom/prometheus@sha256:d445d1806b38f85b5c394a1b87f94113fde0473b45245922c2e8b1bd41fa68ae
docker pull docker.m.daocloud.io/grafana/grafana@sha256:6e5d306338670f381a942f1c50d3c266bf71531e5ddd9ee4fc1a133dac5ce63d
docker pull docker.m.daocloud.io/prom/node-exporter@sha256:da83fae85603c4e47e6c68369a7d746e2dda683dc35ea2e234b4f171e0d92798
make observability-up
make platform-observability-test
```

Prometheus 为 `http://127.0.0.1:19090`，Grafana 为 `http://127.0.0.1:13000`，仪表盘 UID 为 `agentbox-observation`。Grafana 用户 admin，随机密码只保存在 `.local/observability/grafana-password`，权限 0600；凭据不进入报告。静态规则、仪表盘源和锁文件均为 YAML，生成的 Grafana JSON 与目标文件在忽略缓存中。Prometheus 保留 2 天/512MB 时序，两者先到即清理；这不是完整长期实验归档，验收另导出原始 query_range 与事件。

验收创建临时会话 API 和图片产品 API，各自选择 `api-metrics-patch.yaml`，自动建立独立 metrics 转发和只读观察范围。它核对程序失败与 HTTP 成功分离、真实 CPU/内存、批任务发布、指标通道丢失、只读权限撤销、观察器重启 initial 事件，以及原有七组图片产品行为。结束时删除本轮工作负载/队列/PVC，停止观察器和转发、移除动态 API 目标，保留集群、监控栈和已归属的基础只读 RBAC。业务故障时原产品报告保留，可按[图片夹具清理](#batch-images)使用其中 `batch-product-test.json` 限定清理；不要直接删除未识别的对象。

已有自管业务实例可手动指定执行资源所在 namespace：

```bash
python3 scripts/observability.py watch --session-namespace agentbox-WORK --batch-namespace agentbox-BATCH
python3 scripts/observability.py stop-watch
make observability-down
```

上面的 namespace 是占位值，替换为本开发集群中已归属的实际名称，至少提供一个 scope。观察器不自行发现其他项目，也不控制业务。`watch` 更新独立只读 Role/Binding、限定 namespace GET 和两个原节点的 `nodes/metrics` GET，凭据为一小时 ServiceAccount token；到期数据变为 unknown，重新执行 watch 刷新。不授予 Secret 或 `nodes/proxy` 权限，不跳过节点 TLS 校验。管理 namespace/SA 和限定节点的只读 ClusterRole 随集群保留，业务 namespace 的 Role 随其回收。down 验证归属后停止本项目监控容器/观察器，保留时序卷，不删除集群。

手动实例还需显式开启 `agentbox-api --metrics-listen=0.0.0.0:9091`（默认关闭），再对 API Pod 使用仅 loopback 的 `kubectl port-forward ... :9091`。业务 TLS/Bearer 监听不承载 metrics，API Pod 可以与执行 namespace 不同。将固定本地转发地址写入 `.local/observability/config/prometheus/targets.yaml`，例如：

```yaml
- targets: ['127.0.0.1:19091']
  labels: {instance: session}
- targets: ['127.0.0.1:19092']
  labels: {instance: batch}
```

metrics 不带业务凭据，只供受控网络/本地转发，不应暴露公网。标签仅包含有限的 operation/status/outcome/domain/role/source 等维度，不包含 request_id、session_id、Job/Pod UID、路径或 token；原始 ID 只用于 JSON 事件关联。所有 scrape 只访问内存快照，不按 scrape 请求量访问 Kubernetes/产物存储。

解读时同时查看 `up`、`agentbox_backend_observation_valid`、`agentbox_inventory_valid` 和带 source 的 `agentbox_runtime_observation_valid`。有效性为 0/序列缺失时不能补零；完整读取后确实为空的库存才报告 0。已有对象在观察器重启时计作 initial，与 live 分开；轮询会漏过短对象，事件计数不能代替后续实验的完整逐请求账本。API/业务计数器随进程重置，API P95 是请求时延，不等于首次可执行或完整批任务时延。

CPU/working set 来自 kubelet `/metrics/resource`；补充 RSS/throttling/I/O 来自可用的 `/metrics/cadvisor` 叶统计。当前 gVisor 的 cAdvisor 叶 CPU 缺失、部分内存为零，不能据此报告无成本；观察器仅在同叶 CPU 序列存在时采用它的补充统计，其他缺项保持未知。CRI 容器 start time 用于区分同名重建，首样本、计数器回退和过期源时间戳不产生速率。两种 source 分别标记有效性；此容器口径不承诺覆盖 Pod 外 shim/Sentry。物理宿主机 `/proc`、`/sys` 由只读 node-exporter 单独采集，CPU/内存/磁盘/PSI 不与容器或 kind 节点相加。端点差异参见 [Kubernetes 节点指标说明](https://kubernetes.io/docs/reference/instrumentation/node-metrics/)。

本次仅验收采集语义和业务兼容性，报告耗时不构成性能收益或 R/B 容量结论。正式 R/B 前仍需冻结到达轨迹、首次/后续命令的逐条时间戳、请求正确性和资源口径，记录采集本身成本；不以当前稀疏样本给出 SLO。

<a id="agent"></a>
## Agent/MCP 部署与真实验收

`make build` 只构建 agentbox-api、agentbox-cleanup、agentbox-mcp、agentbox-agent。管理员先安装受控平台与 API，再向调用方提供 API URL、可信 CA 和一个配置身份；不把 Kubernetes 或模型凭据放进执行 Pod。`make tools` 另行构建只读观察器、上游 SDK 验收客户端和可选 mockcore，不参与默认产品路径。

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

<a id="session-boundaries"></a>
## 固定轨迹的会话边界实验

`make platform-session-boundary-test` 在已有 Calico/gVisor/Agent Sandbox 安装上创建临时固定池、API 和观察范围。配置位于 `experiments/configs/session/session.yaml`，Go 驱动复用真实 `session/client`，Python 负责准备、编排、对账和清理。首批包含顺序结果校准、长短任务与同会话冲突、低高低恢复、命令期限、关闭竞争、突发领取耗尽和观察器失联恢复。

每个计划请求写一条 JSONL 终态记录：计划/发送/完成相对时间、原会话身份、HTTP 状态、correct/wrong/rejected/error/unknown/dropped、输出 SHA-256 和是否在诊断期限内正确完成。请求不会因为服务变慢而重新排队或自动重试；生成器超出 `max_lag_ms` 或自身并发名额不足时记录 dropped，本轮 measurement_valid=false。HTTP 200 仍需退出码和完整预期 stdout 匹配才算 correct。场景之间逐个关闭会话并确认 API 回收；最终核对捕获的 runtime 进程。关闭确认与 close_acknowledged 分别解释。

配置中的 deadline_ms 是本轮诊断阈值，不是生产 SLO。摘要延迟分位数针对所有已发送操作的往返时间，包括失败和关闭；分析首次/后续/故障操作时须按原始账本 ID/action 分组，不能只看该聚合数。计划数=发送数+未发出数；业务结果错误与测量失效分别保留。固定池空闲及补池成本仍在 namespace 预算内，物理宿主与容器资源沿用独立口径，缺失数据保持 unknown。

本入口是有界行为验收。正式 R 容量结论还需要多档负载和独立重复，B/A 对照及崩溃/磁盘写失败等交接窗口实验另行开展。源码、配置、逐条账本与原始 Prometheus 时序留在每轮独立 artifacts 中。

<a id="session-diagnostics"></a>
## 会话请求链路诊断

客户端可通过 context 中的 requestid 传递 `X-Request-ID`，HTTP 层校验并回显；缺失或不合法时生成随机 ID。实验账本保留 request_id、trace_started_at 和 error_code，可把客户端计划/发起/结束时间与服务端 UTC 日志关联。该 ID 只用于日志，不作为 Prometheus 标签，也不提供幂等或重试许可。

API 日志包含 HTTP start/completed/rejected、acquire/execute/validate/release，以及 claim_get、pool_get、sandbox_get、pods_list、services_list、claim_patch、claim_create 的 start/end、分类原因和耗时。对象读取记录 UID/resourceVersion，awaiting_pod 记录 Claim/Sandbox UID；不打印 CAS 标记、原生错误消息、代码、文件内容或凭据。execute 阶段仍包含 RPC/连接校验，不能把它等同 Python CPU 时间。

`agentbox_diagnostics_total{mode,stage,reason}` 和 `agentbox_diagnostic_duration_seconds{mode,stage,reason}` 使用有限 stage/reason，未知输入归 other。可按 acquire/session_busy 查看并发拒绝，按 state_observe/awaiting_pod 查看等待现象，按后端阶段的 timeout/forbidden/throttled/conflict 查看底层分类。pool_get 包括后台 readiness 检查；scrape 本身不会查询后端。

公开响应区分 session_busy、state_conflict、identity_changed、session_not_ready，以及 backend_timeout（504）、backend_conflict（409）、backend_forbidden/backend_throttled/backend_unavailable（503）。backend_forbidden 表示依赖返回 Forbidden/Unauthorized，不能仅据此断定是 RBAC 而非配额准入；具体归因需结合 Events 和控制器日志。异步 creating 的 awaiting_* 继续作为 View.reason 返回，不伪装成已失败或已清理。

边界实验结束前保存 api-events.jsonl、kubernetes-events.json 和按本轮 namespace 筛选的 controller-events.log。失败另保存对象状态快照。清理后仍能沿 request ID → session ID → Sandbox/Pod UID 对账；控制器日志可能因保留窗口而缺失，缺失时保持根因未确认。

<a id="session-matrix"></a>
## 多 Pod、负载与用户行为矩阵

`make platform-session-matrix-test` 使用 `experiments/configs/session/matrix.yaml`，对照 2/4 个执行会话，固定每 Pod 规格与总 namespace 上限 7 Pod（含固定池目标 2 和回收余量）。CPU/内存 requests/limits 随该总上限显式配置，原开发集群与产品默认配额不变；这个上限不代表物理独占，也不是可扩展吞吐量结论。

场景包含同构短任务并发、Alice 长计算与 Bob 多会话短任务、32MiB 有界内存、128KiB 文件读写/摘要、8KiB 输出、预期非零退出、占用但不发命令的会话、主动关闭、突发领取，以及 800ms 客户端截止。文件位于会话内存卷，本场景不测物理磁盘性能。每条代码先验证独立会话 marker，完整 stdout 与预期退出码共同决定正确性。

Go 驱动按 target.user 或显式 request.user 选择真实配置的 HTTP 客户端。`--users-file` 是 0600 的 actor/token 映射，只保存在本轮私有目录，不能进入账本或 Prometheus 标签。每个多用户场景还主动验证其他身份 GET/execute 返回 404。结果按 user 输出数量、正确及时成功及分组时延，保留从未发出命令的会话 ID；这些占用记录不能等同于实际 CPU 空闲时间。总分位数含失败与控制操作，不能代替各用户、各负载分组结果。

客户端截止只取消本次等待，unknown 不重放；最终由独立关闭查询和 namespace/CRI 进程核对确认回收。预期程序退出 23 可以是正确的失败表达；不将它与请求传输错误混合。所有质量未达标与准备失败仍保留日志、Events、快照和原始账本。

可用 `python3 scripts/platform_lab.py session-matrix-test --matrix-scenarios burst,disconnect,parallel-four` 选择独立诊断批次；报告保存选择和实际配置，跨批次不拼接为单次性能成绩。突发创建的每个响应均保留，503/504 不补算成功；清理仅查找相同请求确定名称、同一 namespace/owner/instance 的已存在 Claim。准备阶段的命名创建和关闭控制可在原 180s/90s 共享预算内恢复瞬时错误，同一 request_id/参数保持不变；execute/write 从不重放。场景按低频基线、混合、代码类别、空闲关闭、突发、客户端截止、四 Pod 排列，保留每个阶段的成功和失败。


<a id="static-baseline"></a>
## R/B 静态基线

在已归属、健康的 Calico/gVisor 会话集群上准备 Kueue、图片 worker bundle 和观测镜像；沿用前文已有安装命令，不重建健康集群。运行 `make platform-static-baseline`，默认只进行 R 标定；离线查看计划用 `python3 scripts/platform_lab.py static-baseline --dry-run`。另存冻结配置后可通过 `--baseline-config PATH` 运行，实际配置和内容摘要进入证据。

默认 [baseline.yaml](../experiments/configs/session/baseline.yaml) 使用固定预热目标 2、已领取会话 2、会话 namespace 总 Pod 预算 4。R 不提交批任务，B1/B2 分别提供 500m/256Mi、1000m/512Mi 的 Kueue 配额，对应固定 500m/256Mi Job 的一/两个并发名额。所有组保持相同 namespace 总预算、镜像、API、观察器和 Pod 规格；R 也部署空闲批 API，控制测量设施差异。Job 使用真实锁定图片及同一 passes；不通过 sleep 填充计算。

30s 测量窗口覆盖低频稳态、高频稳态和低→高→低轨迹。R 标定先运行低峰值负载，每档五轮；业务质量未达标仍继续记录，生成器或观测失效则停止。本次配置的及时成功目标为 3s 内正确完成至少 95% 的全部计划请求，是实验目标，不是已证明的生产 SLO。进入对照时运行 `python3 scripts/platform_lab.py static-baseline --baseline-stage comparison --reference-report PATH/static-baseline.json`：报告须来自相同冻结配置、原平台及镜像且已完成回收。只有已有五轮有效且达到目标的轨迹才进入 R/B1/B2 对照，未达标的档位不静默带入；高档标定中断时，已独立完成的低档证据可以引用，报告同时保留标定不完整的事实。

对照按固定种子交错组序，包含重新运行的 R 控制组。会话与批提交使用同一绝对起始时间，先领取、校验 marker 并预执行命令，再进入后续命令窗口；领取和预执行成本另列。每轮都保留原失败和所有预定请求；没有用户执行/文件写重试，批任务提交也只尝试一次。丢弃、过度迟到、观测缺失使该轮测量无效并停止后续轮次；没有可确认身份的未知批提交也阻止进入下一组合，不能让持久提交意图在后续组合恢复而污染对照。

每个新输出目录包含 `static-baseline.json`、`rounds.csv`、逐组合 trace、session JSONL、batch ledger、带上游时间戳的 Job/Workload/Pod 快照和 Prometheus 原始时序。及时成功以全部计划请求为分母，P50/P95/P99 从逐请求样本计算，空集合为 null。图片产物逐个下载并核对字节及像素；完成时间是被验证的观察上界。窗口外 drain 的完成不计窗口吞吐，未完成任务和积压保持可见。下一组合开始前先取消/回收本组合 Job，确认没有 Workload 或 compute Pod；无法解决的提交不带入下一组合。

容器成本仅积分明确采到的 CRI 叶容器 CPU 和工作集，相邻样本间隔超过 10s 不插值，保留实际覆盖秒数；CRI/cAdvisor、宿主/容器不重复求和。短任务未采到、进程在 Pod 外的成本及缺失资源仍为 unknown，宿主 PSI 与资源原始序列用于诊断，不自动解释成批任务因果。五轮共享一个开发宿主和安装，不能当作五台独立机器或生产 SLO；30s 窗口是有界基线扫描，不是长期稳态吞吐证明。最高已测通过负载也只是该配置下的下界，未跨过失败区间时不报告最大容量。

实验结束独立清理本次会话、批任务 namespace、队列、flavor 和临时卷，并确认捕获的运行时进程；诊断失败仍尝试归属校验后的删除。保留集群、安装组件与本地监控服务。R/B 结果用于选择后续验证的静态候选配置；P4 的正式 B/A 比较需要冻结该候选及另行验证轨迹，不能把扫描中事后最优点当成已部署策略。

如果回收超时，先确认原进程退出，再运行 `python3 scripts/platform_lab.py static-baseline-cleanup --baseline-report PATH/static-baseline.json`。恢复入口验证记录的 run、归属、namespace/队列/flavor 名称与 UID，接受已经删除的 namespace，将复核结果写入新的 artifacts 目录；原失败报告保持不变。它只恢复清理，不续跑或补算未完成的性能轮次。

<a id="diagnostic-details"></a>
## 观测细节与故障归档

`make platform-diagnostics-test` 使用[诊断场景](../experiments/configs/session/diagnostics.yaml)，在独立 namespace 临时撤销本轮 batch 观察器 Role 的读取权限，确认 `403/forbidden`、session 观测与执行不受该故障影响，并在 UID/resourceVersion 保护下恢复原规则。它有意触发 `baseline_observation_invalid`：`static-baseline.json` 保留负向状态，独立 `diagnostics-acceptance.json` 表示诊断、恢复与回收检查是否通过，不产生合格容量基线。

库存按 domain、节点指标按 node/source 独立使用 4s 预算；库存与运行时使用独立采集循环。读取日志携带 poll ID、domain、GET/LIST、resource、namespace、分类原因、HTTP 状态、耗时和剩余预算。Prometheus 新增 `agentbox_inventory_requests_total`、`agentbox_inventory_request_duration_seconds_bucket`、`agentbox_runtime_polls_total` 和 `agentbox_runtime_poll_duration_seconds_bucket`。指标只用有限分类，实例、请求、Pod 和进程 ID 保留在日志。失败保持 unknown，预算隔离不会将缺测改为零。

会话 API 的 sandboxd 日志以同一 request ID、session ID 和 Pod UID 关联以下阶段；gRPC 拨号回调会保留连接自身的取消/截止语义，同时恢复原请求关联。Init/Exit 中的 process ID 是上游句柄，不等于宿主 PID。

| 阶段 | 时间范围 |
| --- | --- |
| runtime_validate_before / runtime_validate_after | 建连前/后的原身份校验 |
| runtime_connect | TCP 建连 |
| runtime_rpc_start | 发起 Start RPC；包含上述拨号及校验的嵌套耗时 |
| runtime_wait_init | Start 返回流到收到进程 Init |
| runtime_first_output | Init 到客户端收到第一段 stdout 或 stderr；无输出则不产生样本 |
| runtime_process | Init 到客户端收到 Exit |
| runtime_stream_finish | Exit 到流完成，或结束阶段的错误 |

这些区间存在嵌套，不能将八项相加当作总时长；客户端接收时间也不能替代服务器 CPU 时间。日志不写用户代码、输出正文或原生错误消息。细分原因用于诊断，执行未知仍按原契约关闭并且不重放。

静态基线另保留 `host-diagnostics.jsonl`（每 5s 的 PSI、磁盘计数、内存和可读取进程 I/O）、`controlplane-diagnostics.jsonl`（每 30s 的 API Server 请求/等待及 etcd WAL fsync/backend commit 直方图）和清理期间的 `cleanup-resources.jsonl`。后者按记录的 namespace UID 检查对象，保留 Pod/PVC/finalizer/事件原因与 CRI 状态；清理操作自身的开始、结束、结果和资源关联见 teardown_timeline。权限不足、采样缺失和超限明确记录，不补零；控制面直方图是聚合证据，不能自动证明某次请求的根因。

成功和异常路径都在停止观察器后，将本轮时间范围的日志冻结为 `observer-events.jsonl`，另归档 `prometheus-final.json`。查询中途失败仍保存成功部分和 unavailable 标记，再返回 `diagnostic_metrics_incomplete`；所有归档失败均经过独立处理，namespace 清理继续尝试。采样器有单独的停止/失败状态，诊断子进程限制时间和输出字节；清理结束后不留下后台采样进程。


<a id="product-entries"></a>
## 独立产品部署入口

应用入口为 `deploy/agentbox/overlays/lab/session/` 与 `batch/`；lab 根目录不再组合不相关资源。完整私有清单由 `scripts/render_product.py` 生成，再由管理员显式 apply，资源长期保留。底层 Calico/gVisor/Agent Sandbox 与 Kueue 沿用各自平台安装入口。准备命令、显式输入、TLS/凭据和 Service 访问方式见[部署资源说明](../deploy/README.md)。

`python3 scripts/platform_lab.py product-images --with-batch` 在已校验的原节点上准备 API 镜像和 immutable 引用，并生成两份非秘密 inputs YAML。OCI manifest、config ID 和同配置可能出现的导入别名分别处理；选择具体 tag 的真实描述符并校验 config ID，不从 CRI RepoDigests 任取一个。补充缓存引用不升级 runtime 或修改已有安装 bundle。批 API 支持与配置 digest 精确一致的 worker 引用；旧受控本地 tag 保持兼容，其他可变 tag 仍被拒绝。

最终 session 清单包含 12 个对象，batch 包含 15 个，均包括所需 Namespace、Service、Secret、RBAC、网络和各自预算。图片产品的 requests 预算为 600m/384Mi；500m/256Mi 的单 Job 配额只在验收夹具中使用。清理管理员权限独立选择。镜像/ID/digest 占位值、凭据不完整和已有 namespace 归属冲突会阻止渲染或检查，私有输出为新建的 0600 文件，不进入 stdout。

验收夹具已迁至 `tests/fixtures/platform/`，会话实验参数迁至 `experiments/configs/session/`。平台 session 身份使用 v3 的锁与控制器补丁；SDK Job、RBAC 和 Dockerfile 另记验收身份，runtime-only 校验不读取 SDK 文件或要求缓存 SDK 镜像。旧记录仅在平台字段与已知旧记录精确匹配时兼容；模板变更另体现在产品组合身份，真实 UID 与 bundle 校验继续保留。

`make platform-product-entry-test` 使用同一完整清单渲染器，在临时归属 namespace 验证执行、文件、真实图片产物和回收。它不接管或清理管理员显式部署的长期实例。批产品仍受当前有限记录容量和持久存储身份契约约束，改镜像/模板不能绕过已有数据与幂等保护。
