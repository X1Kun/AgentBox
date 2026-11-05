# 部署资源

静态资源按组件组织，Kustomize 组合实际环境；Python 编排实验，并验证、补齐必须显式提供的部署输入。

```text
deploy/
├── lab/                         # kind、工具锁、镜像源和兼容身份表
├── components/                  # gVisor、Calico、Agent Sandbox、Kueue、观测组件
└── agentbox/
    ├── api/                     # 会话 API、Namespace、RBAC、共享网络与 Service
    ├── cleanup/                 # 独立管理员权限，产品入口不包含它
    ├── batch/                   # 队列、flavor、Namespace、计算网络边界
    ├── batch-api/               # 图片 API、PVC、权限、产品预算与 Service
    ├── batch-images/            # 固定数据集、工作镜像、Job 模板及像素契约
    └── overlays/lab/
        ├── session/             # 独立会话产品组合和输入样例
        └── batch/               # 独立图片批任务产品组合和输入样例

tests/fixtures/platform/         # smoke、gVisor、网络、SDK、批 Job 验收夹具
experiments/configs/session/     # 行为、用户矩阵、R/B、诊断实验参数
```

lab 根目录没有总 Kustomization。`session/` 不创建批任务队列或管理员清理权限；`batch/` 只使用 `batch-product-budget`，不包含单 Job 验收的 `batch-budget`。共享 API 网络策略位于 `agentbox/api/network/`，固定预热补丁位于 `components/agent-sandbox/warm-pool-patch.yaml`。部署资源不能反向引用测试夹具。

## 两层部署关系

先用 `make platform-up` 准备或复用本项目的 kind、Calico、gVisor/RuntimeClass、Agent Sandbox CRD/控制器；批任务另用 `make platform-batch-install` 准备 Kueue。这些是共享平台前置，应用清单不能替代 containerd handler 的宿主安装。

再分别生成并应用会话或图片产品清单。静态 Kustomization 是组合源，完整清单还必须包含实际镜像、API Server 出口和私有凭据。预览只用于审阅：

```bash
make k8s-render
kubectl kustomize deploy/agentbox/overlays/lab/session
kubectl kustomize deploy/agentbox/overlays/lab/batch
python3 scripts/render_manifests.py --fixture sessions
```

默认会话入口使用冷池。图片产品为一个 API 加一个 500m/256Mi 计算名额，预算为 2 Pod、600m/384Mi requests；单 Job 的 1 Pod、500m/256Mi 配额只属于 `tests/fixtures/platform/batch/resources/`。

## 显式镜像与部署输入

已有平台和 worker 构建缓存后，运行：

```bash
make platform-batch-image-build
python3 scripts/platform_lab.py product-images --with-batch --output .local/products/prepared-1
```

该入口独立构建产品 API，并核对既有 runtime/worker 的 OCI 描述符及 config ID，在原节点缓存中补齐 digest 引用。它不更新组件版本或覆盖既有镜像别名。输出目录的 `session-inputs.yaml`、`batch-inputs.yaml` 是非秘密部署输入，明确列出归属、实例、namespace、镜像及 API Server 的 Service/EndpointSlice 地址；可先审阅再使用。也可从各入口的 `inputs.example.yaml` 手动填写，REQUIRED 占位值不能通过最终渲染。

准备私有目录中的 `tls.crt`、`tls.key`、`principals.json`，后两份文件权限必须为 0600。证书与私钥须匹配、证书未过期，SAN 应覆盖实际访问地址；本地 port-forward 使用 127.0.0.1 时也需要对应 SAN。principals 沿用产品 API 的 JSON 契约，不进入静态源码。

```bash
python3 scripts/render_product.py \
  --config .local/products/prepared-1/session-inputs.yaml \
  --credentials-dir .local/products/session-credentials \
  --kubeconfig .local/platform/kubeconfig --context kind-agentbox-platform-lab \
  --output .local/products/session.yaml
kubectl --kubeconfig .local/platform/kubeconfig --context kind-agentbox-platform-lab \
  apply -f .local/products/session.yaml
```

批任务使用 `batch-inputs.yaml`、相应凭据目录和新的输出文件。渲染器校验实际控制器、CRD、RuntimeClass、Calico、namespace 归属和声明的 API 出口；镜像与 digest 必须非占位且相互匹配。未指定 kubeconfig/context 时只离线渲染，不声称已验证集群。完整输出含 Secret，只写入新建的 0600 文件，不打印到 stdout。

应用后资源长期保留；实验验收的自动清理不会管理这些实例。lab 默认拒绝 API 入站，可通过 `service/agentbox-api` 或 `service/agentbox-batch-api` 做管理员 port-forward；开放其他调用来源需另行声明网络入口。批任务仍受有限记录容量约束，长期运行及归档边界见使用契约。删除产品 namespace 会删除其会话或 PVC 数据，应作为独立的卸载操作处理。

## 验收与身份

`make platform-product-entry-test` 在临时 namespace 直接使用同一完整清单渲染器，验证会话执行/文件和图片产物，随后按原 UID 清理。其他验收脚本继续使用小资源组，不再从混合 lab 总集合挑选对象。

七份版本锁保持原值。`lab/format-compatibility.yaml` 继续支持精确的旧锁、缓存和安装身份映射；v1 bundle 中旧路径字符串是协议键，不是物理文件读取路径。平台 session 指纹只包含上游锁和控制器运行补丁；SDK Job/RBAC/客户端 Dockerfile 单独计算验收指纹，应用模板按产品组合记录。SDK 或实验配置修改不会要求重建健康平台；平台运行配置变更仍使旧身份校验失败。

历史运行报告、缓存元数据和原始验收 JSON 不回写。Python 固定依赖见 `scripts/requirements-platform.txt`，产品使用及验证范围见[运维指南](../docs/operations.md)与[验收记录](../docs/validation.md)。

宿主编排库按职责位于 `scripts/lib/{core,manifests,platform,product,acceptance,experiments,observability}/`。`workloads/images/worker.py` 是实际容器入口，共享图片算法由构建器复制为独立运行模块；`tests/fixtures/probes/` 保存 Pod 验收程序，节点进程诊断 Shell 保存在 `scripts/lib/platform/resources/`。平台安装与 SDK/批任务验收分别实现，CLI 按子命令加载，基础预检不加载 Pillow 或验收场景。
