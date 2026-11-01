#!/usr/bin/env python3
"""Generate the landing page (site/index.html and site/zh/index.html).

The page is plain HTML/CSS with a little JS. Copy lives in STRINGS below;
edit it there and run `python3 scripts/build-site.py`. No dependencies.
"""
from html import escape
from pathlib import Path

SITE = Path(__file__).resolve().parent.parent / "site"
URL = "https://agent-kanban.lzhgus.com"
REPO = "https://github.com/lzhgus/agent-kanban"
# Default download link: the DMG of the current release. The page asks the
# GitHub API for the latest release when it loads and swaps in its DMG and
# version, so this only matters if that call fails. Bump it on release.
DMG_VERSION = "0.1.0"
DMG_URL = f"{REPO}/releases/download/v{DMG_VERSION}/agent-kanban-{DMG_VERSION}.dmg"
RELEASES_API = "https://api.github.com/repos/lzhgus/agent-kanban/releases/latest"

ICONS = {
    "chat": '<path d="M4 5h16v11H9l-5 4z"/>',
    "branch": '<circle cx="6" cy="5" r="2"/><circle cx="6" cy="19" r="2"/><circle cx="18" cy="8" r="2"/><path d="M6 7v10M18 10c0 5-12 3-12 7"/>',
    "plan": '<path d="M9 4h6v3H9zM6 6h2m8 0h2v15H6V6"/><path d="M9 12l2 2 4-4M9 17h6"/>',
    "review": '<circle cx="11" cy="11" r="6"/><path d="M20 20l-4.5-4.5M8.5 11l2 2 3.5-3.5"/>',
    "mcp": '<rect x="3" y="4" width="7" height="7" rx="2"/><rect x="14" y="13" width="7" height="7" rx="2"/><path d="M10 7.5h4.5a2 2 0 0 1 2 2V13M14 16.5H9.5a2 2 0 0 1-2-2V11"/>',
    "usage": '<path d="M4 20V10M10 20V4M16 20v-7M22 20H2"/>',
    "queue": '<rect x="4" y="4" width="16" height="4" rx="1.5"/><rect x="4" y="10" width="16" height="4" rx="1.5"/><rect x="4" y="16" width="10" height="4" rx="1.5"/>',
    "auto": '<path d="M13 3L5 14h6l-1 7 8-11h-6z"/>',
    "terminal": '<rect x="3" y="4" width="18" height="16" rx="2.5"/><path d="M7 9l3 3-3 3M12 15h5"/>',
}

STRINGS = {
    "en": {
        "lang": "en", "path": "/", "other_path": "/zh/", "other_label": "中文", "other_hreflang": "zh-CN",
        "title": "Agent Kanban — One board for all your coding agents",
        "desc": "Open-source desktop app that runs Claude Code, GitHub Copilot CLI and Antigravity as tasks on one kanban board: a git branch per task, plan first, AI review, and every agent's tokens and quota in one place. Local, no API keys.",
        "og_image": "/og-en.png", "og_locale": "en_US",
        "nav_features": "Features", "nav_flow": "How it works", "nav_start": "Get started",
        "star": "Star on GitHub", "skip": "Skip to content",
        "eyebrow": "Open source · MIT · macOS desktop app",
        "h1": 'One board for all your <span class="hl">coding agents</span>',
        "lede": "Agent Kanban runs Claude Code, GitHub Copilot CLI and Antigravity as tasks on a kanban board. Each task gets its own git branch. Agents plan, review each other and ask you questions, and you see every agent's progress, tokens and plan quota in one place.",
        "cta_primary": "Get started", "cta_secondary": "View on GitHub",
        "copy": "Copy", "copied": "Copied",
        "video_label": "20-second promo: a task goes from To Do to Running, Review and Done",
        "pause": "Pause video", "play": "Play video",
        "demo_link": "Open the interactive demo", "demo_note": "Fully mocked data, no real work.",
        "agents_kicker": "Works with the CLIs you already use",
        "agents_h": "Your subscriptions. Your agents. One board.",
        "agents_p": "Agent Kanban drives the agent CLIs you have installed and signed in to. No API keys, no account, no server.",
        "agents": [
            ("claude", "Claude Code", "Runs tasks · board tools (MCP) · quota", "Tested", "ok"),
            ("copilot", "GitHub Copilot CLI", "Runs tasks · board tools (MCP) · quota", "Being tested", "beta"),
            ("anti", "Antigravity CLI (agy)", "Runs tasks · quota", "Being tested", "beta"),
            ("codex", "Codex", "Sessions watched read-only · quota", "Read-only", "ok"),
        ],
        "status": "<b>Status: early.</b> It works day to day with Claude Code. GitHub Copilot CLI and Antigravity support is implemented and being tested. Expect rough edges.",
        "feat_kicker": "What it does", "feat_h": "Less terminal juggling. More shipped tasks.",
        "features": [
            ("chat", "Tasks are conversations", "Start a task, then reply to ask for changes. The agent picks up where it stopped, in the same session."),
            ("branch", "A branch per task", "Every task works in its own worktree on a <code>kanban/&lt;id&gt;-&lt;title&gt;</code> branch. Each turn ends in a commit; review the diff and <b>Merge</b> when you're ready."),
            ("plan", "Plan first", "Start with a read-only planning turn. The plan waits for you to confirm or revise it before any code changes."),
            ("review", "AI review", "Have another agent review a plan or finished changes. The loop runs until the reviewer approves or your rounds run out. The decision stays yours."),
            ("mcp", "Agents work together", "A built-in local MCP server lets agents ask you a question, file tasks for other agents and read the board."),
            ("usage", "Usage at a glance", "Tokens and cost per task and per agent, plus each agent's plan windows (Claude's 5-hour and weekly limits, Copilot's premium requests) and when they reset."),
            ("queue", "Queue", "Choose how many agents run at once. The rest wait their turn."),
            ("auto", "Auto pickup", "Turn on <b>Auto</b> and free agents take To Do tasks first come, first served. A task for any agent goes to the one with the most quota left."),
            ("terminal", "Sessions outside the board", "Claude Code, Codex and Antigravity sessions you run in a terminal show up too, read-only, with their token usage."),
        ],
        "flow_kicker": "How a task flows", "flow_h": "From idea to merged, on one board.",
        "flow": [
            ("todo", "To Do", "Filed, not started, or waiting for a free agent with Auto on."),
            ("run", "Running", "An agent is working in the task's own worktree, or Queued for a free slot."),
            ("confirm", "Confirm", "A plan, or an agent's question, waits for you."),
            ("review", "Review", "The turn is done: reply, ask for an AI review, merge, or hand it off."),
            ("done", "Done", "Finished. Outside sessions land here after two idle hours."),
        ],
        "flow_plan": "With <b>Plan first</b>: To Do → Running (plan) → Confirm → Running → Review → Done",
        "priv_kicker": "Local-first", "priv_h": "Everything runs on your machine.",
        "privacy": [
            ("No keys, no account, no server", "It uses the agent CLIs you're already signed in to, so your existing subscriptions just work."),
            ("A local MCP server", "It listens on 127.0.0.1 only, and each turn gets its own URL that stops working when the turn ends."),
            ("Your board stays yours", "The board and worktrees live in your Application Support folder. Deleting a task keeps its branch in your repository."),
        ],
        "priv_warn": "<b>Heads-up:</b> agents run headless with their permission prompts turned off, inside the task's worktree. Only point them at code you trust them with. Planning and review turns are read-only.",
        "dl": "Download for macOS", "dl_meta": "Universal · macOS 12+ · signed &amp; notarized", "dl_src": "or build from source",
        "start_kicker": "Get started", "start_h": "Running in a minute.",
        "start_dl_h": "Install the app",
        "start_dl_steps": [
            "Download the DMG: one universal app for Apple Silicon and Intel Macs, macOS 12 or newer.",
            "Open it and drag <b>Agent Kanban</b> to Applications.",
            "Open it from Applications, the Dock or Spotlight. It's signed and notarized by Apple.",
        ],
        "start_dl_req": "You still need git and at least one of the agent CLIs above, installed and signed in. No Go required.",
        "start_src_h": "Or build from source",
        "start_req": 'You need macOS, <a href="https://go.dev/dl/">Go</a> 1.27.3 or newer, git, and at least one of the agent CLIs above, installed and signed in.',
        "start_run": "Run from source", "start_build": "Build the app",
        "start_run_after": "Click <b>New task</b>, describe what to do, pick an agent and a folder, and press <b>Add &amp; Run</b>. Tick <b>Plan first</b> to review a plan before the agent changes anything.",
        "start_build_after": "Keep it in Applications and open it from the Dock or Spotlight. The app and <code>go run .</code> share the same board. Linux should work but is untested; Windows isn't supported yet.",
        "cta_h": "Free and open source.", "cta_p": "MIT licensed. Download the app, read the code, file an issue, or send a pull request. A star helps other people find it.",
        "cta_readme": "Read the README",
        "foot_license": "MIT License", "foot_built": 'Built with <a href="https://github.com/egoist/mygo">MyGo</a>, a native UI in pure Go.',
        "foot_demo": "Demo", "demo_path": "/demo/",
    },
    "zh": {
        "lang": "zh-CN", "path": "/zh/", "other_path": "/", "other_label": "EN", "other_hreflang": "en",
        "title": "Agent Kanban：一个看板管所有 coding agent",
        "desc": "开源桌面应用：把 Claude Code、GitHub Copilot CLI 和 Antigravity 当作看板上的任务来运行。每个任务一个 git 分支，先计划再动手，AI 互相审查，所有 agent 的 token 用量和额度一目了然。全部本地运行，无需 API key。",
        "og_image": "/og-zh.png", "og_locale": "zh_CN",
        "nav_features": "功能", "nav_flow": "工作流程", "nav_start": "开始使用",
        "star": "在 GitHub 上 Star", "skip": "跳到正文",
        "eyebrow": "开源 · MIT 协议 · macOS 桌面应用",
        "h1": '一个看板管所有 <span class="hl">coding agent</span>',
        "lede": "Agent Kanban 把 Claude Code、GitHub Copilot CLI 和 Antigravity 当作看板上的任务来运行。每个任务在自己的 git 分支上工作；agent 会先做计划、互相审查、向你提问，每个 agent 的进度、token 用量和套餐额度都在一个地方看清楚。",
        "cta_primary": "开始使用", "cta_secondary": "查看 GitHub",
        "copy": "复制", "copied": "已复制",
        "video_label": "20 秒演示：一个任务从 To Do 走到 Running、Review 和 Done",
        "pause": "暂停视频", "play": "播放视频",
        "demo_link": "打开交互式演示", "demo_note": "全部为模拟数据，不含真实工作内容。",
        "agents_kicker": "直接使用你已安装的 CLI",
        "agents_h": "你的订阅，你的 agent，一个看板。",
        "agents_p": "Agent Kanban 驱动你已经安装并登录的 agent CLI。不需要 API key，不需要注册账号，也没有服务器。",
        "agents": [
            ("claude", "Claude Code", "运行任务 · 看板工具（MCP）· 额度", "已测试", "ok"),
            ("copilot", "GitHub Copilot CLI", "运行任务 · 看板工具（MCP）· 额度", "测试中", "beta"),
            ("anti", "Antigravity CLI（agy）", "运行任务 · 额度", "测试中", "beta"),
            ("codex", "Codex", "只读查看会话 · 额度", "只读", "ok"),
        ],
        "status": "<b>状态：早期版本。</b>日常使用 Claude Code 已经没问题；GitHub Copilot CLI 和 Antigravity 的支持已实现，正在测试。难免有粗糙的地方。",
        "feat_kicker": "功能", "feat_h": "少在终端之间来回切，多交付几个任务。",
        "features": [
            ("chat", "任务就是对话", "开一个任务，之后直接回复它提修改意见：agent 会在同一个会话里接着上次的地方继续。"),
            ("branch", "每个任务一个分支", "每个任务在自己的 worktree 里、<code>kanban/&lt;id&gt;-&lt;title&gt;</code> 分支上工作。每一轮以一次 commit 结束；看完 diff，准备好了再点 <b>Merge</b>。"),
            ("plan", "先计划", "任务可以从一轮只读的计划开始。计划等你确认或修改后，才会动代码。"),
            ("review", "AI 审查", "让另一个 agent 审查计划或完成的改动。任务的 agent 修改，循环直到审查通过或你设定的轮数用完。最终决定权在你。"),
            ("mcp", "agent 之间协作", "内置的本地 MCP 服务器让 agent 可以向你提问、给其他 agent 创建任务、读取看板。"),
            ("usage", "用量一目了然", "按任务、按 agent 统计 token 和费用，还有每个 agent 的套餐窗口（Claude 的 5 小时和每周限额、Copilot 的 premium requests 等）以及重置时间。"),
            ("queue", "排队", "设定同时运行几个 agent，其余的排队等候。"),
            ("auto", "自动领取", "打开 <b>Auto</b>，空闲的 agent 按先来先得领取 To Do 任务；指定给任意 agent 的任务交给剩余额度最多的那个。"),
            ("terminal", "看板外的会话", "你在终端里运行的 Claude Code、Codex 和 Antigravity 会话也会出现在看板上（只读），并显示 token 用量。"),
        ],
        "flow_kicker": "任务流程", "flow_h": "从想法到合并，全在一个看板上。",
        "flow": [
            ("todo", "To Do", "已创建、未开始；或在 Auto 模式下等待空闲的 agent。"),
            ("run", "Running", "agent 正在任务自己的 worktree 里工作，或在排队（Queued）等空位。"),
            ("confirm", "Confirm", "一份计划或 agent 的提问在等你回复。"),
            ("review", "Review", "这一轮做完了：回复、请 AI 审查、合并，或转交给别的 agent。"),
            ("done", "Done", "完成。看板外的会话空闲两小时后会移到这里。"),
        ],
        "flow_plan": "开启 <b>Plan first</b> 时：To Do → Running（计划）→ Confirm → Running → Review → Done",
        "priv_kicker": "本地优先", "priv_h": "一切都在你自己的电脑上运行。",
        "privacy": [
            ("无需 key、账号或服务器", "它使用你已经登录的 agent CLI，现有的订阅直接就能用。"),
            ("本地 MCP 服务器", "只监听 127.0.0.1，每一轮都有自己的 URL，这一轮结束后即失效。"),
            ("看板数据只属于你", "看板和 worktree 保存在你的 Application Support 目录里。删除任务会保留它在仓库里的分支。"),
        ],
        "priv_warn": "<b>注意：</b>agent 以无头模式运行，并关闭了权限确认，工作范围在任务的 worktree 内。只让它们处理你信得过的代码。计划和审查轮次是只读的。",
        "dl": "下载 macOS 版", "dl_meta": "通用版 · macOS 12+ · 已签名并经 Apple 公证", "dl_src": "或从源码构建",
        "start_kicker": "开始使用", "start_h": "一分钟跑起来。",
        "start_dl_h": "安装应用",
        "start_dl_steps": [
            "下载 DMG：一个通用版应用，同时支持 Apple Silicon 和 Intel 芯片的 Mac，需要 macOS 12 或更新版本。",
            "打开 DMG，把 <b>Agent Kanban</b> 拖进“应用程序”文件夹。",
            "从“应用程序”、Dock 或 Spotlight 打开。应用已签名并经过 Apple 公证。",
        ],
        "start_dl_req": "你仍然需要 git，以及至少一个上面列出的 agent CLI（已安装并登录）。不需要安装 Go。",
        "start_src_h": "或者从源码构建",
        "start_req": '需要 macOS、<a href="https://go.dev/dl/">Go</a> 1.27.3 或更新版本、git，以及至少一个上面列出的 agent CLI（已安装并登录）。',
        "start_run": "从源码运行", "start_build": "构建应用",
        "start_run_after": "点 <b>New task</b>，描述要做什么，选择 agent 和文件夹，然后点 <b>Add &amp; Run</b>。勾选 <b>Plan first</b> 可以在 agent 改动任何东西之前先审阅计划。",
        "start_build_after": "放进“应用程序”文件夹后，就能从 Dock 或 Spotlight 打开。应用和 <code>go run .</code> 共用同一个看板。Linux 应该可以运行但未经测试；暂不支持 Windows。",
        "cta_h": "免费、开源。", "cta_p": "MIT 协议。欢迎下载使用、阅读代码、提 issue 或发 pull request。点个 Star 能帮更多人发现它。",
        "cta_readme": "阅读 README",
        "foot_license": "MIT 许可证", "foot_built": '基于 <a href="https://github.com/egoist/mygo">MyGo</a> 构建：纯 Go 绘制的原生界面。',
        "foot_demo": "演示", "demo_path": "/zh/demo/",
    },
}

RUN_CMD = "git clone https://github.com/lzhgus/agent-kanban.git\ncd agent-kanban\ngo run ."
BUILD_CMD = 'go run github.com/egoist/mygo/cmd/mygo build -skip-dmg\ncp -R "build/darwin-arm64/Agent Kanban.app" /Applications/'

GH_SVG = '<svg viewBox="0 0 16 16" aria-hidden="true" width="16" height="16"><path fill="currentColor" d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>'
DL_SVG = '<svg viewBox="0 0 24 24" aria-hidden="true" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 4v11M7 10.5l5 5 5-5M5 20h14"/></svg>'
STAR_SVG = '<svg viewBox="0 0 16 16" aria-hidden="true" width="16" height="16"><path fill="currentColor" d="M8 .25a.75.75 0 01.67.42l1.88 3.81 4.2.61a.75.75 0 01.42 1.28l-3.04 2.96.72 4.19a.75.75 0 01-1.09.79L8 12.33l-3.76 1.98a.75.75 0 01-1.09-.79l.72-4.19L.83 6.37a.75.75 0 01.42-1.28l4.2-.61L7.33.67A.75.75 0 018 .25z"/></svg>'


def icon(name):
    return f'<svg class="ficon" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">{ICONS[name]}</svg>'


def codeblock(cmd, t, label):
    return (f'<div class="code"><div class="code-head"><span>{label}</span>'
            f'<button class="copy" type="button" data-copy="{escape(cmd)}" data-done="{t["copied"]}">{t["copy"]}</button></div>'
            f'<pre><code>{escape(cmd)}</code></pre></div>')


def dl_button(t, extra=""):
    return f'<a class="btn btn-primary{extra}" href="{DMG_URL}" data-dl>{DL_SVG}{t["dl"]}</a>'


def dl_meta(t, src=True):
    link = f' <span class="nw">· <a href="#source">{t["dl_src"]}</a></span>' if src else ""
    return f'<p class="dl-meta"><span data-dl-ver>v{DMG_VERSION}</span> · {t["dl_meta"]}{link}</p>'


# Swap in the latest release's DMG and version. Fails silently: the links
# keep pointing at DMG_URL. Cached per tab session to spare the rate limit.
DL_JS = ("<script>(function(){var K='ak-latest',P='" + REPO + "/releases/download/';"
         "function apply(d){if(!d||!d.u||d.u.indexOf(P)!==0)return;"
         "document.querySelectorAll('a[data-dl]').forEach(function(a){a.href=d.u});"
         "if(d.v)document.querySelectorAll('[data-dl-ver]').forEach(function(e){e.textContent=d.v})}"
         "try{var c=JSON.parse(sessionStorage.getItem(K));if(c){apply(c);return}}catch(e){}"
         "if(!window.fetch)return;"
         "fetch('" + RELEASES_API + "',{headers:{Accept:'application/vnd.github+json'}})"
         ".then(function(r){return r.ok?r.json():null}).then(function(j){if(!j||!j.assets)return;"
         "var a=j.assets.filter(function(x){return /\\.dmg$/i.test(x.name)})[0];if(!a)return;"
         "var d={u:a.browser_download_url,v:String(j.tag_name||'').slice(0,24)};apply(d);"
         "try{sessionStorage.setItem(K,JSON.stringify(d))}catch(e){}}).catch(function(){})})();</script>")


def page(key):
    t = STRINGS[key]
    lang = "zh" if key == "zh" else "en"
    canonical = URL + t["path"]
    agents = "".join(
        f'<li class="agent"><div class="ahead"><span class="adot {a}" aria-hidden="true"></span><b>{n}</b></div><span class="adesc">{d}</span><em class="tag {c}">{s}</em></li>'
        for a, n, d, s, c in t["agents"])
    feats = "".join(f'<li class="feat">{icon(i)}<h3>{h}</h3><p>{p}</p></li>' for i, h, p in t["features"])
    flow = "".join(f'<li class="step {k}"><span class="sdot" aria-hidden="true"></span><h3>{h}</h3><p>{p}</p></li>' for k, h, p in t["flow"])
    priv = "".join(f'<li><h3>{h}</h3><p>{p}</p></li>' for h, p in t["privacy"])
    steps = "".join(f'<li>{x}</li>' for x in t["start_dl_steps"])
    return f'''<!doctype html>
<html lang="{t["lang"]}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>{t["title"]}</title>
<meta name="description" content="{escape(t["desc"])}">
<meta name="color-scheme" content="dark light">
<meta name="theme-color" content="#0c0d0c" media="(prefers-color-scheme: dark)">
<meta name="theme-color" content="#f3f2ec" media="(prefers-color-scheme: light)">
<link rel="canonical" href="{canonical}">
<link rel="alternate" hreflang="en" href="{URL}/">
<link rel="alternate" hreflang="zh-CN" href="{URL}/zh/">
<link rel="alternate" hreflang="x-default" href="{URL}/">
<link rel="icon" type="image/png" sizes="32x32" href="/favicon-32.png">
<link rel="apple-touch-icon" href="/apple-touch-icon.png">
<meta property="og:type" content="website">
<meta property="og:site_name" content="Agent Kanban">
<meta property="og:title" content="{escape(t["title"])}">
<meta property="og:description" content="{escape(t["desc"])}">
<meta property="og:url" content="{canonical}">
<meta property="og:image" content="{URL}{t["og_image"]}">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="630">
<meta property="og:locale" content="{t["og_locale"]}">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="{escape(t["title"])}">
<meta name="twitter:description" content="{escape(t["desc"])}">
<meta name="twitter:image" content="{URL}{t["og_image"]}">
<link rel="preconnect" href="https://api.github.com" crossorigin>
<link rel="preload" as="image" href="/media/poster-{lang}.webp" fetchpriority="high">
<link rel="stylesheet" href="/styles.css">
<script type="application/ld+json">{{"@context":"https://schema.org","@type":"SoftwareApplication","name":"Agent Kanban","applicationCategory":"DeveloperApplication","operatingSystem":"macOS 12+","downloadUrl":"{DMG_URL}","softwareVersion":"{DMG_VERSION}","license":"https://opensource.org/licenses/MIT","url":"{canonical}","codeRepository":"{REPO}","offers":{{"@type":"Offer","price":"0","priceCurrency":"USD"}}}}</script>
</head>
<body class="lang-{lang}">
<a class="skip" href="#main">{t["skip"]}</a>
<header class="nav">
  <div class="wrap nav-in">
    <a class="brand" href="{t["path"]}"><img src="/logo-96.png" width="28" height="28" alt=""> Agent Kanban</a>
    <nav aria-label="Primary" class="links">
      <a href="#features">{t["nav_features"]}</a>
      <a href="#flow">{t["nav_flow"]}</a>
      <a href="#start">{t["nav_start"]}</a>
    </nav>
    <div class="nav-r">
      <a class="lang" href="{t["other_path"]}" hreflang="{t["other_hreflang"]}" lang="{t["other_hreflang"]}">{t["other_label"]}</a>
      <a class="btn btn-ghost btn-sm" href="{REPO}">{GH_SVG}<span class="hide-s">GitHub</span></a>
    </div>
  </div>
</header>

<main id="main">
<section class="hero">
  <div class="wrap">
    <p class="eyebrow"><span class="pulse" aria-hidden="true"></span>{t["eyebrow"]}</p>
    <h1>{t["h1"]}</h1>
    <p class="lede">{t["lede"]}</p>
    <div class="ctas">
      {dl_button(t, " btn-lg")}
      <a class="btn btn-ghost btn-lg" href="{REPO}">{STAR_SVG}{t["star"]}</a>
    </div>
    {dl_meta(t)}
  </div>
  <div class="wrap wide">
    <figure class="stage">
      <div class="glow" aria-hidden="true"></div>
      <div class="frame">
        <video id="promo" autoplay muted loop playsinline preload="metadata" poster="/media/poster-{lang}.webp" width="1144" height="646" aria-label="{t["video_label"]}">
          <source src="/media/promo-{lang}.mp4" type="video/mp4">
        </video>
        <button class="vctl" type="button" id="vctl" data-pause="{t["pause"]}" data-play="{t["play"]}" aria-label="{t["pause"]}"><span class="i-pause" aria-hidden="true"></span></button>
      </div>
      <figcaption><a href="{t["demo_path"]}">{t["demo_link"]} →</a> <span>{t["demo_note"]}</span></figcaption>
    </figure>
  </div>
</section>

<section class="sec" id="agents" aria-labelledby="agents-h">
  <div class="wrap">
    <p class="kicker">{t["agents_kicker"]}</p>
    <h2 id="agents-h">{t["agents_h"]}</h2>
    <p class="sub">{t["agents_p"]}</p>
    <ul class="agents">{agents}</ul>
    <p class="status">{t["status"]}</p>
  </div>
</section>

<section class="sec" id="features" aria-labelledby="features-h">
  <div class="wrap">
    <p class="kicker">{t["feat_kicker"]}</p>
    <h2 id="features-h">{t["feat_h"]}</h2>
    <ul class="feats">{feats}</ul>
  </div>
</section>

<section class="sec" id="flow" aria-labelledby="flow-h">
  <div class="wrap">
    <p class="kicker">{t["flow_kicker"]}</p>
    <h2 id="flow-h">{t["flow_h"]}</h2>
    <ol class="flow">{flow}</ol>
    <p class="flow-plan">{t["flow_plan"]}</p>
  </div>
</section>

<section class="sec" id="privacy" aria-labelledby="privacy-h">
  <div class="wrap">
    <div class="band">
      <p class="kicker">{t["priv_kicker"]}</p>
      <h2 id="privacy-h">{t["priv_h"]}</h2>
      <ul class="priv">{priv}</ul>
      <p class="warn">{t["priv_warn"]}</p>
    </div>
  </div>
</section>

<section class="sec" id="start" aria-labelledby="start-h">
  <div class="wrap">
    <p class="kicker">{t["start_kicker"]}</p>
    <h2 id="start-h">{t["start_h"]}</h2>
    <div class="install">
      <div>
        <h3>{t["start_dl_h"]}</h3>
        <ol class="isteps">{steps}</ol>
        <p class="ireq">{t["start_dl_req"]}</p>
      </div>
      <div class="install-cta">
        {dl_button(t, " btn-lg")}
        {dl_meta(t, src=False)}
      </div>
    </div>
    <h3 class="src-h" id="source">{t["start_src_h"]}</h3>
    <p class="sub">{t["start_req"]}</p>
    <div class="starts">
      <div>{codeblock(RUN_CMD, t, t["start_run"])}<p>{t["start_run_after"]}</p></div>
      <div>{codeblock(BUILD_CMD, t, t["start_build"])}<p>{t["start_build_after"]}</p></div>
    </div>
  </div>
</section>

<section class="sec">
  <div class="wrap">
    <div class="cta">
      <img src="/logo-96.png" width="64" height="64" alt="" loading="lazy">
      <h2>{t["cta_h"]}</h2>
      <p>{t["cta_p"]}</p>
      <div class="ctas">
        {dl_button(t)}
        <a class="btn btn-ghost" href="{REPO}">{STAR_SVG}{t["star"]}</a>
        <a class="btn btn-ghost" href="{REPO}#readme">{t["cta_readme"]}</a>
      </div>
      {dl_meta(t, src=False)}
    </div>
  </div>
</section>
</main>

<footer class="foot">
  <div class="wrap foot-in">
    <a class="brand" href="{t["path"]}"><img src="/logo-96.png" width="22" height="22" alt="" loading="lazy"> Agent Kanban</a>
    <p>{t["foot_built"]}</p>
    <nav aria-label="Footer">
      <a href="{REPO}">GitHub</a>
      <a href="{REPO}/blob/main/LICENSE">{t["foot_license"]}</a>
      <a href="{t["demo_path"]}">{t["foot_demo"]}</a>
      <a href="{t["other_path"]}" hreflang="{t["other_hreflang"]}" lang="{t["other_hreflang"]}">{t["other_label"]}</a>
    </nav>
  </div>
</footer>
{DL_JS}
<script src="/site.js" defer></script>
</body>
</html>
'''


def main():
    (SITE / "index.html").write_text(page("en"), encoding="utf-8")
    (SITE / "zh").mkdir(exist_ok=True)
    (SITE / "zh" / "index.html").write_text(page("zh"), encoding="utf-8")
    print("wrote site/index.html and site/zh/index.html")


if __name__ == "__main__":
    main()
