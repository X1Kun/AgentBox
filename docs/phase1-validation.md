# 第一阶段验收记录

日期：2026-10-12。范围：单次冷启动执行闭环，不包含预热池与动态 QoS。

## 环境与依赖

| 项目 | 实际使用 |
|---|---|
| 宿主 | Ubuntu 22.04.5，Linux 6.8.0-111-generic，x86_64 |
| CPU | i7-9750H，6 核 12 线程 |
| Go | go1.25.11 linux/amd64 |
| gVisor | `release-20261005.0`，OCI spec 支持版本 1.2.1 |
| Python rootfs | `python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258` |
| 测试平台 | KVM、systrap，分别运行同一套测试 |
| 测试容器 | 私有 cgroup/PID/网络命名空间，无外网，源码与依赖只读挂载，执行 umask 为 0077 |
| 外层预算 | 6 CPU 等效带宽、5 GiB 内存、2304 个宿主任务 |
| 单沙箱预算 | 1 CPU 等效带宽、512 MiB 硬上限、384 MiB 软水位、禁止 swap |

runsc 从官方发布存储下载并完成 SHA-512 校验，保留 sidecar 目录。实际版本、下载源、校验值与 rootfs digest 保存在被 Git 忽略的 `.local/` 目录。

宿主只读检查确认当前用户可以打开 `/dev/kvm`，`KVM_GET_API_VERSION` 返回 12。普通用户的会话 cgroup 只有 memory/pids，缺少 cpu 委派，无法直接启动本阶段要求的资源控制。因此真实功能验收在明确授权的一次性 privileged 测试容器中完成，没有修改宿主 systemd 配置或挂载宿主 cgroup 树。

## 通过的检查

- `make check`：Go race 单元测试、`go vet`、gofmt 检查、只读 `git diff --check`、Shell 语法检查。
- `make build`：生成 `bin/agentboxd`。
- `bash scripts/integration-container.sh kvm`：16 项真实集成子测试通过。
- `bash scripts/integration-container.sh systrap`：相同 16 项真实集成子测试通过。

审查修复后，权限单元测试分别在独立子进程的 `umask 0022 / 0077 / 0777` 下验证输入权限；真实集成测试统一使用与 systemd 相同的 `umask 0077`，新增两个回归用例。

普通 `make integration` 未设置显式环境时会跳过；上述真实验收使用容器内的显式配置，不以跳过结果代替验证。

## 真实集成用例

| 用例 | 验证结果 | KVM | systrap |
|---|---|---|---|
| `http_execute` | 经鉴权 HTTP 接口执行 Python 并返回结果 | 通过 | 通过 |
| `stdio_and_environment` | stdin、stdout、stderr 与环境允许列表正常 | 通过 | 通过 |
| `guest_input_permissions` | umask 0077 下 UID 1000 可读任务 JSON，stdin 文件仍为私有权限 | 通过 | 通过 |
| `main_module_namespace` | 实际注册的 __main__ 与 globals 一致，顶层函数与 dataclass 实例可以 pickle 往返 | 通过 | 通过 |
| `user_failure` | Python 异常归为用户代码失败 | 通过 | 通过 |
| `user_exit_125_is_not_runtime_error` | 用户退出码 125 不混淆为运行时故障 | 通过 | 通过 |
| `timeout` | 死循环在执行期限后被终止 | 通过 | 通过 |
| `output_flood` | 输出达到合计上限后终止，缓冲保持有界 | 通过 | 通过 |
| `filesystem_and_network` | 根只读、无设备泄露、出口拒绝、下次工作目录全新 | 通过 | 通过 |
| `background_child_is_destroyed` | 主任务退出后回收后台子进程 | 通过 | 通过 |
| `process_limit` | 确认配置 rlimit，并验证降低后的进程拦截 | 通过 | 通过 |
| `memory_oom` | 硬限制触发后识别 OOM，内存峰值达到 512 MiB | 通过 | 通过 |
| `cancellation_and_capacity` | 满载拒绝、取消终止、后续执行恢复 | 通过 | 通过 |
| `restart_reconciles_owned_intents` | 重启清理属于本状态目录的残留 intent/cgroup | 通过 | 通过 |
| `cancel_during_setup` | 创建期间取消仍完成回收 | 通过 | 通过 |
| `cleanup_failure_blocks_admission_and_restart_repairs` | 注入 delete 故障后保留记录、停止接单，重启修复 | 通过 | 通过 |

常规任务执行后检查 bundle 和沙箱 cgroup 不残留。测试临时目录也能正常回收。

## 验收中发现并处理的问题

1. 新版 runsc 默认共享空网络命名空间，会在 runtime root 中留下 `null-netns` 挂载。第一阶段明确指定 `--gofer-network-namespace=new`，以便每次实例生命周期结束后释放，而不是保留共享挂载。
2. 内存分配在 384 MiB 软水位触发回收与限速时，可能先达到执行超时，不能据此宣称 OOM 识别失败。硬限制用例单独把该次任务的 `memory.high` 提高到 512 MiB，以验证 memcg OOM；常规配置仍为 384 MiB。
3. 多进程 KVM 地址空间开销可能先触发内存软水位。因此 fork 用例确认 `RLIMIT_NPROC=(64,64)`，再降低为 `(8,8)` 验证拒绝机制；没有证明 64 个应用进程的精确实测边界。
4. 创建期间的客户端取消归类为 `cancelled`，清理使用独立上下文。
5. 部分 cgroup 配置失败也保留该实例的所有权，不能丢弃回收记录。
6. systemd 的 `UMask=0077` 会过滤文件创建权限。输入准备在创建后显式 chmod：输入目录 0755、任务 JSON 0444、stdin 文件 0600，外层 bundle 保持 0700；保留服务的严格 umask。
7. 用户代码原先执行于独立字典，未对应 `sys.modules['__main__']`。现在创建并注册专用模块，在其 `__dict__` 中执行，并将 `__file__` 与 `sys.argv` 指向任务文件，保证模块相关行为正常。

## 结论的适用范围

- 这些是功能验收，不是正式延迟、吞吐或隔离安全证明。
- 日志中的 setup、execute、cleanup 耗时来自少量功能用例，不能计算有意义的 P50/P99，也不能证明 KVM 固定优于 systrap。
- 集成测试位于 Docker 容器内，不能替代裸机性能实验。
- 尚未启动或安装宿主 systemd 服务；裸机启动方式与模板见 README。
- 尚无预热池、排队、去重、动态 PSI QoS、独立 Gateway、MCP、多节点或 GPU 接入。
- 没有执行 Git 写命令；新增实现与文档等待用户审阅、提交。

## 2026-10-15 rootfs 准备权限回归

- `prepare-rootfs.sh` 解压时添加 `--same-permissions`，保留镜像目录、解释器及库文件的原有权限；根目录与 `/agentbox`、`/workspace` 挂载点显式设为 `0755`。
- 新增 `tests/scripts` 回归测试，在 `umask 0022 / 0077` 下调用实际准备脚本和 tar，仅模拟 Docker 镜像传输；验证可执行权限、库文件读权限、私有文件权限、sticky bit 和 Python 符号链接。
- `make check` 与 `make build` 通过。
- 使用已有真实 Python rootfs 快照，在禁网、资源受限的普通容器中，以 root 和 `umask 0077` 调用实际准备脚本，随后 chroot 并切换为 UID/GID 1000，成功执行 Python 并导入标准库。该检查验证不同所有者下的访问权限，没有使用 privileged 或 Docker socket，也没有覆盖项目现有 rootfs。
- 本次改动未重跑 gVisor 集成套件；上述 UID 1000 检查属于文件权限与解释器可用性验证。
