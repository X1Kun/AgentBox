# 部署资源

这里按组件职责组织静态 YAML、原生配置和版本锁。Kustomize 负责组合和固定环境差异；Python 负责每轮实验的资源选择、namespace/归属、镜像、命令、运行时发现、验证和清理。

```text
deploy/
├── lab/                         # kind 配置、镜像源、工具版本锁、格式迁移身份表
├── components/
│   ├── gvisor/                  # RuntimeClass、runsc.toml、版本/归档锁
│   ├── calico/                  # 上游版本/镜像锁、lab 覆盖补丁
│   └── agent-sandbox/           # Template、WarmPool、控制器补丁、runtime 镜像
├── agentbox/
│   ├── api/                     # Deployment、RBAC、Dockerfile
│   ├── cleanup/                 # 独立管理员 RBAC
│   └── overlays/lab/            # 应用环境的节点约束、拉取策略、配额
└── acceptance/
    ├── smoke/
    ├── gvisor/
    ├── network/
    └── sessions/                # SDK 客户端、网络策略、预热/配额夹具
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

版本锁位于 `lab/versions.lock.yaml` 和三个组件各自的 `versions.lock.yaml`，版本、镜像 digest、下载摘要不因迁移改变。版本/摘要应保留为字符串。YAML 采用安全加载器并拒绝重复键和可执行标签；普通检查要求可用 kubectl/Kustomize 和 PyYAML，当前固定 kubectl v1.35.8 内置 Kustomize v5.7.1。

Python 镜像的默认下载仓库集中在 `lab/image-sources.yaml`，使用本实验已验证的 `docker.m.daocloud.io/library/python`；实际引用的 digest 取自版本锁。smoke、gVisor/network 验收和 session-build 共用该配置，API 基镜像沿用 session bundle 已校验的来源。版本锁中的官方仓库地址保留为内容基线，不是这些入口的默认下载地址。命令行 `--smoke-image-source` / `--session-python-source` 可显式覆盖，但必须保留相同 digest。镜像源变更进入源码报告，不修改集群版本身份；更改 digest 仍使旧版本身份失效。当前官方 Python 源的一次拉取失败不代表其他仓库不可用。

`lab/format-compatibility.yaml` 仅承接 8958f50 的精确等价配置：解析后的锁值及资源/补丁指纹都相同，才沿用旧集群状态与镜像缓存身份。实际源文件仍逐一记录当前字节哈希；内容、权限或版本改变不会被格式桥接许可。镜像 bundle 的 v1 输入 ID 保留历史名称，不表示继续读取旧 JSON/Dockerfile 路径。

运行报告、缓存元数据、私有 principals 以及历史验收 JSON 保留原格式，不与静态资源混放；原始证据位于忽略的 artifacts 中。凭据和 Secret 内容不进入这里的清单或渲染参数。等价检查和真实集群复核见[迁移验收](../docs/validation.md#manifests)。
