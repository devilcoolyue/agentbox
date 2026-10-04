#!/usr/bin/env python3
"""Generate the README architecture diagrams. No third-party dependencies.

Run: python3 scripts/render-architecture.py
The SVGs keep text/vector geometry sharp at any zoom. The optional desktop
client is released separately; model calls originate in the workspace CLI.
"""
from html import escape
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
LABELS = {
    "en": {
        "title": "Your agents. Your infrastructure.",
        "subtitle": "One Go server. Persistent workspaces. Open from anywhere.",
        "access": "01  YOUR DEVICE", "host": "02  YOUR LINUX SERVER", "model": "03  MODEL SERVICES",
        "browser": "Browser", "browser_lines": ["Chat · terminal · files", "Desktop, tablet or phone"],
        "desktop": "Desktop app", "desktop_lines": ["Optional", "macOS · Windows"],
        "host_label": "GO BINARY + LOCAL DOCKER ENGINE", "server": "agentbox server", "control": "Control plane",
        "server_lines": ["Users & access", "Account pool", "Workspace lifecycle", "Usage & quotas"],
        "workspace": "Workspace container", "agent": "Claude Code or Codex", "workspace_lines": ["Streaming chat · CLI", "Shell · tmux · Git"],
        "per_workspace": "ONE CONTAINER PER WORKSPACE", "providers": "Model APIs", "provider_lines": ["Anthropic", "OpenAI / relays"],
        "provider_note": ["The CLI calls the provider.", "Optional account", "egress proxy."],
        "state": "state + history", "mounts": "directory mounts", "storage": "Persistent on your server",
        "storage_line": "SQLite · chat history · workspace / home / shared", "persistence": "Stop a container. Keep your work.",
        "tunnel": "OPTIONAL  /  REACH YOUR PRIVATE NETWORK", "no_port": "No inbound port on your computer",
        "relay": "Workspace → server relay", "link": "abox-link on your computer", "internal": "Internal services",
        "outbound": "Client initiates the connection", "allowlist": "Allowlist", "services": "Git · databases · APIs",
        "desc": "A browser or optional desktop client connects to the agentbox Go server over HTTPS and WebSocket. The Linux host runs a local Docker engine with one workspace container running Claude Code or Codex. The CLI calls model APIs, optionally through an account egress proxy. SQLite, chat history and workspace, home and shared directories persist on the host. Optional abox-link initiates an outbound tunnel from the user's computer to the server. Workspace requests pass through the server and tunnel to services allowed by the local client.",
    },
    "cn": {
        "title": "你的智能体，你的基础设施。",
        "subtitle": "一个 Go 服务端，持久化工作空间，随时随地访问。",
        "access": "01  你的设备", "host": "02  你的 LINUX 服务器", "model": "03  模型服务",
        "browser": "浏览器", "browser_lines": ["对话 · 终端 · 文件", "电脑、平板或手机"],
        "desktop": "桌面客户端", "desktop_lines": ["可选客户端", "macOS · Windows"],
        "host_label": "GO 单二进制 + 本机 DOCKER ENGINE", "server": "agentbox 服务端", "control": "统一管理与调度",
        "server_lines": ["用户与访问授权", "账号池与凭证", "工作空间生命周期", "用量与额度"],
        "workspace": "工作空间容器", "agent": "Claude Code 或 Codex", "workspace_lines": ["流式对话 · 原生 CLI", "Shell · tmux · Git"],
        "per_workspace": "每个工作空间一个容器", "providers": "模型 API", "provider_lines": ["Anthropic", "OpenAI / 中转服务"],
        "provider_note": ["由 CLI 调用模型服务", "可选：按账号指定", "出口代理"],
        "state": "状态与聊天记录", "mounts": "挂载持久化目录", "storage": "数据持久保存在你的服务器",
        "storage_line": "SQLite · 聊天记录 · workspace / home / shared", "persistence": "容器可以停止，工作成果继续保留。",
        "tunnel": "可选能力  /  访问你的内网", "no_port": "你的电脑无需开放入站端口",
        "relay": "工作空间 → 服务端转发", "link": "本机运行 abox-link", "internal": "内网服务",
        "outbound": "由本机客户端主动连接服务端", "allowlist": "白名单校验", "services": "Git · 数据库 · API",
        "desc": "浏览器或可选的桌面客户端通过 HTTPS 和 WebSocket 连接 agentbox Go 服务端。Linux 服务器通过本机 Docker Engine 为每个工作空间创建运行 Claude Code 或 Codex 的容器。CLI 调用模型 API，可按账号配置出口代理。SQLite、聊天记录以及 workspace、home、shared 目录持久保存在宿主机。可选的 abox-link 从用户本机主动连接服务端，工作空间请求经服务端与隧道访问本机白名单允许的内网服务。",
    },
}


def render(lang):
    t = LABELS[lang]
    parts = []
    def add(s): parts.append(s)
    def text(x, y, value, cls="body", anchor=None):
        attrs = f' text-anchor="{anchor}"' if anchor else ""
        add(f'<text x="{x}" y="{y}" class="{cls}"{attrs}>{escape(value)}</text>')
    def lines(x, y, values, cls="body", step=28):
        for i, value in enumerate(values): text(x, y + step * i, value, cls)
    def box(x, y, w, h, cls="card", radius=14):
        add(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{radius}" class="{cls}"/>')
    def path(d, cls="flow", arrow=True):
        marker = ' marker-end="url(#arrow)"' if arrow else ""
        add(f'<path d="{d}" class="{cls}"{marker}/>')

    add(f'''<svg xmlns="http://www.w3.org/2000/svg" width="1280" height="872" viewBox="0 0 1280 872" role="img" aria-labelledby="title desc" lang="{'zh-CN' if lang == 'cn' else 'en'}">
<title id="title">{escape(t['title'])} — agentbox</title>
<desc id="desc">{escape(t['desc'])}</desc>
<defs>
  <marker id="arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M1 1 L9 5 L1 9" fill="none" stroke="#e8a33d" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/></marker>
  <style>
    text {{ font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', 'Noto Sans CJK SC', 'PingFang SC', 'Microsoft YaHei', sans-serif; fill: #f4efe6; }}
    .title {{ font-size: 37px; font-weight: 650; letter-spacing: -.8px; }}
    .subtitle {{ font-size: 21px; fill: #bdb5a8; }}
    .lane {{ font-size: 16px; font-weight: 600; letter-spacing: 1.2px; fill: #bdb5a8; }}
    .heading {{ font-size: 24px; font-weight: 600; letter-spacing: -.35px; }}
    .body {{ font-size: 18px; fill: #d5cec1; }}
    .muted {{ font-size: 16px; fill: #aba396; }}
    .edge {{ font-size: 15px; fill: #d2b483; }}
    .storage-title {{ font-size: 22px; font-weight: 600; }}
    .tunnel-heading {{ font-size: 20px; font-weight: 550; }}
    .overline {{ font-size: 15px; font-weight: 600; letter-spacing: .7px; fill: #c8b491; }}
    .pill {{ font-size: 12px; font-weight: 600; letter-spacing: .8px; fill: #e8bb74; }}
    .card {{ fill: #23221f; stroke: #4a4439; stroke-width: 1.5; }}
    .control {{ fill: #30271c; stroke: #8a6835; stroke-width: 1.5; }}
    .boundary {{ fill: #191917; stroke: #3d3a32; stroke-width: 1.5; }}
    .flow {{ fill: none; stroke: #e8a33d; stroke-width: 1.7; stroke-linecap: round; stroke-linejoin: round; }}
    .optional {{ fill: none; stroke: #e8a33d; stroke-width: 1.7; stroke-dasharray: 5 5; stroke-linecap: round; }}
    .rule {{ stroke: #494237; stroke-width: 1; }}
  </style>
</defs>
<rect width="1280" height="872" rx="20" fill="#121311"/>''')
    text(48, 66, t['title'], 'title')
    text(49, 102, t['subtitle'], 'subtitle')
    # Project-native vector mark, adapted from internal/web/static/img/logo.svg.
    add('''<g transform="translate(1157 36) scale(2.05)" aria-hidden="true">
<path d="M5.6 12 L1.2 5.8 L4.9 2.9 L9.9 9.9 Z M26.4 12 L30.8 5.8 L27.1 2.9 L22.1 9.9 Z" fill="#e8a33d"/>
<rect x="5" y="11.4" width="22" height="16.2" rx="3" fill="#e8a33d"/><rect x="8" y="11.4" width="16" height="6.6" rx="1.8" fill="#241803"/>
<rect x="10.6" y="12.7" width="4" height="4" rx="1.3" fill="#ffc76b"/><rect x="17.4" y="12.7" width="4" height="4" rx="1.3" fill="#ffc76b"/>
<path d="M10.6 20.3 L13.7 22.8 L10.6 25.3" stroke="#8a5f1f" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/><rect x="16.4" y="23.9" width="4.6" height="2" rx="1" fill="#8a5f1f"/>
</g>''')
    text(48, 167, t['access'], 'lane')
    text(350, 167, t['host'], 'lane')
    text(1006, 167, t['model'], 'lane')
    box(322, 190, 642, 437, 'boundary', 18)
    text(350, 223, t['host_label'], 'overline')
    # Entry points share a single connection to the control plane.
    box(48, 249, 210, 226)
    text(68, 285, t['browser'], 'heading')
    lines(68, 316, t['browser_lines'], 'muted', 25)
    add('<path d="M68 366 H238" class="rule"/>')
    text(68, 402, t['desktop'], 'heading')
    lines(68, 430, t['desktop_lines'], 'muted', 24)
    path('M258 355 H350')
    text(304, 332, 'HTTPS', 'edge', 'middle')
    text(304, 382, 'WebSocket', 'edge', 'middle')
    # Main control plane and runtime are separate roles, on the same host.
    box(350, 249, 244, 198, 'control')
    text(370, 285, t['server'], 'heading')
    text(370, 311, t['control'], 'muted')
    add('<path d="M370 327 H574" class="rule"/>')
    lines(370, 351, t['server_lines'], 'body', 25)
    box(666, 241, 268, 198, 'boundary')
    box(659, 249, 275, 198)
    text(679, 285, t['workspace'], 'heading')
    text(679, 318, t['agent'], 'body')
    lines(679, 356, t['workspace_lines'], 'muted', 27)
    add('<rect x="675" y="406" width="243" height="24" rx="6" fill="#332a1d"/>')
    text(796.5, 423, t['per_workspace'], 'pill', 'middle')
    path('M594 355 H659')
    text(626, 320, 'Docker', 'edge', 'middle')
    text(626, 339, 'API', 'edge', 'middle')
    # Direct model path: the Go process manages workspaces, the CLI calls providers.
    box(1006, 269, 226, 145)
    text(1026, 308, t['providers'], 'heading')
    lines(1026, 342, t['provider_lines'], 'body', 28)
    path('M934 355 H1006')
    text(970, 321, 'Model', 'edge', 'middle')
    text(970, 339, 'API', 'edge', 'middle')
    lines(1006, 453, t['provider_note'], 'muted', 25)
    # Host state and bind-mounted directories survive container stops.
    path('M472 447 V509')
    text(486, 482, t['state'], 'edge')
    path('M797 447 V509')
    text(810, 482, t['mounts'], 'edge')
    box(350, 509, 584, 86)
    text(372, 544, t['storage'], 'storage-title')
    text(372, 574, t['storage_line'], 'body')
    text(350, 654, t['persistence'], 'muted')
    # Separate optional lane shows request direction and who establishes the tunnel.
    box(48, 687, 1184, 155, 'boundary', 16)
    text(72, 717, t['tunnel'], 'overline')
    text(1208, 717, t['no_port'], 'muted', 'end')
    text(72, 765, t['relay'], 'tunnel-heading')
    text(584, 765, t['link'], 'tunnel-heading')
    text(1040, 765, t['internal'], 'tunnel-heading')
    path('M340 759 H562', 'optional')
    text(451, 749, 'WSS / yamux', 'edge', 'middle')
    path('M874 759 H1020', 'optional')
    text(947, 749, t['allowlist'], 'edge', 'middle')
    text(1040, 794, t['services'], 'muted')
    path('M562 797 H340', 'optional')
    text(451, 826, t['outbound'], 'muted', 'middle')
    add('</svg>')
    return '\n'.join(parts) + '\n'


for language, filename in [('en', 'architecture.svg'), ('cn', 'architecture-cn.svg')]:
    destination = ROOT / 'docs/images' / filename
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(render(language), encoding='utf-8')
    print(destination.relative_to(ROOT))
