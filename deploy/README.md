# 部署资源

这里按组件职责组织静态 YAML、原生配置和版本锁。Kustomize 负责组合和固定环境差异；Python 负责每轮实验的资源选择、namespace/归属、镜像、命令、运行时发现、验证和清理。

```text
deploy/
├── lab/                         # kind 配置、镜像源、工具版本锁、格式迁移身份表
├── components/
│   ├── gvisor/                  # RuntimeClass、runsc.toml、版本/归档锁
│   ├── calico/                  # 上游版本/镜像锁、lab 覆盖补丁
│   ├── agent-sandbox/           # Template、WarmPool、控制器补丁、runtime 镜像
│   └── kueue/                   # 发布锁、控制器配置、batch/job webhook 补丁
├── agentbox/
│   ├── api/                     # Deployment、RBAC、Dockerfile
│   ├── cleanup/                 # 独立管理员 RBAC
│   ├── batch/                   # 隔离队列、Worker flavor、namespace 与网络边界
│   ├── batch-api/               # 独立批任务 API、权限、PVC 与预算
│   ├── batch-images/            # 原图清单、库锁、像素契约、工作镜像与 Job 模板
│   └── overlays/lab/            # 应用环境的节点约束、拉取策略、配额
└── acceptance/
    ├── smoke/
    ├── gvisor/
    ├── network/
    ├── sessions/                # SDK 客户端、网络策略、预热/配额夹具
    └── batch/                   # 计算、超配额和 deadline Job 夹具
```

`agentbox/api/`、`agentbox/cleanup/`、`components/gvisor/`、`components/agent-sandbox/` 是组件组合入口；`agentbox/overlays/lab/` 是应用的实际环境覆盖。验收夹具各有一个组合入口，不为每种 Python 调用再建立 overlay。Calico 和上游控制器的发行清单按锁文件校验后，由 Python 使用本地 Kustomize 补丁适配，不在构建时抓取远端资源。

```bash
python3 -m pip install -r scripts/requirements-platform.txt
make k8s-render
python3 scripts/render_manifests.py --component api --environment base
python3 scripts/render_manifests.py --component api --environment lab \
  --set WORK_NS=agentbox-work --set CLIENT_NS=agentbox-client --set RUN=preview
python3 scripts/render_manifests.py --fixture sessions
kubectl kustomize deploy/agentbox/overlays/lab
```

以上只渲染，不联系 Kubernetes 或 Docker。默认值是审阅样例；实验实际创建由 `scripts/platform_lab.py` 的现有入口执行，保留专用 kubeconfig、版本、UID/归属和回收检查。Python 只更新明确的动态叶子字段，不替换整段 YAML、不生成静态权限/预算，也不引入参数 ConfigMap 转发层或全局 namespace/selector 变换。实验命令与动态 API CIDR 仍由编排层注入。

版本锁位于 `lab/versions.lock.yaml` 和四个组件各自的 `versions.lock.yaml`；原有版本、镜像 digest、下载摘要不因迁移改变，Kueue 在 P3a 独立锁定。版本/摘要应保留为字符串。YAML 采用安全加载器并拒绝重复键和可执行标签；普通检查要求可用 kubectl/Kustomize 和 PyYAML，当前固定 kubectl v1.35.8 内置 Kustomize v5.7.1。

P3b 的第六份库/ABI/wheel 锁、四张公开图片及其许可/摘要、预期像素契约集中在 `agentbox/batch-images/`。二进制图片和 wheel 进入忽略的本地准备缓存，运行时输入预装进固定镜像，不在任务中随机取图。`batch-api/` 是独立应用入口，复用静态队列但使用自己的 API/PVC 配额，与 P3a 的单 Pod 验收预算分别渲染，不能把两个配额混用于同一 namespace。

Python 镜像的默认下载仓库集中在 `lab/image-sources.yaml`，使用本实验已验证的 `docker.m.daocloud.io/library/python`；实际引用的 digest 取自版本锁。smoke、gVisor/network 验收和 session-build 共用该配置，API 基镜像沿用 session bundle 已校验的来源。版本锁中的官方仓库地址保留为内容基线，不是这些入口的默认下载地址。命令行 `--smoke-image-source` / `--session-python-source` 可显式覆盖，但必须保留相同 digest。镜像源变更进入源码报告，不修改集群版本身份；更改 digest 仍使旧版本身份失效。当前官方 Python 源的一次拉取失败不代表其他仓库不可用。

`lab/format-compatibility.yaml` 仅承接 8958f50 的精确等价配置：解析后的锁值及资源/补丁指纹都相同，才沿用旧集群状态与镜像缓存身份。实际源文件仍逐一记录当前字节哈希；内容、权限或版本改变不会被格式桥接许可。镜像 bundle 的 v1 输入 ID 保留历史名称，不表示继续读取旧 JSON/Dockerfile 路径。

运行报告、缓存元数据、私有 principals 以及历史验收 JSON 保留原格式，不与静态资源混放；原始证据位于忽略的 artifacts 中。凭据和 Secret 内容不进入这里的清单或渲染参数。等价检查和真实集群复核见[迁移验收](../docs/validation.md#manifests)。

批任务资源可通过 `python3 scripts/render_manifests.py --component batch --environment lab` 审阅，计算夹具使用 `--fixture batch`。Kueue 的上游发行清单校验后离线组合，不在 Kustomize 构建中读取远端 URL。安装、固定配额与失败夹具清理见[批任务运维](../docs/operations.md#batch)。

图片产品入口使用 `--component batch-api`，实际实例须注入经过构建/CRI 验证的 API 镜像、工作镜像、配置 ID 与 manifest digest。Job YAML 与像素契约通过本地 ConfigMap 文件生成器组合，Python 仅对解析后的 namespace/归属、镜像和明确参数更新动态字段；不做模板文本替换。API 凭据单独提供，计算容器只挂载本次尝试的产物子目录。使用与清理见[图片产品运维](../docs/operations.md#batch-images)。
