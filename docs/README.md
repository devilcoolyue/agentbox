# 使用文档

[返回项目首页](../README.md)

agentbox 的首页提供功能概览与最短上手路径，本目录解释日常使用和配置细节。命令未特别注明时，在 agentbox 仓库根目录执行；文中的 `box.example.com`、`your-server`、用户名称与账号 ID 均需替换为自己的值。

## 从哪里开始

| 你的目标 | 建议阅读顺序 |
| --- | --- |
| 第一次部署 | [快速开始](../README.md#快速开始) → [账号与模型](accounts-and-models.md) → [部署与运维](../deploy/README.md) |
| 使用已有实例 | [工作空间使用指南](user-guide.md) → [技能与 MCP](skills-and-mcp.md) → [使用记录](usage-and-quotas.md) |
| 管理用户和成本 | [配置参考](configuration.md) → [账号与模型](accounts-and-models.md) → [额度与价目表](usage-and-quotas.md) |
| 访问本地数据库或公司内网 | [出口代理与内网隧道](networking.md) |
| 接入脚本或参与开发 | [API 参考](api.md) → [开发指南](development.md) |
| 遇到错误 | [常见问题](troubleshooting.md) → [部署日志](../deploy/README.md#日志与健康检查) |

## 文档地图

```text
README.md                        功能、快速开始、能力边界
docs/
  README.md                      当前文档导航
  user-guide.md                  工作空间、对话、终端、文件与 Git
  accounts-and-models.md          账号接入、凭证与默认模型
  configuration.md               配置字段、默认值与数据目录
  skills-and-mcp.md               技能、市场、home 模板与 MCP
  usage-and-quotas.md              使用记录、价格、额度与结算
  networking.md                  出口代理、abox-link 与内网访问
  api.md                         HTTP / WebSocket 接口
  development.md                 开发、测试、构建与贡献
  troubleshooting.md             常见问题与定位方法
deploy/README.md                 生产部署、更新、备份与恢复
AGENTS.md                        代码维护约定与实现细节
```

文档描述当前仓库实现。内置模型列表、价格快照和镜像里的 CLI 版本可能随源码或上游变化，部署时以自己的配置与实际账号能力为准。
