import { S } from "../../state.js";
import { $, toast, fmtUptime, fmtBytes, fmtTime, fmtDateTime } from "../../util.js";
import { api } from "../../api.js";
import { agentIcon, agentName } from "../../brand.js";
import { setTip } from "../../tip.js";
import { Poller } from "../../shared/poller.js";
/* ---------------- 关于 ---------------- */
export async function loadSystem() {
    const kv = $("about-kv");
    kv.replaceChildren();
    kv.classList.add("hidden");
    $("about-loading").classList.remove("hidden");
    let sys;
    try {
        sys = await api("/system");
    }
    catch (e) {
        toast("读取系统信息失败：" + e.message, true);
        return;
    }
    finally {
        $("about-loading").classList.add("hidden"); // 失败时也别留着转圈
    }
    kv.classList.remove("hidden");
    const add = (k, v) => {
        const dt = document.createElement("dt");
        dt.textContent = k;
        const dd = document.createElement("dd");
        dd.textContent = v;
        kv.append(dt, dd);
    };
    add("版本", sys.version || "dev");
    add("数据库版本", String(sys.schema_version ?? "unknown"));
    add("运行时", "agentbox · " + sys.go_version);
    add("Docker", sys.docker_version ? sys.docker_version : "无法连接");
    add("监听地址", sys.listen);
    add("工作空间", `${sys.sessions_running} 个运行中 / 共 ${sys.sessions_total} 个`);
    add("账号池", sys.accounts + " 个账号");
    add("用户", sys.users + " 个（含管理员）");
    add("数据目录", sys.data_dir);
    add("配置文件", sys.config_path);
    add("运行时长", fmtUptime(Date.now() - sys.started_at) + "（自 " + fmtDateTime(sys.started_at) + "）");
}
/* ---------------- 运维监控 ---------------- */
const monitor = new Poller();
export function startMonitor() {
    stopMonitor();
    $("mon-tiles").replaceChildren();
    $("mon-tbody").replaceChildren();
    $("mon-loading").classList.remove("hidden");
    let first = true;
    monitor.start(5000, async (signal) => {
        if (S.view !== "settings" || S.sec !== "monitor") {
            stopMonitor();
            return;
        }
        if (!first && (!$("mon-auto").checked || document.hidden))
            return;
        await loadMonitor(first, signal);
        first = false;
    });
}
export function stopMonitor() { monitor.stop(); }
async function loadMonitor(surfaceErr, signal) {
    let m;
    try {
        m = await api("/monitor", { signal });
    }
    catch (e) {
        if (!signal.aborted && surfaceErr && S.sec === "monitor")
            toast("读取监控失败：" + e.message, true);
        return;
    }
    if (signal.aborted || S.view !== "settings" || S.sec !== "monitor")
        return; // 请求在途中切走了页，丢弃这帧
    $("mon-loading").classList.add("hidden");
    renderMonitorTiles(m);
    renderMonitorTable(m);
}
function monTile(label, value, sub, pct) {
    const el = document.createElement("div");
    el.className = "mon-tile";
    el.append(Object.assign(document.createElement("div"), { className: "mt-label", textContent: label }), Object.assign(document.createElement("div"), { className: "mt-value", textContent: value }));
    if (typeof pct === "number") {
        const bar = document.createElement("div");
        bar.className = "mt-bar";
        const fill = document.createElement("span");
        const p = Math.max(0, Math.min(100, pct));
        fill.style.width = p + "%";
        if (p >= 90)
            fill.classList.add("hot");
        else if (p >= 70)
            fill.classList.add("warn");
        bar.appendChild(fill);
        el.appendChild(bar);
    }
    if (sub)
        el.appendChild(Object.assign(document.createElement("div"), { className: "mt-sub", textContent: sub }));
    return el;
}
function renderMonitorTiles(m) {
    const box = $("mon-tiles");
    const p = m.process, h = m.host, su = m.summary;
    const memPct = h.mem_total ? (h.mem_used / h.mem_total) * 100 : 0;
    const diskPct = h.disk_total ? (h.disk_used / h.disk_total) * 100 : 0;
    // 首帧还没有上一帧可做差，CPU 速率标「测量中」而不是误导的 0.0%。
    const cpu = (v) => (m.window_ms ? v.toFixed(1) + "%" : "测量中");
    const cpuPct = (v) => (m.window_ms ? v : undefined);
    const tiles = [
        monTile("后端 CPU", cpu(p.cpu_percent), "已运行 " + fmtUptime(p.uptime_ms), cpuPct(p.cpu_percent)),
        monTile("后端内存", fmtBytes(p.rss), "Go 堆 " + fmtBytes(p.heap_alloc) + " · " + p.goroutines + " 协程"),
        monTile("主机 CPU", cpu(h.cpu_percent), h.cpu_count + " 核 · 负载 " + h.load1.toFixed(2), cpuPct(h.cpu_percent)),
        // 主数字只放已用量，总量进副标题：「17 GB / 30 GB」在 1440 宽度下会折成两行
        monTile("主机内存", fmtBytes(h.mem_used), `共 ${fmtBytes(h.mem_total)} · ${memPct.toFixed(0)}% 已用`, memPct),
    ];
    // 数据盘写满会连带拖垮 SQLite 与所有会话，水位单独给一格（读不到则不显示）。
    if (h.disk_total) {
        const hint = diskPct >= 90 ? "数据盘将满，尽快清理" : "数据目录所在磁盘";
        tiles.push(monTile("磁盘水位", fmtBytes(h.disk_used), `共 ${fmtBytes(h.disk_total)} · ${diskPct.toFixed(0)}% 已用 · ${hint}`, diskPct));
    }
    tiles.push(monTile("运行容器", su.running + " / " + su.total, "个工作空间容器在运行"), monTile("容器合计", cpu(su.cpu_percent), "内存 " + fmtBytes(su.mem_usage)));
    box.replaceChildren(...tiles);
}
function renderMonitorTable(m) {
    const win = m.window_ms
        ? "采样窗口 " + (m.window_ms / 1000).toFixed(1) + " 秒"
        : "首次采样中";
    $("mon-sub").textContent =
        `共 ${m.summary.total} 个工作空间 · ${m.summary.running} 个运行中 · ${win}`;
    const tb = $("mon-tbody");
    tb.replaceChildren();
    if (!m.containers.length) {
        const tr = document.createElement("tr");
        const td = document.createElement("td");
        td.colSpan = 9;
        td.className = "mon-empty";
        td.textContent = "暂无工作空间容器";
        tr.appendChild(td);
        tb.appendChild(tr);
        return;
    }
    for (const c of m.containers)
        tb.appendChild(monRow(c, m.now, !!m.window_ms));
}
function monCell(text, cls) {
    const el = document.createElement("td");
    el.textContent = text;
    if (cls)
        el.className = cls;
    return el;
}
function monRow(c, now, rate) {
    const tr = document.createElement("tr");
    if (!c.running)
        tr.className = "off";
    const name = document.createElement("td");
    name.className = "mon-name";
    name.append(agentIcon(c.agent, 13), document.createTextNode(c.name || c.session_id));
    const status = document.createElement("td");
    status.className = "mon-status";
    const dot = document.createElement("span");
    dot.className = "mon-dot" + (c.running ? " on" : "");
    status.append(dot, document.createTextNode(c.running ? "运行中" : "已停止"));
    const mem = monCell(c.running ? fmtBytes(c.mem_usage) : "—", "num");
    if (c.running && c.mem_limit)
        setTip(mem, fmtBytes(c.mem_usage) + " / " + fmtBytes(c.mem_limit) + " 上限");
    tr.append(name, monCell(c.user), monCell(agentName(c.agent)), status, monCell(c.running && c.started_at ? fmtTime(c.started_at) : "—"), monCell(c.running && c.started_at ? fmtUptime(now - c.started_at) : "—"), monCell(c.running ? (rate ? c.cpu_percent.toFixed(1) + "%" : "…") : "—", "num"), mem, monCell(c.running && c.pids ? String(c.pids) : "—", "num"));
    return tr;
}
