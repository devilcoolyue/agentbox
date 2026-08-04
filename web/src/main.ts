/* main：入口。各模块在被 import 时自行完成 DOM 事件挂载（与旧单文件时代
 * 的顶层执行语义一致），这里只负责聚合加载与启动。 */
"use strict";

import { S } from "./state.js";
import "./util.js";
import "./theme.js";
import "./shell.js";
import "./data.js";
import { showLogin, tryEnter } from "./login.js";
import "./sessions.js";
import "./chat.js";
import "./voice.js";
import "./term.js";
import "./files.js";
import "./changes.js";
import "./preview.js";
import "./settings.js";
import "./proxies.js";
import "./tunnel.js";
import "./ping.js";

/* 品牌图形：吉祥物「盒仔」与 logo 主形各留一份模板，盖章到各挂载点
 * （登录页 / 侧栏字标 / 工作台空状态），避免大段 SVG 在 HTML 里重复。
 * 吉祥物填入容器（.mascot-* 控制尺寸），logo 主形直接替换占位。 */
const boxi = (document.getElementById("tpl-mascot") as HTMLTemplateElement).content;
for (const m of document.querySelectorAll("[data-mascot]")) m.append(boxi.cloneNode(true));
const mark = (document.getElementById("tpl-mark") as HTMLTemplateElement).content;
for (const m of document.querySelectorAll("[data-mark]")) m.replaceWith((mark.cloneNode(true) as DocumentFragment).firstElementChild!);
const themeIco = (document.getElementById("tpl-theme-ico") as HTMLTemplateElement).content;
for (const m of document.querySelectorAll("[data-theme-ico]")) m.append(themeIco.cloneNode(true));

if (S.token) tryEnter();
else showLogin();
