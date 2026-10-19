# 固定 systemd 部署与验收

固定 `agentbox.service` 支持 cold 和一次性 warm 模式，默认 cold。预热池配置与调度语义见 [预热池文档](warm-pool.md)，CPU 配额与压力准入/补池门控见 [QoS 调度](qos-scheduling.md)。动态库存和多节点仍待实现。

## 部署前提

- Linux cgroups v2，systemd 249 或更新版本，Python 3.10+。
- 普通用户已执行 `make build`，准备 `.local/gvisor`、sidecar 和 Python rootfs；安装阶段不下载依赖、不构建 Go。
- KVM 模式下，宿主提供可用的 `/dev/kvm`；systrap 需要显式选择，没有自动降级。
- 固定宿主安装需要管理员权限。免密 sudo 为可选配置，不是前置要求。

## 路径与权限

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

## 首次安装与启动

在项目根目录，由普通用户完成构建，然后执行管理员操作：

```bash
make build
sudo bash scripts/install-systemd.sh
sudo systemctl start agentbox.service
sudo bash scripts/smoke-host.sh
```

首次使用 systrap 时给安装脚本传入 `--platform systrap`。安装脚本不会自动启动服务或设置开机自启；需要开机自启时单独执行 `sudo systemctl enable agentbox.service`。

`/etc/agentbox.env` 采用简单、无引号的 `KEY=VALUE` 格式，支持：

- `AGENTBOX_API_TOKEN`：首次生成随机 token，再次安装保留；至少 32 字符。
- `AGENTBOX_PLATFORM`：`kvm` 或 `systrap`。
- `AGENTBOX_LISTEN`：显式 loopback IP 与端口，默认 `127.0.0.1:8080`。
- `AGENTBOX_CONCURRENCY`：1～4，默认 1。
- `AGENTBOX_MODE`：cold 或 warm，默认 cold；其余池容量与队列参数见 [配置表](warm-pool.md#配置)。
- `AGENTBOX_QOS`：off、observe 或 enforce，默认 off。`AGENTBOX_QOS_CPU_CONTROL=true` 可在 observe 下预演，enforce 自动开启真实控制。两者均需显式受保护 cgroup 路径；采样与执行器失败行为见 [CPU QoS](cpu-qos.md)。
- `AGENTBOX_QOS_SCHEDULING=true`：启用压力准入与补池门控，默认 false；observe 只报告建议，enforce 实际执行。当前是否接单应同时查看 `qos.scheduling.admission_open`。

平台已经写入状态所有权，不能直接修改平台后复用旧状态。配置解析不执行 shell，不输出 token；smoke 脚本读取私有配置后在进程内构造 HTTP 请求，不通过命令行参数传递 token。

## 服务生命周期

unit 使用 `Type=notify` 与 `NotifyAccess=main`。daemon 完成配置检查、状态加锁、cgroup 初始化、残留回收、warm 模式最低库存初始化和 HTTP listener 创建后发送 `READY=1`。初始化错误会使 `systemctl start` 失败。手动运行时没有 `NOTIFY_SOCKET`，通知自动跳过。

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

## 任务退出码与回收

systemd 会回收退出的孤儿进程，真实服务验收暴露了原 `start → 独立 wait` 对短任务的竞态：Sentry 可能先退出，wait 无法取得退出码。

每个任务独占全新实例，cold 在请求到达后创建，warm 提前创建；其 init 只负责待命；任务使用附着式 `runsc exec --process=...` 执行。宿主等待该 CLI，结合 task PID 创建确认和运行时状态区分任务退出与控制故障。用户退出码 125 和 128 都有回归用例。

任务结束后先读资源统计和 OOM 事件，再显式销毁实例及 cgroup，最后删除执行记录和释放并发名额。没有实例复用。控制命令的 CPU/内存仍位于 manager，不包含在单沙箱 Usage 中。

## 更新与回退

更新只替换程序和 unit，保留配置、状态、runsc 与 rootfs：

```bash
sudo systemctl stop agentbox.service
sudo bash scripts/install-systemd.sh --update
sudo systemctl start agentbox.service
sudo bash scripts/smoke-host.sh
```

更新会检查服务已停止、状态锁已释放、执行记录已回收，以及依赖信息未改变。存在残留时先用原服务启动恢复，再停止更新；不会自动删除 owner.json 或执行记录。

单文件使用临时文件与原子替换。保留一份 previous 快照，更新异常时尝试恢复；需要回退时：

```bash
sudo systemctl stop agentbox.service
sudo bash scripts/install-systemd.sh --rollback
sudo systemctl start agentbox.service
sudo bash scripts/smoke-host.sh
```

本阶段不提供 runsc、镜像或平台的自动迁移。此类变化需要先确认实例完全回收，再明确决定状态归档或清除。

## 卸载与重装

```bash
sudo bash scripts/uninstall-systemd.sh
```

默认停止并禁用服务，删除程序和 unit，保留配置与状态。相同固定配置可重新安装；如果状态所有权不匹配或执行记录未回收，则拒绝安装。

需要一次性完整移除配置和状态时显式选择：

```bash
sudo bash scripts/uninstall-systemd.sh --purge
```

该操作仅对仍具有有效 INSTALL.json 的托管部署执行，且要求没有未回收记录。已默认卸载后，不能对缺少托管标记的目录盲目执行 purge。

## 验收方式

`smoke-host.sh` 默认检查实际服务的就绪、鉴权、manager 归属、父级配额、Python 模块语义、用户退出、超时、输出限额、后台进程和工作目录隔离。

显式生命周期检查会启停服务、注入一次主进程 SIGKILL，并在结束后恢复服务：

```bash
mkdir -p artifacts
sudo bash scripts/smoke-host.sh --lifecycle --output "$PWD/artifacts/host-systemd.json"
```

它额外检查单实例配额、Sentry/Gofer 归属、满载拒绝、三轮重启、执行中停止、进程回收和崩溃后的状态恢复。输出包含结果和 cgroup 路径，不包含 token；证据文件要求不存在，避免覆盖旧记录。

服务停止或崩溃时，已有 HTTP 请求可能收到结果或连接关闭，不自动重试不确定结果。

无管理员权限时可以先进行文件部署 staging，不管理宿主服务：

```bash
bash scripts/install-systemd.sh --destdir /tmp/agentbox-stage
```

staging 是文件验证，不代表 systemd、KVM 或宿主验收通过。再次测试需使用新的目录或通过该 staging 部署的卸载流程清理。

## 已执行验证与宿主状态

- `make check` 和 `make build` 通过；包含 Unix 文件及 abstract socket 通知测试、12 项部署与工具测试，以及脚本语法检查。测试禁用 Go 结果缓存，确保子进程中的 Python 脚本修改也会重新验证。
- KVM 与 systrap 的执行集成套件分别通过 17 项测试，包括新增的用户退出码 128 用例。OOM 用例在任务创建确认之后设置测试软水位，避免被运行时初始化覆盖。
- 已在一次性、禁网、私有 PID/cgroup 的 systemd 252 容器中完成 KVM 与 systrap 的真实服务验收：非法配置启动失败、执行、重启、执行中停止、主进程崩溃恢复、更新、回退、保留数据卸载、重装和显式 purge。
- 该容器使用已缓存的 `kindest/node:v1.29.2` 镜像，没有操作现有 kind 节点，没有挂载宿主 cgroup 树或 Docker socket。复现命令为 `bash scripts/integration-systemd-container.sh kvm` 或 `systrap`。
- 2026-10-15 已在本机 systemd 249 上完成固定服务安装和 KVM 生命周期验收，记录见下文。systemd 252 容器与 systemd 249 宿主结果分别记录。
- 以上为 systemd 接入阶段的验证记录。后续预热池实现、验收和基准口径见 [预热池文档](warm-pool.md)，动态 QoS 尚未实现。

## 2026-10-15 宿主 KVM 验收

用户在本机终端完成管理员安装、启动和 `smoke-host.sh --lifecycle`，原始证据保存在本机被 Git 忽略的 `artifacts/host-systemd.json`。本节保留可提交的验收摘要。

| 项目 | 实际结果 |
|---|---|
| 记录时间 | 2026-10-15 02:33:03 UTC（北京时间 10:33:03） |
| Linux | `6.8.0-111-generic` |
| systemd | `249.11-0ubuntu3.23` |
| runsc | `release-20261005.0`，KVM |
| 服务 | `agentbox.service`，`Type=notify`，`Delegate=yes` |
| cgroup | `/sys/fs/cgroup/system.slice/agentbox.service` |
| 最终状态 | `ready=true`；只读核对时为 `active/running` |
| 父级约束 | 4 CPU 等效带宽，memory.high 3 GiB、memory.max 4 GiB、swap 0、pids.max 2048 |
| 单沙箱约束 | 1 CPU 等效带宽，memory.high 384 MiB、memory.max 512 MiB、swap 0、pids.max 256 |
| 运行时归属 | Sentry 与 Gofer 均位于任务 cgroup |

14 项结果均符合预期，且每项 `cleanup_complete=true`：Python 模块和权限、用户异常、退出码 125/128、超时、输出限额、后台子进程、文件系统隔离、新工作目录、三轮重启、执行中停止恢复和主进程崩溃恢复。用户失败用例返回 `failed` 为预期行为。

证据中的 daemon SHA-256 为 `bf4af9080ce1d3de02fc9421b50348a1a2c2ada2623e0607930215daad04f936`，已核对与工作区构建产物、宿主部署二进制及 INSTALL.json 一致；runsc 校验值和镜像 digest 也与安装 manifest 一致。

只读核对确认部署权限：程序目录和二进制由 root 持有、0755；配置 0600；状态目录 0700；unit 0644。没有读取或输出 API token。

本轮宿主验证范围为 KVM 安装与执行生命周期；systrap、更新、回退、卸载重装仍以先前的 systemd 252 隔离容器验证为依据，没有在宿主重复执行。该记录不包含性能基准或动态 QoS 验收。

官方参考：[systemd 委派](https://systemd.io/CGROUP_DELEGATION/)、[systemd 249 service 语义](https://github.com/systemd/systemd/blob/v249/man/systemd.service.xml)、[gVisor OCI 使用](https://gvisor.dev/docs/user_guide/quick_start/oci/)。
