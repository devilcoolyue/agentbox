# 图标统一管理

界面上的图标按「位置」统一管理：代码只引用位置 ID，图形由管理员在 **系统设置 → 图标管理** 里选择。换一个图标不需要改代码，也不需要重新发布。

## 图标库

| 库 | 版本 | 数量 | 风格 | 许可 |
|---|---|---|---|---|
| Tabler（主） | 3.49.0 | 5184 线性 + 1054 填充 | 24px 网格、圆角描边 | MIT |
| Lucide（补充） | 1.61.0 | 1903 | 24px 网格、圆角描边 | ISC |

两套规格一致，混用不违和。默认图形大多取自 Tabler。图标库文件在 `internal/web/static/vendor/icons/`，随二进制分发，只在管理员打开图标选择器时加载；页面平时只带用到的那些图形（`web/src/icon-builtin.ts`，约 20 KB）。

## 位置与语义位

`web/src/icon-slots.ts` 登记了所有位置，分两类：

- **语义位**（ID 不带点，如 `refresh`、`trash`、`copy`）：一类动作或对象，被许多按钮共用。换一次，所有引用它的按钮一起变。
- **具体位置**（ID 带点，如 `nav.usage`、`chat.thinking`、`tab.terminal`）：
  - 有独立默认图标（`icon`）的，例如侧边栏菜单、工作空间标签、对话里的工具调用和思考过程；
  - 默认跟随某个语义位（`base`）的，例如「复制消息」跟随「复制」。管理员可以单独给它换图标，不影响其他按钮。

图标的确定顺序：该位置的管理员设置 → 该位置的默认图标 → 沿 `base` 找到的语义位（同样先看管理员设置）。

## 管理页面

- **按位置**：按分组列出每个位置当前的图标、默认值、是否跟随语义位，可以「更换」或「恢复默认」。
- **按图标**：列出每个图标用在哪些位置。「整体更换」把这些位置一起换成另一个图标；「设置到哪些位置…」勾选这个图标要用在哪些位置。

修改先在当前页面实时预览，点「保存」才写入服务端并对全站生效；离开页面时未保存的预览会撤掉。配置存在 `<data_dir>/icons.json`，进入系统备份。

## 开发约定

1. 新增用图标的地方：先在 `icon-slots.ts` 登记位置（写 `icon: "tabler:名字"` 或 `base: "语义位"`），label 用中文并在 `web/src/locales/*-dynamic.ts` 补英文与繁体。
2. 代码里写位置 ID：`data-icon="nav.usage"`、`svgIcon("chat.thinking")`、`buttonLabel(el, label, "refresh")`。TypeScript 会检查 ID 是否已登记。
3. `npm run build` 会运行 `scripts/gen-icons.mjs`，把新的默认图形写进 `icon-builtin.ts`。
4. `npm run test:icons` 检查：HTML/模板里的 `data-icon` 都已登记、`base` 没有环、默认图形存在、翻译齐全、图标库只含渲染器允许的元素。
5. 升级图标库：改 `scripts/vendor-icons.py` 里的版本与 integrity，运行它会重写 `vendor/icons/*.json`、许可证与 `third_party/vendor.json`；`--check` 可复核文件与上游一致。

## 安全

- 浏览器只用白名单元素（path/circle/rect/line/ellipse/polyline/polygon）和属性画图标，不使用 innerHTML。
- `PUT /api/icons`（仅管理员）只接受「位置 ID → 库:名字」，并核对图标在随二进制分发的库里存在；不接受任意 SVG。
- `GET /api/icons` 不需要登录（登录页也要用），只返回位置与图标名和对应形状，没有敏感信息。
