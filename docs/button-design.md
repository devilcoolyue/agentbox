# 全站按钮盘点与设计规范

盘点覆盖主控制台 170 个静态按钮、abox-link 面板 7 个静态按钮，以及前端模块动态创建的操作入口。主控制台为其中 122 个静态按钮补充或替换统一线性图标；已有 SVG 的操作继续复用。数量按按钮定义统计，不按列表渲染后的实例数统计。

## 设计方向

参考 Linear 的低噪声工具栏、GitHub 的操作层级，沿用产品琥珀强调色与深浅主题，使用细描边、8px 圆角和统一图文间距。

| 类型 | 规则 | 示例 |
|---|---|---|
| 主操作 | 实心强调色，动作图标 + 文字 | 创建、保存、提交、登录 |
| 普通操作 | 中性描边与浅底，相同动作使用相同图标 | 上传、下载、刷新、安装 |
| 次要操作 | 透明底，悬停时出现浅背景 | 编辑、测试、重置、取消 |
| 危险操作 | 低饱和红色文字与浅底，保留原有确认 | 删除、丢弃 |
| 分段选择 | 独立选中底色、描边与内边距 | 文件范围、技能范围、预览 / 源码 |
| 纯图标操作 | 可访问名称与焦点反馈 | 关闭、目录展开、文件行操作、分页 |

取消、日历数字、页码和模型选项保留简洁文字。悬停反馈为 140ms，按下时轻微下移，键盘焦点明确；减少动画偏好下关闭相关动效。触屏通用操作按钮最小高度 44px。

## 页面与动态操作

| 页面 / 模块 | 按钮范围 | 调整 |
|---|---|---|
| 登录与空状态 | 登录、新建空间 | 主按钮 + 动作图标 |
| 全局导航 | 新建、使用记录、隧道、设置、主题、用户菜单、退出、侧栏开关 | 复用已有 SVG，替换字符省略号 |
| 工作台 | 启动、停止、额度、删除、重命名、功能页签 | 继承统一动作层级与图标 |
| 对话 | 历史、新建、重连、重试、四个任务入口、附件、语音、发送、模型返回、复制、预览 | emoji 入口改线性图标，补动态预览与返回入口 |
| 终端 | 重连 | 复用已有图标 |
| 文件与共享目录 | 范围、新建目录、上传、下载、展开、上一级、预览、重命名、移动、删除 | 统一图标，操作列为五个图标预留空间 |
| 文件预览 | 预览 / 源码、视口切换、刷新、新标签页、全屏 / 还原、保存、下载、关闭 | 静态与动态状态统一图标 |
| Git 变更 | 刷新、全部丢弃、提交、逐文件丢弃、差异 / 完整内容 | 明确提交与危险操作层级，工具栏可换行 |
| 技能 | 范围、刷新、安装、上传 / 市场、复制、下载、删除、返回、预览 / 源码、目录展开 | 动态按钮补图标，手机工具栏分行 |
| 账号池 | 添加、登录 / 授权 / Key、编辑、删除、认证模式、链接、复制、测试、保存 | 主次与风险分层，加载完成保留图标 |
| IP 代理 | 添加、导入、导出、刷新、测试、编辑、删除、桥接保存 | 表格与代理选择弹层均补图标 |
| 模型、资源、界面 | 保存、添加模型、移除 / 默认 | 统一保存、添加、勾选、移除图标 |
| 价目表 | 填入官方价、保存、添加行、删除行 | 操作栏换行，删除行提供可访问名称 |
| 安全与用户 | 添加用户、额度、重置密码、删除、修改密码、保存配置 | 动态行补齐图标 |
| 使用记录 | 筛选、重置、刷新、导出、日期确定、月份导航、排序、分页、费用明细 | 保留现有图标，补日期确定与月份导航 SVG |
| 隧道 | 前往设置、生成配对码、客户端下载 | 设置、链接、下载图标 |
| 通用弹窗 | 确定、取消、关闭、移动、删除、提交、丢弃、充值 | 动作图标与语义色一致 |
| abox-link | 接入、启动 / 停止、添加规则 / 映射、删除规则、解绑、放弃、保存 | 独立轻量图标集，补加载态；轮询更新后仍保留图标 |

## 维护约定

- `web/src/icons.ts` 不依赖业务模块。静态按钮声明 `data-icon`，入口执行 `decorateIcons()`；动态节点或文字变化调用 `buttonLabel(element, label, icon)`。用户文本仍通过文本节点写入。
- `chat-render.ts` 继续导出 `svgIcon`，兼容已有调用方，实际实现统一位于 `icons.ts`。
- `btnBusy` / `btnDone` 保存并恢复原节点，更新 `aria-busy`，避免加载结束后图标丢失。
- SVG 使用 `currentColor`，装饰图标设置 `aria-hidden` 和 `focusable=false`，不依赖外部字体或网络图标资源。
- 颜色使用现有设计令牌；TypeScript 与编译后的 JavaScript 一起更新。
- abox-link 独立嵌入自身用到的少量图标，不依赖主服务静态目录；发布面板变更时需要另行构建客户端。

## 静态按钮逐项清单

“已有图标”包含内联 SVG 和现有模块初始化时添加的图标。“文字控件”主要是取消按钮及信息选择控件。

### 主控制台

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
| `btn-unpair` | 解除绑定 | close |
| `btn-revert` | 放弃 | undo |
| `btn-save` | 保存并应用 | save |
## 验证记录

- `npm run check`、`npm run build`、`go build ./...`、`go test ./...`、`git diff --check` 通过。
- 使用本地示例 API 数据检查桌面深浅主题、390px 窄屏：账号池、设置各操作页、对话、文件、Git、技能、预览弹窗、日期选择器、隧道和 abox-link 面板。
- 检查加载态的禁用 / `aria-busy` / 原图标恢复，以及全屏 / 还原、文件范围文案和技能动态按钮的图标保留。
- 修正技能工具栏、价目表动作栏和文件操作列的窄屏挤压；浏览器检查不是生产 Docker / 隧道链路验证。
- 预览截图保存在本地 `output/playwright/buttons-*.png`（不进入版本库）。
