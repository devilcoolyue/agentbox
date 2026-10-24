# 使用文档

[返回项目首页](../README.md)

agentbox 的首页提供功能概览与最短上手路径，本目录解释日常使用和配置细节。命令未特别注明时，在 agentbox 仓库根目录执行；文中的 `box.example.com`、`your-server`、用户名称与账号 ID 均需替换为自己的值。

## 从哪里开始

| 你的目标 | 建议阅读顺序 |
| --- | --- |
| 先看界面 | [操作演示](../README_CN.md#操作演示) → [功能亮点](../README_CN.md#功能亮点) → [界面截图](../README_CN.md#界面预览) |
| 第一次部署 | [快速开始](../README_CN.md#快速开始) → [账号与模型](accounts-and-models.md) → [部署与运维](../deploy/README.md) |
| 使用已有实例 | [工作空间使用指南](user-guide.md) → [创建与项目导入](project-creation.md) → [技能与 MCP](skills-and-mcp.md) → [使用记录](usage-and-quotas.md) |
| 换一套界面配色 | [界面主题](themes.md)（导入、导出模板、全站与个人主题） |
| 提前告知用户发版维护 | [系统通告](announcements.md)（全站横幅、倒计时、`release.py activate --announce-minutes`） |
| 恢复未发送的输入 | [聊天草稿与恢复](chat-recovery.md)（v0.1.11 起；草稿、待确认副本和查询恢复） |
| 接入持久聊天接收与结果查询 | [聊天协议 v1](chat-protocol.md)（v0.1.11 起；网页发送与显式核对） |
| 管理用户和成本 | [配置参考](configuration.md) → [账号与模型](accounts-and-models.md) → [额度与价目表](usage-and-quotas.md) |
| 为不同 Agent 打断模型复读 | [独立循环保护网关](loop-guard.md)（开发预览；Responses / Chat / Anthropic / Gemini SSE） |
| 启用或开发扩展 | [扩展与 SPI 协议](architecture/extensions-spi.md)（管理员「扩展」菜单；官方扩展「循环保护」作用于 Claude / Codex 的 API Key 与中转账号，「Webhook 通知」推送空间与回合事件） |
| 区分服务端、镜像和桌面更新 | [三类更新与生效范围](update-components.md) → [兼容矩阵](compatibility.md) → [升级与回退](releases.md) |
| 访问本地数据库或公司内网 | [出口代理与内网隧道](networking.md) |
| 接入脚本或参与开发 | [API 参考](api.md) → [开发指南](development.md) → [统一验证](verification.md) |
| 核对当前支持与待验收范围 | [能力/版本/验证清单](capabilities.md) |
| 参与开源重构 | [重构设计与实施计划](architecture/opensource-refactor.md) |
| 规划后续迭代 | [第二轮计划 M6～M11](roadmap-2.md) · [M6 进展](milestones/m6.md) · [第一轮计划 M1～M5（已封存）](roadmap.md) · [M1 实施与验收](milestones/m1.md) · [M2 进展](milestones/m2.md) · [M3 进展](milestones/m3.md) · [M4 进展](milestones/m4.md) · [候选镜像验证](agent-image-validation.md) · [M5 进展](milestones/m5.md) |
| 让强模型带经济模型干活 | [主管委派](architecture/delegation-supervisor.md)（schema 18：主 Agent 按能力标签把子任务交给跑腿模型，可选 Jev 辅助判断；替换并删除了旧多 Agent 协作）→ [Mindshub Jev 真实服务验证](milestones/decision-mindshub-live.md)（旧协作时期的接入记录） |
| 遇到错误 | [常见问题](troubleshooting.md) → [错误码与操作关联](errors.md) → [分层环境诊断](diagnostics.md) → [部署日志](../deploy/README.md#日志与健康检查) |

当前实现见[本地存储容量管理](storage-capacity.md)。存储扩容与云端计划见 [存储容量专项 S1～S4](milestones/storage.md)：先完成本地容量治理，再推进 S3 备份和空间归档；记录明确区分已实现、人工运维和后续待办。

## 文档地图

```text
README.md                        界面截图、功能、快速开始、能力边界
docs/
  README.md                      当前文档导航
  user-guide.md                  工作空间、对话（任务列表、@ 引用、/ 命令、提问与跟进）、终端、文件与 Git
  accounts-and-models.md          账号接入、凭证与默认模型
  images/                        README 截图与动图：en/、zh-CN/ 各一套（合成数据）；架构图 SVG
  media/                         README 演示视频（MP4）与 capture.json 采集记录
  configuration.md               配置字段、默认值与数据目录
  skills-and-mcp.md               技能、市场、home 模板与 MCP
  usage-and-quotas.md              使用记录、价格、额度与结算
  networking.md                  出口代理、abox-link 与内网访问
  themes.md                      自定义界面主题：文件格式、令牌与取值规则
  api.md                         HTTP / WebSocket 接口
  development.md                 开发、测试、构建与贡献
  roadmap.md                     第一轮里程碑 M1～M5（已封存）
  roadmap-2.md                   第二轮里程碑 M6～M11：收口发布、真实验收、维护成本、多 Agent
  milestones/                    各里程碑实施记录；m1～m5 已封存，m6 起为第二轮
  troubleshooting.md             常见问题与定位方法
deploy/README.md                 生产部署、更新、备份与恢复
AGENTS.md                        代码维护约定与实现细节
```

文档描述当前仓库实现。内置模型列表、价格快照和镜像里的 CLI 版本可能随源码或上游变化，部署时以自己的配置与实际账号能力为准。

- [共享工具与用户缓存](shared-tools.md)：Go SDK、Playwright/Puppeteer 公共只读工具库，用户缓存隔离和数据盘启用步骤。
