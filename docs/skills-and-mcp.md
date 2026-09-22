# 技能、插件与 MCP

[返回文档目录](README.md) · [项目首页](../README.md)

## home 模板

agentbox 提供 Claude 技能文件的管理界面；完整插件与 MCP 仍由容器内原版 CLI 管理，可按其方式安装（`claude mcp add -s user …`、
`~/.claude/skills/<名字>/SKILL.md`、`/plugin` 等）。但**新建工作空间的 home 从独立空目录开始**，
装在工作空间里的东西只属于那个工作空间。要预置给多个工作空间，用 home 模板 —— 它在每次工作空间启动时
叠加到 `/home/agent`，分两层，后者盖前者：

| 模板 | 位置 | 影响范围 | 谁维护 |
|---|---|---|---|
| 服务器模板 | `data/home-template/` | **所有用户的所有工作空间** | 管理员，宿主机上改 |
| 用户模板 | `data/users/<user>/home-template/` | 该用户的所有工作空间 | 用户自己，网页「技能」页签或宿主机 |

```text
data/home-template/
  .claude/
    skills/my-skill/SKILL.md     # 所有 claude 工作空间都带这个技能
    settings.json                # 例如 enableAllProjectMcpServers
  .codex/AGENTS.md
  .bashrc
```

规则：

- **逐文件按 mtime「谁新用谁」**：容器里改过的文件保留；模板里更新过的文件推送到已存在的工作空间。
  反过来说，在工作空间里删掉模板文件不会持久——下次启动又回来。
- **符号链接原样重建、不跟随**，所以大块内容可以指向 `/shared` 而不必每个工作空间复制一份。
- 可执行位保留（hook 脚本能直接跑）；`.claude/` 只对 claude 工作空间有意义、`.codex/` 只对 codex
  有意义，放在同一份模板里互不干扰。
- 模板在凭证播种**之前**执行，所以模板里误放的凭证文件压不过账号池；模板出错只记日志，
  不会挡住工作空间启动。
- 两层模板在写盘**之前**先合并（用户层覆盖服务器层），所以用户模板里较旧的同名文件
  照样能盖住服务器模板 —— mtime 比较只发生在合并结果与工作空间副本之间。

## 「技能」页签

工作台的**技能**页签（仅 claude 工作空间）把上面这套东西做成了界面：左侧是当前工作空间
`~/.claude/skills` 的**文件树** —— 技能行展开就是这个技能目录的全部内容
（`SKILL.md`、`scripts/`、`references/`、`assets/` 以及任意层级的子目录，脚本带可执行位的
标 `+x`），并标出每个技能是**空间自装**、来自**我的模板**还是**服务器模板**。

右侧看内容：`.md` 默认按 Markdown 渲染（复用对话那套渲染器），右上角可切「预览 / 源码」，
预览时 front matter 里的键单独列成小标签，不会被吞掉；脚本和其它文本原样显示；
`assets/` 里的图片内联预览；二进制文件只报大小并给「下载」。单个文件的文本预览上限
256KB，超出截断。

- 范围切到「我的模板」即直接管理 `data/users/<user>/home-template/.claude/skills`，
  用户不用碰宿主机就能把技能铺给自己的所有工作空间；
- 「安装技能」弹窗有两个来源：

  - **本地上传**：`.md`（单文件技能，存成 `<名字>/SKILL.md`）或 `.zip`/`.tar.gz`
    （技能目录打包，允许外面套一层同名目录），支持拖拽；
  - **官方市场**：浏览 `anthropics/claude-plugins-official`（可搜索、按分类筛选，条目数随上游更新），
    安装时服务端拉取该条目的源码并把其中的技能装进当前范围。注意**市场的单位是插件**，
    可能只含斜杠命令或 MCP 服务器——这类条目没有技能可装，会提示改用终端
    `claude plugin install <名字>@claude-plugins-official` 装整包；
- 目录仓库浅克隆缓存在 `data/marketplace/repo`，12 小时过期，可在弹窗里点「刷新目录」强制更新；
  拉不动时沿用旧副本，浏览不会整个瘫掉；
- 「复制到我的模板」把工作空间里调好的技能推给自己的所有工作空间，「装到本空间」反向把模板技能
  立刻装进正在跑的工作空间（模板本身要下次启动才铺，这个按钮省掉一次重启）。

服务器模板不在界面里开放：它对全体用户可见，仍由管理员在宿主机上维护。

## MCP 配置

- **Claude**：用户级 MCP 写在 `~/.claude.json`，服务端只在缺失时生成该文件，不会覆盖，
  工作空间内 `claude mcp add -s user` 即可长期生效。项目级 `/workspace/.mcp.json` 在 headless
  回合里默认不加载，需要在 `~/.claude/settings.json` 里加 `"enableAllProjectMcpServers": true`。
- **Codex**：账号池目录存在 `config.toml` 时，每次启动会覆盖空间内的 `~/.codex/config.toml`，所以 `[mcp_servers.*]` 要写在
  该账号实际的 `<credentials_dir>/config.toml` 里（例如 `accounts/<id>/` 或 `data/creds/<id>/`），写在工作空间内或 home 模板里可能被覆盖。控制台改中转站地址
  是行级替换，不会破坏该文件里的其它段落。
- 对话模式每回合都新起一次 CLI 进程，stdio 型 MCP server 每回合都会重新拉起；依赖
  `npx -y` 现拉包的 server 会让每条消息都多等几秒，建议预装到 home 里。

## 准备一个可复用技能

最小结构是 `<技能名>/SKILL.md`，其中写明适用场景与操作步骤；有辅助文件时放在同一个技能目录内：

```text
my-skill/
  SKILL.md
  scripts/
  references/
  assets/
```

单独上传 Markdown 会保存成该技能的 `SKILL.md`；包含资源时把整个目录打包上传。先装到当前空间验证，再复制到「我的模板」，可避免未验证的脚本影响其他空间。

这些示例使用默认 `data_dir=data`，自定义数据目录时对应替换。服务器模板、用户模板和空间 home 都包含需要保留的配置，完整备份范围见[部署手册](../deploy/README.md#备份与恢复)。
