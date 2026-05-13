#!/usr/bin/env node
'use strict';

const http = require('http');
const { spawn, execSync } = require('child_process');
const fs = require('fs');
const path = require('path');
const os = require('os');

const PORT = process.env.WIZARD_PORT || 8080;
// Resolve the service user home — wizard runs as root but cc-connect runs as ubuntu.
const SERVICE_USER = process.env.SERVICE_USER || 'ubuntu';
const SERVICE_HOME = (() => {
    try { return execSync(`getent passwd ${SERVICE_USER} | cut -d: -f6`, { encoding: 'utf8' }).trim(); } catch {}
    return `/home/${SERVICE_USER}`;
})();
const CONFIG_DIR = path.join(SERVICE_HOME, '.cc-connect');
const CONFIG_FILE = path.join(CONFIG_DIR, 'config.toml');
const ENV_FILE = path.join(CONFIG_DIR, 'agent.env');

// ── binary helpers ─────────────────────────────────────────────────────────────

function findBin(name) {
    for (const dir of [path.join(SERVICE_HOME, '.local/bin'), '/usr/local/bin', '/usr/bin']) {
        const p = path.join(dir, name);
        if (fs.existsSync(p)) return p;
    }
    try { return execSync(`which ${name}`, { encoding: 'utf8' }).trim(); } catch {}
    return null;
}

function runAs(cmd) {
    // Run a shell command as the service user (wizard may be root, cc-connect is not).
    return execSync(`sudo -u ${SERVICE_USER} ${cmd} 2>&1`, { encoding: 'utf8' });
}

function isClaudeAuthenticated() {
    try {
        const claude = findBin('claude');
        if (!claude) return false;
        const out = runAs(`${claude} auth status`);
        try {
            const j = JSON.parse(out);
            return j.loggedIn === true;
        } catch {
            return /logged.?in|authenticated/i.test(out);
        }
    } catch { return false; }
}

// ── config writers ─────────────────────────────────────────────────────────────

function writeAgentEnv(agent, apiKey) {
    fs.mkdirSync(CONFIG_DIR, { recursive: true });
    if (agent === 'gemini') {
        fs.writeFileSync(ENV_FILE, `GEMINI_API_KEY=${apiKey}\n`, { mode: 0o600 });
    } else if (agent === 'codex') {
        fs.writeFileSync(ENV_FILE, `OPENAI_API_KEY=${apiKey}\n`, { mode: 0o600 });
    } else {
        try { fs.unlinkSync(ENV_FILE); } catch {}
    }
}

function writeCredsFile(agent, credsJson) {
    if (agent === 'gemini') {
        const dir = path.join(SERVICE_HOME, '.gemini');
        fs.mkdirSync(dir, { recursive: true });
        fs.writeFileSync(path.join(dir, 'oauth_creds.json'), credsJson, { mode: 0o600 });
        try { fs.chownSync(path.join(dir, 'oauth_creds.json'), SERVICE_USER, SERVICE_USER); } catch {}
    } else if (agent === 'codex') {
        const dir = path.join(SERVICE_HOME, '.codex');
        fs.mkdirSync(dir, { recursive: true });
        fs.writeFileSync(path.join(dir, 'auth.json'), credsJson, { mode: 0o600 });
        try { fs.chownSync(path.join(dir, 'auth.json'), SERVICE_USER, SERVICE_USER); } catch {}
    }
}

function writeConfig(agent, platform, tokens) {
    fs.mkdirSync(CONFIG_DIR, { recursive: true });

    const agentBlocks = {
        claudecode: `[projects.agent]
type = "claudecode"

[projects.agent.options]
work_dir = "${SERVICE_HOME}"
mode = "bypassPermissions"`,
        gemini: `[projects.agent]
type = "gemini"

[projects.agent.options]
work_dir = "${SERVICE_HOME}"
mode = "yolo"`,
        codex: `[projects.agent]
type = "codex"

[projects.agent.options]
work_dir = "${SERVICE_HOME}"
mode = "full-auto"`,
    };

    const agentBlock = agentBlocks[agent] || agentBlocks.claudecode;

    let platformBlock;
    if (platform === 'telegram') {
        platformBlock = `[[projects.platforms]]
type = "telegram"

[projects.platforms.options]
token = "${tokens.token}"`;
    } else if (platform === 'discord') {
        platformBlock = `[[projects.platforms]]
type = "discord"

[projects.platforms.options]
token = "${tokens.token}"`;
    } else if (platform === 'slack') {
        platformBlock = `[[projects.platforms]]
type = "slack"

[projects.platforms.options]
bot_token = "${tokens.bot_token}"
app_token = "${tokens.app_token}"`;
    } else {
        throw new Error('Unsupported platform: ' + platform);
    }

    const config = `language = "en"

[display]
thinking_messages = false
tool_messages = false

[[projects]]
name = "assistant"

${agentBlock}

${platformBlock}
`;
    fs.writeFileSync(CONFIG_FILE, config, { mode: 0o600 });
}

// ── HTML ───────────────────────────────────────────────────────────────────────
const HTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>cc-connect Setup</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0f1117;color:#e2e8f0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:1rem}
.card{background:#1a1f2e;border:1px solid #2d3748;border-radius:12px;width:100%;max-width:520px;overflow:hidden}
.header{background:linear-gradient(135deg,#667eea,#764ba2);padding:2rem;text-align:center}
.header h1{font-size:1.5rem;font-weight:700;color:#fff}
.header p{color:rgba(255,255,255,.8);margin-top:.25rem;font-size:.9rem}
.tabs{display:flex;border-bottom:1px solid #2d3748}
.tab{flex:1;padding:.75rem;text-align:center;font-size:.7rem;color:#4a5568;border-bottom:2px solid transparent;transition:all .2s}
.tab.active{color:#667eea;border-bottom-color:#667eea}
.tab.done{color:#48bb78;border-bottom-color:#48bb78}
.body{padding:2rem}
.step{display:none}.step.active{display:block}
h2{font-size:1.1rem;font-weight:600;margin-bottom:.5rem}
p{color:#a0aec0;font-size:.9rem;line-height:1.6;margin-bottom:1rem}
.btn{display:block;width:100%;padding:.75rem;border:none;border-radius:8px;font-size:.95rem;font-weight:600;cursor:pointer;transition:opacity .2s}
.btn:disabled{opacity:.5;cursor:not-allowed}
.btn-primary{background:linear-gradient(135deg,#667eea,#764ba2);color:#fff}
.btn-primary:not(:disabled):hover{opacity:.9}
.btn-secondary{background:#2d3748;color:#e2e8f0;margin-top:.5rem}
.terminal{background:#0f1117;border:1px solid #2d3748;border-radius:8px;padding:1rem;font-family:monospace;font-size:.8rem;color:#68d391;min-height:80px;max-height:180px;overflow-y:auto;white-space:pre-wrap;word-break:break-all;margin-bottom:1rem}
.url-box{background:#1e3a5f;border:1px solid #2b6cb0;border-radius:8px;padding:1rem;margin-bottom:1rem;font-size:.85rem;color:#90cdf4;word-break:break-all}
.url-box a{color:#63b3ed}
label{display:block;font-size:.85rem;font-weight:500;margin-bottom:.35rem}
input,select{width:100%;padding:.65rem .75rem;background:#0f1117;border:1px solid #2d3748;border-radius:8px;color:#e2e8f0;font-size:.9rem;margin-bottom:1rem;outline:none}
input:focus{border-color:#667eea}
.hint{font-size:.8rem;color:#4a5568;margin-top:-.75rem;margin-bottom:1rem}
ol{padding-left:1.25rem;margin-bottom:1rem}
ol li{color:#a0aec0;font-size:.9rem;line-height:1.8}
ol li code{color:#e2e8f0}
textarea{width:100%;padding:.65rem .75rem;background:#0f1117;border:1px solid #2d3748;border-radius:8px;color:#e2e8f0;font-size:.8rem;font-family:monospace;margin-bottom:1rem;outline:none;resize:vertical}
textarea:focus{border-color:#667eea}
.alert-success{background:#1c4532;border:1px solid #276749;color:#9ae6b4;padding:.75rem 1rem;border-radius:8px;margin-bottom:1rem;font-size:.875rem}
.spinner{display:inline-block;width:14px;height:14px;border:2px solid rgba(255,255,255,.3);border-top-color:#fff;border-radius:50%;animation:spin .8s linear infinite;vertical-align:middle;margin-right:.4rem}
@keyframes spin{to{transform:rotate(360deg)}}
/* auth flow demo */
.auth-demo{position:relative;width:100%;height:110px;margin-bottom:1rem;overflow:hidden;border-radius:8px;background:#0a0d14;border:1px solid #2d3748}
.auth-demo-frame{position:absolute;inset:0;display:flex;flex-direction:column;align-items:center;justify-content:center;gap:.4rem;opacity:0;transition:opacity .7s;padding:.75rem;text-align:center}
.auth-demo-frame.active{opacity:1}
.auth-demo-step{font-size:.65rem;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:#667eea;margin-bottom:.1rem}
.auth-demo-label{font-size:.8rem;color:#e2e8f0;line-height:1.4}
.auth-demo-label em{color:#a0aec0;font-style:normal}
.demo-btn{display:inline-block;background:linear-gradient(135deg,#667eea,#764ba2);color:#fff;font-size:.7rem;font-weight:700;padding:.3rem .7rem;border-radius:5px;margin-top:.2rem}
.demo-code{font-family:monospace;font-size:1.05rem;letter-spacing:.18em;color:#68d391;background:#111827;border:1px solid #2d3748;padding:.25rem .65rem;border-radius:5px;margin-top:.2rem}
.demo-popup{background:#1a1f2e;border:1px solid #4a5568;border-radius:6px;padding:.5rem .9rem;font-size:.75rem;color:#e2e8f0;box-shadow:0 4px 16px rgba(0,0,0,.5);margin-top:.15rem}
.demo-tab-bar{display:flex;gap:.35rem;align-items:center;font-size:.65rem;color:#4a5568}
.demo-tab{background:#1a1f2e;border:1px solid #2d3748;border-radius:4px 4px 0 0;padding:.2rem .5rem;color:#a0aec0;font-size:.65rem}
.demo-tab.active-tab{color:#e2e8f0;border-bottom-color:#1a1f2e}
.demo-tab .close{color:#718096;margin-left:.3rem;cursor:pointer}
.demo-progress{position:absolute;bottom:0;left:0;height:2px;background:#667eea;transition:width .1s linear}
@keyframes cursor-blink{0%,100%{opacity:1}50%{opacity:0}}
.agent-grid,.platform-grid{display:grid;gap:.75rem;margin-bottom:1.25rem}
.agent-grid{grid-template-columns:repeat(3,1fr)}
.platform-grid{grid-template-columns:repeat(3,1fr)}
.tile{padding:.75rem;background:#0f1117;border:2px solid #2d3748;border-radius:8px;text-align:center;cursor:pointer;transition:all .2s;font-size:.85rem;color:#a0aec0}
.tile:hover{border-color:#667eea;color:#fff}
.tile.selected{border-color:#667eea;background:#1a1f2e;color:#fff}
.tile .icon{font-size:1.4rem;display:block;margin-bottom:.25rem}
.tile .sub{font-size:.7rem;color:#4a5568;margin-top:.2rem}
.tile.selected .sub{color:#a0aec0}
.success-icon{font-size:3rem;text-align:center;margin-bottom:1rem}
.slack-extra{display:none}.slack-extra.show{display:block}
code{background:#0f1117;padding:.1em .3em;border-radius:4px;font-size:.85em}
a{color:#63b3ed}
</style>
</head>
<body>
<div class="card">
  <div class="header">
    <h1>cc-connect Setup</h1>
    <p>Your personal AI assistant in minutes</p>
  </div>
  <div class="tabs">
    <div class="tab active" id="tab1">1. Welcome</div>
    <div class="tab" id="tab2">2. Agent</div>
    <div class="tab" id="tab3">3. Login</div>
    <div class="tab" id="tab4">4. Platform</div>
    <div class="tab" id="tab5">5. Done</div>
  </div>
  <div class="body">

    <!-- Step 1: Welcome -->
    <div class="step active" id="s1">
      <h2>Welcome</h2>
      <p>This wizard connects an AI coding agent to a messaging platform so you can chat 24/7 from Telegram, Discord, or Slack.</p>
      <p>You'll need:<br>
        • A <strong style="color:#e2e8f0">Claude</strong> (claude.ai), <strong style="color:#e2e8f0">Gemini</strong> (Google AI), or <strong style="color:#e2e8f0">OpenAI</strong> (ChatGPT) subscription<br>
        • A bot token for your messaging platform</p>
      <button class="btn btn-primary" onclick="goTo(2)">Get Started →</button>
    </div>

    <!-- Step 2: Agent selection -->
    <div class="step" id="s2">
      <h2>Choose your AI agent</h2>
      <p>Pick the AI you want to chat with.</p>
      <div class="agent-grid">
        <div class="tile" id="agent-claudecode" onclick="pickAgent('claudecode')">
          <span class="icon">🤖</span>Claude
          <div class="sub">Subscription</div>
        </div>
        <div class="tile" id="agent-gemini" onclick="pickAgent('gemini')">
          <span class="icon">✨</span>Gemini
          <div class="sub">Subscription</div>
        </div>
        <div class="tile" id="agent-codex" onclick="pickAgent('codex')">
          <span class="icon">⚡</span>OpenAI
          <div class="sub">Subscription</div>
        </div>
      </div>
      <button class="btn btn-primary" id="agent-next-btn" onclick="agentNext()" disabled>Continue →</button>
    </div>

    <!-- Step 3: Auth (branches by agent) -->
    <div class="step" id="s3">

      <!-- Claude OAuth -->
      <div id="auth-claude" style="display:none">
        <h2>Log in to Claude</h2>
        <p>Click <strong style="color:#e2e8f0">Start Login</strong>. A link will appear — open it in a new tab, sign in to Claude, and you'll see a <strong style="color:#e2e8f0">short verification code</strong>. Come back to <strong style="color:#e2e8f0">this tab</strong> and paste that code below.</p>
        <!-- auth flow demo -->
        <div class="auth-demo" id="auth-demo">
          <div class="auth-demo-frame active" id="demo-f0">
            <div class="auth-demo-step">Step 1</div>
            <div class="auth-demo-label">Open the link and sign in to Claude</div>
            <div class="demo-tab-bar" style="margin-top:.3rem">
              <div class="demo-tab">cc-connect Setup</div>
              <div class="demo-tab active-tab">claude.com <span class="close">✕</span></div>
            </div>
            <div class="demo-popup">claude.com — sign in to continue…</div>
          </div>
          <div class="auth-demo-frame" id="demo-f1">
            <div class="auth-demo-step">Step 2</div>
            <div class="auth-demo-label">Claude shows a code — copy it</div>
            <div class="demo-code">A7F3-K9PQ</div>
            <div class="auth-demo-label" style="color:#4a5568;font-size:.72rem;margin-top:.1rem">then close that tab</div>
          </div>
          <div class="auth-demo-frame" id="demo-f2">
            <div class="auth-demo-step">Step 3</div>
            <div class="auth-demo-label">Come back here — paste the code &amp; submit</div>
            <div style="display:flex;gap:.35rem;margin-top:.3rem;align-items:center">
              <div style="background:#0f1117;border:1px solid #2d3748;border-radius:5px;padding:.25rem .6rem;font-family:monospace;font-size:.85rem;color:#68d391;letter-spacing:.12em">A7F3-K9PQ</div>
              <div class="demo-btn">Submit</div>
            </div>
          </div>
          <div class="demo-progress" id="demo-progress"></div>
        </div>
        <div class="terminal" id="auth-out">Ready. Press Start Login to begin.</div>
        <div class="url-box" id="auth-url-box" style="display:none">
          <strong>Step 1 — Open this link and sign in:</strong><br>
          <a id="auth-url-link" href="#" target="_blank" style="word-break:break-all"></a><br><br>
          <strong>Step 2 — After signing in, Claude shows you a code. Copy it.</strong><br><br>
          <strong>Step 3 — Come back to this tab and paste the code below.</strong>
        </div>
        <div id="auth-code-box" style="display:none">
          <label>Paste the verification code from Claude here:</label>
          <div style="display:flex;gap:.5rem">
            <input type="text" id="auth-code" placeholder="Paste code…" autocomplete="off" style="margin-bottom:0;flex:1" oninput="validateCode(this)" onpaste="setTimeout(()=>validateCode(this),0)">
            <button class="btn btn-primary" id="submit-code-btn" onclick="submitCode()" style="width:auto;padding:.65rem 1rem" disabled>Submit</button>
          </div>
          <div id="code-hint" style="font-size:.8rem;margin-top:.35rem;color:#4a5568"></div>
        </div>
        <button class="btn btn-primary" id="auth-btn" onclick="startAuth()">Start Login</button>
        <button class="btn btn-secondary" id="already-btn" onclick="checkAuth()" style="display:none">I've already logged in → Continue</button>
      </div>

      <!-- Gemini subscription -->
      <div id="auth-gemini" style="display:none">
        <h2>Connect Gemini</h2>
        <p>Use your <strong style="color:#e2e8f0">Google AI Pro or Ultra</strong> subscription — no API credits needed.</p>
        <ol>
          <li>Install Gemini CLI on your computer: <a href="https://github.com/google-gemini/gemini-cli" target="_blank">Setup guide →</a></li>
          <li>Run <code>gemini</code> and sign in with Google</li>
          <li>Run <code>cat ~/.gemini/oauth_creds.json</code> and paste the output below</li>
        </ol>
        <label>Paste credentials JSON</label>
        <textarea id="gemini-creds" rows="5" placeholder='{"client_id": "...", "access_token": "..."}'></textarea>
        <button class="btn btn-primary" onclick="continueWithCreds('gemini')">Continue →</button>
      </div>

      <!-- Codex subscription -->
      <div id="auth-codex" style="display:none">
        <h2>Connect OpenAI Codex</h2>
        <p>Use your <strong style="color:#e2e8f0">ChatGPT Plus, Pro, or higher</strong> subscription — no API credits needed.</p>
        <ol>
          <li>Install Codex CLI on your computer: <a href="https://github.com/openai/codex" target="_blank">Setup guide →</a></li>
          <li>Run <code>codex</code> and sign in with ChatGPT</li>
          <li>Run <code>cat ~/.codex/auth.json</code> and paste the output below</li>
        </ol>
        <label>Paste credentials JSON</label>
        <textarea id="codex-creds" rows="5" placeholder='{"tokens": {...}}'></textarea>
        <button class="btn btn-primary" onclick="continueWithCreds('codex')">Continue →</button>
      </div>

    </div>

    <!-- Step 4: Platform -->
    <div class="step" id="s4">
      <h2>Connect a platform</h2>
      <p>Choose where you want to chat with your assistant.</p>
      <div class="platform-grid">
        <div class="tile" id="plat-telegram" onclick="pickPlat('telegram')"><span class="icon">✈️</span>Telegram</div>
        <div class="tile" id="plat-discord"  onclick="pickPlat('discord')"><span class="icon">🎮</span>Discord</div>
        <div class="tile" id="plat-slack"    onclick="pickPlat('slack')"><span class="icon">💼</span>Slack</div>
      </div>
      <div id="token-form" style="display:none">
        <div id="hint-telegram" style="display:none">
          <ol>
            <li>Open Telegram and message <a href="https://t.me/BotFather" target="_blank"><strong>@BotFather</strong></a></li>
            <li>Send <code>/newbot</code> and follow the prompts to name your bot</li>
            <li>Copy the token BotFather gives you and paste it below</li>
          </ol>
        </div>
        <div id="hint-discord"  style="display:none">
          <ol>
            <li>Open <a href="https://discord.com/developers/applications" target="_blank">discord.com/developers/applications</a> → <strong>New Application</strong></li>
            <li>Go to <strong>Bot</strong> → <strong>Reset Token</strong> → copy the token and paste it below</li>
            <li>On the same Bot page, enable <strong>Message Content Intent</strong></li>
            <li>Go to <strong>OAuth2 → URL Generator</strong> → tick scope <code>bot</code> → tick permission <code>Send Messages</code></li>
            <li>Copy the generated URL, open it in a new tab, and invite the bot to your server</li>
          </ol>
        </div>
        <div id="hint-slack"    style="display:none">
          <ol>
            <li>Open <a href="https://api.slack.com/apps" target="_blank">api.slack.com/apps</a> → <strong>Create New App</strong> → From scratch</li>
            <li>Go to <strong>Socket Mode</strong> → enable it → create an App-Level Token with scope <code>connections:write</code> → copy that token</li>
            <li>Go to <strong>OAuth &amp; Permissions</strong> → add bot scopes: <code>app_mentions:read</code>, <code>chat:write</code>, <code>im:history</code>, <code>channels:history</code></li>
            <li>Click <strong>Install to Workspace</strong> → copy the <em>Bot User OAuth Token</em></li>
            <li>Go to <strong>Event Subscriptions</strong> → enable → subscribe to <code>app_mention</code> and <code>message.im</code></li>
          </ol>
        </div>
        <label id="token-label">Bot Token</label>
        <input type="password" id="token" placeholder="Paste token here…" autocomplete="off">
        <div class="slack-extra" id="slack-extra">
          <label>App Token (xapp-…)</label>
          <input type="password" id="app-token" placeholder="xapp-…" autocomplete="off">
        </div>
        <button class="btn btn-primary" onclick="configure()">Connect &amp; Start →</button>
      </div>
    </div>

    <!-- Step 5: Done -->
    <div class="step" id="s5">
      <div class="success-icon">🎉</div>
      <h2 style="text-align:center">You're live!</h2>
      <p style="text-align:center">cc-connect is running. Send a message to your bot to start chatting.</p>
      <div class="alert-success" id="done-msg"></div>
      <div id="bot-link-box" style="display:none;text-align:center;margin-bottom:1rem">
        <a id="bot-link" href="#" target="_blank" class="btn btn-primary" style="display:inline-block;width:auto;padding:.65rem 1.5rem;text-decoration:none">Open your bot →</a>
      </div>
      <p style="font-size:.8rem;color:#4a5568;margin-top:1rem">
        Logs: <code>sudo journalctl -u cc-connect -f</code><br>
        Restart: <code>sudo systemctl restart cc-connect</code>
      </p>
    </div>

  </div>
</div>
<script>
let step = 1;
let selectedAgent = null;
let apiKey = '';
let plat = null;

function goTo(n) {
  document.getElementById('s' + step).classList.remove('active');
  document.getElementById('tab' + step).classList.remove('active');
  document.getElementById('tab' + step).classList.add('done');
  step = n;
  document.getElementById('s' + n).classList.add('active');
  document.getElementById('tab' + n).classList.add('active');
}

// ── Agent selection ────────────────────────────────────────────────────────────

function pickAgent(a) {
  selectedAgent = a;
  ['claudecode','gemini','codex'].forEach(x =>
    document.getElementById('agent-' + x).classList.toggle('selected', x === a)
  );
  document.getElementById('agent-next-btn').disabled = false;
}

function agentNext() {
  goTo(3);
  ['claude','gemini','codex'].forEach(x =>
    document.getElementById('auth-' + x).style.display = 'none'
  );
  const key = selectedAgent === 'claudecode' ? 'claude' : selectedAgent;
  document.getElementById('auth-' + key).style.display = 'block';
}

// ── Claude OAuth ───────────────────────────────────────────────────────────────

function startAuth() {
  const btn = document.getElementById('auth-btn');
  const out = document.getElementById('auth-out');
  btn.disabled = true;
  btn.innerHTML = '<span class="spinner"></span>Waiting for login…';
  out.textContent = '';
  document.getElementById('auth-url-box').style.display = 'none';
  document.getElementById('auth-code-box').style.display = 'none';
  document.getElementById('auth-code').value = '';
  document.getElementById('code-hint').textContent = '';
  document.getElementById('submit-code-btn').disabled = true;
  document.getElementById('already-btn').style.display = 'none';

  const es = new EventSource('/api/auth/stream');
  es.onmessage = e => {
    const d = JSON.parse(e.data);
    if (d.line) {
      out.textContent += d.line + '\\n';
      out.scrollTop = out.scrollHeight;
      const m = d.line.match(/https:\\/\\/[^\\s]+/);
      if (m) {
        const box = document.getElementById('auth-url-box');
        const link = document.getElementById('auth-url-link');
        box.style.display = 'block';
        link.href = m[0];
        link.textContent = m[0];
      }
      if (/paste code/i.test(d.line)) {
        document.getElementById('auth-code-box').style.display = 'block';
      }
    }
    if (d.done) {
      es.close();
      if (d.success) {
        out.textContent += '\\n✓ Logged in!';
        setTimeout(() => goTo(4), 800);
      } else {
        btn.disabled = false;
        btn.textContent = 'Retry Login';
        const isExpired = out.textContent.includes('status code 400');
        if (isExpired) {
          out.textContent += '\\n\\n⚠️ The code expired or was already used.\\nClick Retry Login to get a fresh link and a new code.';
        }
        document.getElementById('already-btn').style.display = 'block';
      }
    }
  };
  es.onerror = () => {
    es.close();
    btn.disabled = false;
    btn.textContent = 'Retry Login';
    document.getElementById('already-btn').style.display = 'block';
  };
}

function validateCode(input) {
  const val = input.value.replace(/\\s/g, '');
  input.value = val;
  const hint = document.getElementById('code-hint');
  const btn = document.getElementById('submit-code-btn');
  if (val.length === 0) {
    hint.textContent = '';
    hint.style.color = '#4a5568';
    btn.disabled = true;
  } else if (val.length < 20) {
    hint.textContent = '\\u26a0\\ufe0f Code looks too short (' + val.length + ' chars) — make sure you copied the full code.';
    hint.style.color = '#f6ad55';
    btn.disabled = true;
  } else {
    hint.textContent = val.length + ' characters ✓';
    hint.style.color = '#68d391';
    btn.disabled = false;
  }
}

function submitCode() {
  const code = document.getElementById('auth-code').value.trim();
  if (!code) return;
  fetch('/api/auth/input', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code }),
  });
  document.getElementById('auth-code-box').style.display = 'none';
  document.getElementById('submit-code-btn').disabled = true;
  document.getElementById('auth-out').textContent += '\\n[code submitted, waiting…]\\n';
}

function checkAuth() {
  fetch('/api/auth/status').then(r => r.json()).then(d => {
    if (d.authenticated) goTo(4);
    else alert('Not logged in yet — complete the browser login first.');
  });
}

// ── Credentials JSON (Gemini / Codex subscription) ────────────────────────────

function continueWithCreds(agent) {
  const val = document.getElementById(agent + '-creds').value.trim();
  if (!val) return alert('Paste your credentials JSON first');
  try { JSON.parse(val); } catch { return alert('Invalid JSON — make sure you copied the full output'); }
  apiKey = val;  // reuse apiKey slot to carry creds through to configure()
  goTo(4);
}

// ── Platform ───────────────────────────────────────────────────────────────────

function pickPlat(p) {
  plat = p;
  ['telegram','discord','slack'].forEach(x => {
    document.getElementById('plat-' + x).classList.toggle('selected', x === p);
    document.getElementById('hint-' + x).style.display = x === p ? 'block' : 'none';
  });
  document.getElementById('token-form').style.display = 'block';
  document.getElementById('token-label').textContent = p === 'slack' ? 'Bot Token (xoxb-…)' : 'Bot Token';
  document.getElementById('slack-extra').classList.toggle('show', p === 'slack');
}

function configure() {
  if (!plat) return alert('Choose a platform first');
  const token = document.getElementById('token').value.trim();
  if (!token) return alert('Paste your bot token');
  const body = { agent: selectedAgent, platform: plat, token };
  if (selectedAgent !== 'claudecode') body.api_key = apiKey;
  if (plat === 'slack') {
    body.app_token = document.getElementById('app-token').value.trim();
    if (!body.app_token) return alert('App token required for Slack');
  }
  fetch('/api/configure', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }).then(r => r.json()).then(d => {
    if (d.ok) {
      const agentNames = { claudecode: 'Claude', gemini: 'Gemini', codex: 'OpenAI Codex' };
      document.getElementById('done-msg').textContent =
        'Agent: ' + (agentNames[selectedAgent] || selectedAgent) +
        ' — Platform: ' + plat.charAt(0).toUpperCase() + plat.slice(1) + ' ✓';
      if (d.botUsername) {
        const linkBox = document.getElementById('bot-link-box');
        const link = document.getElementById('bot-link');
        link.href = 'https://t.me/' + d.botUsername;
        link.textContent = 'Open @' + d.botUsername + ' on Telegram →';
        linkBox.style.display = 'block';
      }
      goTo(5);
    } else {
      alert('Error: ' + (d.error || 'unknown'));
    }
  });
}

// ── auth demo animation ────────────────────────────────────────────────────────
(function() {
  const FRAMES = 3;
  const HOLD = 4000;
  const CYCLE = FRAMES * HOLD;
  let demoFrame = 0;
  let demoStart = null;

  function tick(ts) {
    if (!demoStart) demoStart = ts;
    const elapsed = (ts - demoStart) % CYCLE;
    const target = Math.floor(elapsed / HOLD);
    if (target !== demoFrame) {
      const prev = document.getElementById('demo-f' + demoFrame);
      if (prev) prev.classList.remove('active');
      demoFrame = target;
      const next = document.getElementById('demo-f' + demoFrame);
      if (next) next.classList.add('active');
    }
    const bar = document.getElementById('demo-progress');
    if (bar) bar.style.width = ((elapsed % HOLD) / HOLD * 100) + '%';
    requestAnimationFrame(tick);
  }

  document.addEventListener('DOMContentLoaded', function() {
    if (document.getElementById('auth-demo')) requestAnimationFrame(tick);
  });
})();
</script>
</body>
</html>`;

let authProc = null;

// ── HTTP server ────────────────────────────────────────────────────────────────
function parseBody(req) {
    return new Promise((resolve, reject) => {
        let body = '';
        req.on('data', c => body += c);
        req.on('end', () => {
            try { resolve(JSON.parse(body || '{}')); }
            catch { reject(new Error('Invalid JSON')); }
        });
    });
}

const server = http.createServer(async (req, res) => {
    const { method, url } = req;

    if (method === 'GET' && url === '/') {
        res.writeHead(200, { 'Content-Type': 'text/html' });
        return res.end(HTML);
    }

    if (method === 'GET' && url === '/api/auth/status') {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        return res.end(JSON.stringify({ authenticated: isClaudeAuthenticated() }));
    }

    if (method === 'GET' && url === '/api/auth/stream') {
        res.writeHead(200, {
            'Content-Type': 'text/event-stream',
            'Cache-Control': 'no-cache',
            'Connection': 'keep-alive',
        });

        const send = obj => res.write(`data: ${JSON.stringify(obj)}\n\n`);
        const claude = findBin('claude');

        if (!claude) {
            send({ line: 'ERROR: claude binary not found. Re-run install.sh.', done: true, success: false });
            return res.end();
        }

        if (authProc) { try { authProc.kill(); } catch {} }
        // Run as service user so credentials land in their home, not root's.
        const proc = spawn('sudo', ['-u', SERVICE_USER, claude, 'auth', 'login', '--claudeai'], {
            env: { ...process.env, TERM: 'dumb', HOME: SERVICE_HOME },
        });
        authProc = proc;
        proc.stdout.on('data', d => d.toString().split('\n').filter(Boolean).forEach(l => send({ line: l })));
        proc.stderr.on('data', d => d.toString().split('\n').filter(Boolean).forEach(l => send({ line: l })));
        proc.on('close', () => {
            authProc = null;
            send({ done: true, success: isClaudeAuthenticated() });
            res.end();
        });
        req.on('close', () => { if (proc === authProc) proc.kill(); });
        return;
    }

    if (method === 'GET' && url.startsWith('/oauth/code/callback')) {
        const params = new URLSearchParams(url.split('?')[1] || '');
        const code = params.get('code');
        if (code && authProc) {
            authProc.stdin.write(code + '\n');
        }
        res.writeHead(200, { 'Content-Type': 'text/html' });
        return res.end(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>${code ? 'Authorized' : 'Error'} — cc-connect</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0f1117;color:#e2e8f0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:1rem}
.card{background:#1a1f2e;border:1px solid #2d3748;border-radius:12px;padding:2.5rem 2rem;max-width:400px;width:100%;text-align:center}
h2{font-size:1.2rem;font-weight:700;margin-bottom:.75rem}
p{color:#a0aec0;font-size:.9rem;line-height:1.6}
.icon{font-size:2.5rem;margin-bottom:1rem}
</style>
</head>
<body>
<div class="card">
  <div class="icon">${code ? '✅' : '⚠️'}</div>
  <h2>${code ? 'Authorized!' : 'No code received'}</h2>
  <p>${code ? 'Code submitted automatically. You can close this tab and return to the setup wizard.' : 'No authorization code found. Please go back and try again.'}</p>
</div>
${code ? '<script>setTimeout(()=>window.close(),2500)</script>' : ''}
</body>
</html>`);
    }

    if (method === 'POST' && url === '/api/auth/input') {
        try {
            const body = await parseBody(req);
            if (authProc && body.code) {
                authProc.stdin.write(body.code + '\n');
            }
            res.writeHead(200, { 'Content-Type': 'application/json' });
            res.end(JSON.stringify({ ok: true }));
        } catch {
            res.writeHead(400); res.end('{}');
        }
        return;
    }

    if (method === 'POST' && url === '/api/configure') {
        try {
            const body = await parseBody(req);
            const { agent, platform, token, app_token, api_key } = body;
            if (!platform || !token) throw new Error('Missing platform or token');

            const validAgents = ['claudecode', 'gemini', 'codex'];
            const resolvedAgent = validAgents.includes(agent) ? agent : 'claudecode';

            if (resolvedAgent !== 'claudecode' && !api_key) {
                throw new Error('API key required for ' + resolvedAgent);
            }

            const tokens = { token };
            if (platform === 'slack') {
                if (!app_token) throw new Error('Missing app_token for Slack');
                tokens.bot_token = token;
                tokens.app_token = app_token;
                delete tokens.token;
            }

            if ((resolvedAgent === 'gemini' || resolvedAgent === 'codex') && api_key) {
                // api_key carries the credentials JSON for subscription-based auth
                try { JSON.parse(api_key); writeCredsFile(resolvedAgent, api_key); } catch {}
                writeAgentEnv(resolvedAgent, '');
            } else {
                writeAgentEnv(resolvedAgent, api_key || '');
            }
            writeConfig(resolvedAgent, platform, tokens);
            // Ensure the service user owns all config files (wizard may run as root).
            try { execSync(`chown -R ${SERVICE_USER}:${SERVICE_USER} ${CONFIG_DIR}`, { stdio: 'pipe' }); } catch {}

            try { execSync('sudo systemctl restart cc-connect', { stdio: 'pipe' }); }
            catch { execSync('sudo systemctl start cc-connect', { stdio: 'pipe' }); }

            // Fetch bot info for Telegram so we can show a deep link.
            let botUsername = null;
            if (platform === 'telegram') {
                try {
                    const tgRes = await new Promise((resolve, reject) => {
                        const https = require('https');
                        https.get(`https://api.telegram.org/bot${token}/getMe`, r => {
                            let data = '';
                            r.on('data', c => data += c);
                            r.on('end', () => resolve(JSON.parse(data)));
                        }).on('error', reject);
                    });
                    if (tgRes.ok) botUsername = tgRes.result.username;
                } catch {}
            }

            setTimeout(() => process.exit(0), 2000);

            res.writeHead(200, { 'Content-Type': 'application/json' });
            res.end(JSON.stringify({ ok: true, botUsername }));
        } catch (e) {
            res.writeHead(400, { 'Content-Type': 'application/json' });
            res.end(JSON.stringify({ ok: false, error: e.message }));
        }
        return;
    }

    res.writeHead(404);
    res.end('Not found');
});

server.listen(PORT, '0.0.0.0', () => {
    const ip = Object.values(os.networkInterfaces())
        .flat()
        .find(i => i && i.family === 'IPv4' && !i.internal)?.address || 'localhost';
    console.log(`\n  cc-connect setup wizard`);
    console.log(`  Open: http://${ip}:${PORT}\n`);
});
