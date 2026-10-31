# 旧运行链路归档

最后保留完整旧实现的主线版本为 `c6c2e48`（Claim 保留阶段合并）。该版本包含 agentboxd、专用 Gateway、旧 execute_python/get_status、直接 runsc 后端、FIFO/Warm/AIMD、systemd 安装和旧压测脚本。当前活动代码不构建或调用这些组件。

需要历史复现时，由管理员在独立隔离目录检出该版本，按该版本 README、docs/runtime.md、docs/operations.md 和 docs/validation.md 操作。不要在新平台工作目录恢复旧入口，也不要把旧请求自动迁入新会话。旧代码版本和历史输入/资源条件必须一并记录。

- [旧源码树](https://github.com/X1Kun/AgentBox/tree/c6c2e48)
- [旧运行时说明](https://github.com/X1Kun/AgentBox/blob/c6c2e48/docs/runtime.md)
- [旧部署与配置](https://github.com/X1Kun/AgentBox/blob/c6c2e48/docs/operations.md)
- [旧验证与实验命令](https://github.com/X1Kun/AgentBox/blob/c6c2e48/docs/validation.md)
- [移除文件校验和](../data/legacy-source-retirement.json)

历史 JSON、图表及原始 artifacts 保持原口径。`.local` 中的历史镜像/缓存、用户模型配置和本地备份没有作为仓库源码清退的一部分删除；当前平台使用自己的受归属管理的缓存。系统旧安装的退役需要独立确认进程、cgroup、挂载和状态记录，操作见[退役工具](../operations.md#retirement)。

本目录其余分析资料仅描述历史实现；其中旧代码/脚本链接固定到上述版本，不承担当前安装或运行入口。
