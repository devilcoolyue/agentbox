# 全站按钮盘点与设计规范

当前主页面有 207 个静态按钮、abox-link 面板有 7 个；TypeScript 模板另有 53 个按钮定义，以及列表中按数据创建的动态操作。数量按定义统计，不按列表实例统计。下方原始逐项清单用于索引；最新图标映射以源码声明及本节语义约定为准。

## 设计方向

主控制台按上下文呈现操作按钮，桌面和手机遵循同一规则：密集工具栏里只有刷新、下载、新建目录这类常见的普通操作用纯图标；主操作（提交、安装、保存、添加、上传）和危险操作（丢弃、删除）即使在工具栏里也保留文字，否则「提交」只剩一个琥珀色圆点。独立入口、表单提交和说明性操作使用图标加文字。abox-link 是独立面板，不使用主控制台样式。

一组操作超过三个时，次要与低频动作收进「⋯」菜单（`web/src/menu.ts`），删除类动作放在菜单最后一项，不和启停、编辑这类常用动作并排。列表行（账号、Git 连接、文件）每行最多露出两三个带字的常用动作。

| 类型 | 规则 | 示例 |
|---|---|---|
| 主操作 | 强调底色与文字，工具组内也保留文字 | 创建、保存、提交、上传、安装、登录 |
| 普通操作 | 独立操作保留图文；工具组内使用中性图标与悬停背景 | 下载、刷新、新建目录 |
| 次要操作 | 透明底，悬停时出现浅背景；行内常用的带文字 | 编辑、测试、重置、取消 |
| 危险操作 | 红色语义并保留文字；行内超过三个动作时放进「⋯」最后一项 | 删除、丢弃、清空后上传 |
| 更多菜单 | 「⋯」图标按钮，菜单项一律图标加文字 | 工作空间、账号行、Git 连接行、文件行 |
| 分段选择 | 独立选中底色、描边与内边距 | 文件范围、技能范围、预览 / 源码 |
| 纯图标操作 | 可访问名称与焦点反馈 | 关闭、目录展开、文件行操作、分页 |

密集工具组内的桌面图标按钮为 32px 方形，图标为 16px，清除文字占位和 gap，保持水平及垂直居中。手机与触屏点击范围为 44px，背景内缩到 32px；长按显示说明。悬停反馈为 140ms，键盘焦点明确；减少动画偏好下关闭相关动效。日历数字、页码和模型选项保留简洁文字。

## 页面与动态操作

| 页面 / 模块 | 按钮范围 | 调整 |
|---|---|---|
| 登录与空状态 | 登录、新建空间 | 主按钮 + 动作图标 |
| 全局导航 | 新建、使用记录、隧道、设置、主题、用户菜单、退出、侧栏开关 | 复用已有 SVG，替换字符省略号 |
| 工作台 | 运行状态、启动 / 停止（同一位置切换）、⋯（额度、重命名、删除）、功能页签 | 桌面头部与窄屏顶栏共用同一份菜单 |
| 对话 | 历史、新建、重连、重试、四个任务入口、附件、语音、发送、模型返回、复制、预览 | emoji 入口改线性图标，补动态预览与返回入口 |
| 终端 | 重连 | 复用已有图标 |
| 文件与共享目录 | 范围、新建目录、上传、下载、⋯（清空后上传）、展开、上一级、预览、行内 ⋯（重命名、移动、删除） | 行内露出预览与下载，窄屏只留预览与 ⋯ |
| 文件预览 | 预览 / 源码、视口切换、刷新、新标签页、全屏 / 还原、保存、下载、关闭 | 静态与动态状态统一图标 |
| Git 变更 | 分支、远程、刷新、⋯（克隆、Git 身份、丢弃全部）、提交、逐文件丢弃、差异 / 完整内容 | 提交带字在最右，丢弃全部收进 ⋯ |
| 技能 | 范围、刷新、安装、上传 / 市场、复制、下载、删除、返回、预览 / 源码、目录展开 | 动态按钮补图标，手机工具栏分行 |
| 账号池 | 添加、认证、编辑、⋯（使用范围、模型能力、删除）、认证模式、链接、复制、测试、保存 | 行内两个带字动作，其余进 ⋯ |
| IP 代理 | 添加、导入、导出、刷新、测试、编辑、删除、桥接保存 | 表格与代理选择弹层均补图标 |
| 模型、资源、界面 | 底部统一保存条（放弃修改 / 保存）、添加模型、移除 / 默认 | 不再每张卡片各放一个保存 |
| 价目表 | 填入官方价、保存、添加行、删除行 | 操作栏换行，删除行提供可访问名称 |
| 安全与用户 | 添加用户、额度、重置密码、删除、修改密码、保存配置 | 动态行补齐图标 |
| 使用记录 | 筛选、重置、刷新、导出、日期确定、月份导航、排序、分页、费用明细 | 保留现有图标，补日期确定与月份导航 SVG |
| 隧道 | 前往设置、生成配对码、客户端下载 | 设置、链接、下载图标 |
| 通用弹窗 | 确定、取消、关闭、移动、删除、提交、丢弃、充值 | 动作图标与语义色一致 |
| abox-link | 接入、启动 / 停止、添加规则 / 映射、删除规则、解绑、放弃、保存 | 独立轻量图标集，补加载态；轮询更新后仍保留图标 |

## 维护约定

- `web/src/icons.ts` 不依赖业务模块。静态按钮声明 `data-icon`，入口执行 `decorateIcons()`；动态节点或文字变化调用 `buttonLabel(element, label, icon)`。用户文本仍通过文本节点写入。
- `.btn` 操作通过上述入口加 `action-control`，缺少文字的常规图标按钮另加 `action-icon`。密集组容器显式标记 `action-tools`，`css/actions.css` 仅压缩它的直接子按钮，并跳过 `.btn-primary`、`.btn-danger` 和 `.keep-label`；不要按屏幕宽度或瞬时按钮数量推断。
- 「⋯」菜单统一用 `menu.ts` 的 `moreButton(items)` / `bindMenu(button, items)`：菜单挂在 body（或所在的模态框）上 fixed 定位，列表的 overflow 裁不到；items 每次打开时重新取，禁用和隐藏状态随当前数据。删除类项用 `danger: true, sep: true` 放最后。
- 图标一种意思只对应一个：检查类用 `refresh`，导出 / 下载文件用 `download`，账号认证用 `key`，网页 / OAuth 授权用 `login`，Git 远程用 `cloud`，IP 代理用 `network`，安全设置用 `shield`。文件类型用 `fileIconName()`，不要再用 ◨ ▣ ⌘ 这类字符。按钮移入更多菜单后恢复文字，避免一串无说明图标。先设置 className，再调用标签函数，避免覆盖统一类名。空标签必须隐藏，否则 flex gap 会把图标挤偏。
- `chat-render.ts` 继续导出 `svgIcon`，兼容已有调用方，实际实现统一位于 `icons.ts`。
- `btnBusy` / `btnDone` 保存并恢复原节点，更新 `aria-busy`，避免加载结束后图标丢失。
- SVG 使用 `currentColor`，装饰图标设置 `aria-hidden` 和 `focusable=false`，不依赖外部字体或网络图标资源。
- 颜色使用现有设计令牌；TypeScript 与编译后的 JavaScript 一起更新。
- abox-link 独立嵌入自身用到的少量图标，不依赖主服务静态目录；发布面板变更时需要另行构建客户端。

## 静态按钮逐项清单

“已有图标”包含内联 SVG 和现有模块初始化时添加的图标。“文字控件”主要是取消按钮及信息选择控件。

### 主控制台（初版索引，后续新增入口见源码）

| 标识 | 操作 | 图标 |
|---|---|---|
| `login-btn` | 登录 | login |
| `btn-menu` | 打开菜单 | 已有图标 |
| `btn-kebab` | 工作空间操作 | more |
| `kb-start` | 启动工作空间 | 已有图标 |
| `kb-stop` | 停止工作空间 | 已有图标 |
| `kb-usage` | 账号额度 | 已有图标 |
| `kb-rename` | 重命名工作空间 | 已有图标 |
| `kb-delete` | 删除工作空间 | 已有图标 |
| `btn-home` | AGENTBOX | 已有图标 |
| `btn-sidebar-toggle` | 收起侧栏 | 已有图标 |
| `btn-sidebar-close` | 关闭菜单 | 已有图标 |
| `btn-new` | 新建工作空间 | 已有图标 |
| `btn-usagelog` | 使用记录余额— | 已有图标 |
| `btn-tunnel` | 内网隧道离线 | 已有图标 |
| `btn-settings` | 系统设置 | 已有图标 |
| `data-theme-option=system` | 切换到跟随系统 | 已有图标 |
| `data-theme-option=light` | 切换到浅色 | 已有图标 |
| `data-theme-option=dark` | 切换到深色 | 已有图标 |
| `data-theme-cycle=` | 切换主题 | 已有图标 |
| `btn-user-menu` | 用户菜单 | 已有图标 |
| `btn-logout` | 退出登录 | 已有图标 |
| `empty-new` | 新建工作空间 | plus |
| `btn-start` | 启动 | 已有图标 |
| `btn-stop` | 停止 | 已有图标 |
| `btn-usage` | 额度 | 已有图标 |
| `btn-delete` | 删除 | 已有图标 |
| `data-tab=chat` | 对话 | 已有图标 |
| `data-tab=term` | 终端 | 已有图标 |
| `data-tab=files` | 文件 | 已有图标 |
| `data-tab=changes` | 变更 | 已有图标 |
| `tab-btn-skills` | 技能 | 已有图标 |
| `btn-threads` | 新对话 | 已有图标 |
| `btn-thread-new` | 新建对话 | plus |
| `chat-reconnect` | 立即重连 | refresh |
| `chat-loading-retry` | 重试 | refresh |
| `hero-pill` | 梳理仓库结构 | network |
| `hero-pill` | 实现一个新功能 | sparkles |
| `hero-pill` | 审查当前代码 | list-check |
| `hero-pill` | 修复一个问题 | bug |
| `chat-scroll-bottom` | 滚动到最新消息 | 已有图标 |
| `btn-attach` | 添加附件 | 已有图标 |
| `btn-voice` | 语音输入 | 已有图标 |
| `btn-pick` |  | 文字控件 |
| `chat-send` | 发送 | 已有图标 |
| `term-reconnect` | 重新连接 | 已有图标 |
| `scope-ws` | 空间文件 | folder |
| `scope-shared` | 共享目录 | users |
| `btn-mkdir` | 新建文件夹 | folder-plus |
| `btn-upload` | 上传代码包 | upload |
| `btn-download` | 下载空间文件 (zip) | download |
| `btn-changes-refresh` | 刷新 | refresh |
| `btn-changes-discard-all` | 全部丢弃 | undo |
| `btn-changes-commit` | 提交 | commit |
| `btn-view-diff` | 差异 | diff |
| `btn-view-full` | 完整内容 | code |
| `skill-scope-session` | 本空间 | box |
| `skill-scope-template` | 我的模板 | users |
| `btn-skills-refresh` | 刷新 | refresh |
| `btn-skill-install` | 安装技能 | plus |
| `data-sec=accounts` | 账号池 0 | users |
| `data-sec=proxies` | IP 代理 0 | network |
| `data-sec=container` | 容器与资源 | box |
| `data-sec=models` | 模型管理 | cpu |
| `data-sec=pricing` | 价目表 0 | wallet |
| `data-sec=interface` | 界面与提示 | sliders |
| `data-sec=security` | 安全与访问 | shield |
| `data-sec=monitor` | 运维监控 | activity |
| `data-sec=about` | 关于 | info |
| `btn-acct-add` | 添加账号 | plus |
| `btn-proxy-reload` | 刷新 | refresh |
| `btn-proxy-import` | 导入 | upload |
| `btn-proxy-export` | 导出 | download |
| `btn-proxy-add` | 添加代理 | plus |
| `btn-save-bridge` | 保存 | save |
| `btn-save-container` | 保存 | save |
| `btn-save-idle` | 保存 | save |
| `btn btn-sm mdl-add` | 添加 | plus |
| `btn btn-sm mdl-add` | 添加 | plus |
| `price-fill` | 填入 Claude 官方价 | download |
| `price-fill-oai` | 填入 OpenAI 官方价 | download |
| `price-save` | 保存价目表 | save |
| `price-add` | 添加一行 | plus |
| `btn-save-timezone` | 保存 | save |
| `btn-save-tips` | 保存 | save |
| `btn-user-add` | 添加用户 | plus |
| `btn-pw-save` | 修改密码 | key |
| `btn-save-tunnel` | 保存 | save |
| `btn-save-security` | 保存 | save |
| `uf-toggle` | 筛选条件 | 文字控件 |
| `uf-date-range` | 当天 | 已有图标 |
| `uf-reset` | 重置 | undo |
| `uf-refresh` | 刷新 | refresh |
| `uf-export` | 导出 CSV | download |
| `usage-sort-asc` | 按时间正序排列 | 文字控件 |
| `usage-sort-desc` | 按时间倒序排列 | 文字控件 |
| `usage-prev` | 上一页 | 已有图标 |
| `usage-next` | 下一页 | 已有图标 |
| `tun-goto-settings` | 前往设置启用 | sliders |
| `tun-pair` | 生成配对码 | link |
| `new-cancel` | 关闭 | close |
| `new-ok` | 创建工作空间 | plus |
| `fv-mode-view` | 预览 | eye |
| `fv-mode-src` | 源码 | code |
| `data-vp=desktop` | 桌面 | desktop |
| `data-vp=tablet` | 平板 | tablet |
| `data-vp=phone` | 手机 | phone |
| `fv-reload` | 刷新 | refresh |
| `fv-newtab` | 新标签页 | external |
| `fv-full` | 全屏 | expand |
| `fv-save` | 保存 | save |
| `fv-download` | 下载 | download |
| `fv-close` | 关闭 | close |
| `file-move-close` | 关闭 | close |
| `file-move-ws` | 空间文件 | folder |
| `file-move-shared` | 共享目录 | users |
| `file-move-cancel` | 取消 | 文字控件 |
| `file-move-ok` | 移动到这里 | move |
| `file-delete-close` | 关闭 | close |
| `file-delete-cancel` | 取消 | 文字控件 |
| `file-delete-ok` | 删除 | trash |
| `git-commit-close` | 关闭 | close |
| `git-commit-cancel` | 取消 | 文字控件 |
| `git-commit-ok` | 提交 | commit |
| `skill-install-close` | 关闭 | close |
| `skill-src-local` | 本地上传 | upload |
| `skill-src-market` | 官方市场 | box |
| `skill-drop` | 拖拽文件到这里，或点击选择 .md = 单文件技能 · .zip / .tar.gz = 技能目录打包（可以外面套一层同名目录） | upload |
| `btn-market-refresh` | 刷新目录 | refresh |
| `git-discard-close` | 关闭 | close |
| `git-discard-cancel` | 取消 | 文字控件 |
| `git-discard-ok` | 丢弃 | undo |
| `file-name-close` | 关闭 | close |
| `file-name-cancel` | 取消 | 文字控件 |
| `file-name-ok` | 确定 | check |
| `ask-close` | 关闭 | close |
| `ask-cancel` | 取消 | 文字控件 |
| `ask-ok` | 确定 | check |
| `ask-input-close` | 关闭 | close |
| `ask-input-cancel` | 取消 | 文字控件 |
| `ask-input-ok` | 确定 | check |
| `q-close` | 关闭 | close |
| `q-grant-btn` | 充值 | wallet |
| `au-close` | 关闭 | close |
| `au-refresh` | 刷新 | refresh |
| `cost-close` | 关闭 | close |
| `lb-close` | 关闭 | close |
| `auth-close` | 关闭 | close |
| `auth-mode-oauth` | 订阅 OAuth | shield |
| `auth-mode-key` | 中转站 API Key | key |
| `auth-gen` | 生成授权链接 | link |
| `auth-copy` | 复制 | copy |
| `auth-finish` | 完成登录 | check |
| `auth-test` | 测试连接 | activity |
| `auth-savekey` | 保存 | save |
| `auth-clearkey` | 清除中转站配置，切回订阅凭证 | undo |
| `del-cancel` | 关闭 | close |
| `del-ok` | 删除 | trash |
| `acct-cancel` | 关闭 | close |
| `acct-ok` | 创建并登录 | plus |
| `acct-edit-cancel` | 关闭 | close |
| `acct-edit-ok` | 保存 | save |
| `acct-del-cancel` | 关闭 | close |
| `acct-del-ok` | 删除 | trash |
| `proxy-cancel` | 关闭 | close |
| `proxy-test` | 测试连接 | activity |
| `proxy-ok` | 保存 | save |
| `proxy-import-cancel` | 关闭 | close |
| `proxy-import-ok` | 导入 | upload |
| `tp-close` | 关闭 | close |
| `tp-new` | 新建对话 | plus |

### abox-link 面板

| 标识 | 操作 | 图标 |
|---|---|---|
| `btn-pair` | 接入 | link |
| `btn-toggle` | 启动 | play |
| `btn-add-allow` | 添加规则 | plus |
| `btn-add-map` | 添加映射 | plus |
| `btn-unpair` | 解除绑定 | unlink |
| `btn-revert` | 放弃 | undo |
| `btn-save` | 保存并应用 | save |
## 验证记录

- `npm run check`、`npm run build`、`go build ./...`、`go test ./...`、`git diff --check` 通过。
- 使用本地示例 API 数据检查桌面深浅主题、390px 窄屏：账号池、设置各操作页、对话、文件、Git、技能、预览弹窗、日期选择器、隧道和 abox-link 面板。
- 检查加载态的禁用 / `aria-busy` / 原图标恢复，以及全屏 / 还原、文件范围文案和技能动态按钮的图标保留。
- 修正技能工具栏、价目表动作栏和文件操作列的窄屏挤压；浏览器检查不是生产 Docker / 隧道链路验证。
- 预览截图保存在本地 `output/playwright/buttons-*.png`（不进入版本库）。

## 2026-09-27 图标收敛

- 全部操作图形统一为 24×24 视框、1.8 描边、圆端点/圆连接、currentColor。工具栏与文件行的图标为 16px，点击区域维持桌面 32px / 触屏 44px。品牌图形和终端键帽字符保留各自语义。
- 主页面的导航、页签、主题、附件、发送/停止、日历、分页使用 `svg[data-icon]` 槽位，从共享图标表填充；保留原状态 class，避免内联路径继续分叉。费用帮助和排序也使用共享 SVG。
- `folder-shared` 表示本人跨空间共享目录，`template` 表示我的技能模板，`store` 表示官方市场，`package-plus` 表示安装；下载文件仍用 `download`。
- Git 克隆/获取/拉取/推送/PR 分别使用 `git-clone` / `git-fetch` / `git-pull` / `git-push` / `git-pr`；连接管理用 `link`，绑定用 `plug`，身份用 `user`，操作记录用 `history`。
- 查看更新用 `info`，使用记录用 `list`，退出登录用 `logout`，解绑用 `unlink`，撤销授权用 `shield-off`。密码开关图标与提示统一表示点击后的动作。
- `ConfirmOpts.icon` 显式表达确认动作；`danger` 只控制颜色，不再自动替换成垃圾桶。删除、撤销授权、放弃并关闭、丢弃并重载均保留各自图标。
- abox-link 仍独立嵌入小图标表，公共动作的路径与主控制台保持一致；客户端包需同步重建后发布。
- 账号行的五个动作固定区分：管理认证 `shield`、编辑账号 `edit`（方框加铅笔）、使用范围 `users`、模型能力 `sliders`、删除账号 `trash`。编辑账号采用用户提供的图形参考，与设置齿轮、模型能力滑杆区分。
