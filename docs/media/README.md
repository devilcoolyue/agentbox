# Product tour / 项目演示

[English README](../../README.md) · [中文首页](../../README_CN.md)

| Language / 语言 | Full tour / 完整版 MP4 | Looping GIF / 循环动图 | Screenshots / 截图 |
| --- | --- | --- | --- |
| English | [agentbox-tour-en.mp4](agentbox-tour-en.mp4?raw=1) — 70 s, 5.3 MB | [tour.gif](../images/en/tour.gif) — 42 s, 5.0 MB | [docs/images/en](../images/en) |
| 简体中文 | [agentbox-tour-zh-CN.mp4](agentbox-tour-zh-CN.mp4?raw=1) — 65 s, 4.7 MB | [tour.gif](../images/zh-CN/tour.gif) — 38 s, 4.8 MB | [docs/images/zh-CN](../images/zh-CN) |

A silent browser recording in seven chapters, with a chapter bar above the unmodified
product UI. The README embeds the GIF of the first four chapters, because GitHub does not
play repository videos inline; the MP4 has all seven at 1920 px.

无声浏览器录屏，共七个章节，章节栏位于未改动的产品界面上方。GitHub README 不能内嵌播放仓库里的
视频，所以首页嵌入前四章的 GIF，1920 像素的完整 MP4 包含全部七章。

| # | English | 中文 |
| --- | --- | --- |
| 1 | Hand off a task — `@` file mention, streamed thinking, tool calls, task list, context meter | 下达任务：`@` 引用文件、流式思考、工具调用、任务列表与上下文占用 |
| 2 | Run CLI commands — the `/` menu and the `/context` grid | 使用 CLI 命令：`/` 菜单与 `/context` 格子图 |
| 3 | Use the original terminal — type check and tests | 使用原生终端：类型检查与测试 |
| 4 | Review every change — per-file Git diff | 逐行审查改动：按文件查看 Git diff |
| 5 | Browse and preview files — rendered Markdown | 浏览与预览文件：Markdown 渲染预览 |
| 6 | Know what every request costs — usage records | 看清每次请求的花费：使用记录 |
| 7 | Make it yours — six interface styles, light and dark | 换上你喜欢的风格：六种界面风格与明暗 |

The interface is rendered by a real browser from the shipped static files. Projects,
conversations, command output, usage and accounts are synthetic: nothing connects to
production, no model is called, and no real user data is shown. The pointer is a demo
overlay that follows the scripted mouse.

界面由真实浏览器渲染发布的静态资源。项目、对话、命令输出、用量与账号均为合成演示数据：
不连接生产、不调用模型，也不展示真实用户数据。鼠标指针是跟随脚本操作的演示叠加层。

Desktop screenshots are **2880 × 1920** PNGs captured at 2× pixel density; the phone
screenshot is **780 × 1688**. The MP4 is **1920 × 1424** (including the 144 px chapter bar),
30 fps, H.264 / yuv420p with faststart; the GIF is 1200 px wide at 10 fps.
[capture.json](capture.json) records the source revision, sizes, durations and chapter times.

The architecture illustrations are editable [English](../images/architecture.svg) and
[Chinese](../images/architecture-cn.svg) SVGs.

To regenerate these assets, follow the [capture guide / 素材生成说明](../development.md#文档截图).
UI captures reflect the recorded source revision; released packages may differ.
