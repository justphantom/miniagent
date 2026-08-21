// WebUI frontend: login overlay (x-api-key), session list, turn composer, NDJSON event stream.
// Vanilla JS, no build step; the NDJSON contract is identical to the CLI stdout stream.
"use strict";
const $ = (id) => document.getElementById(id);
const KEY = "miniagent.web.key";
let key = localStorage.getItem(KEY) || "";
let session = "";       // current session id ("" = next send creates one)
let sending = false;

function authHeaders() { return { "x-api-key": key }; }

async function api(path, opts = {}) {
  const r = await fetch(path, { ...opts, headers: { ...authHeaders(), ...(opts.headers || {}) } });
  if (r.status === 401) throw new Error("unauthorized");
  return r;
}

function showLogin(err = "") {
  $("app").classList.add("hidden");
  $("login").classList.remove("hidden");
  $("login-err").textContent = err;
  $("key").focus();
}
function showApp() {
  $("login").classList.add("hidden");
  $("app").classList.remove("hidden");
  loadModels();
  loadSessions();
}

$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  key = $("key").value.trim();
  $("login-err").textContent = "";
  try {
    const r = await fetch("/api/whoami");
    if (r.ok && key) {
      const probe = await fetch("/api/sessions", { headers: authHeaders() });
      if (probe.status === 401) throw new Error("密钥不正确");
    }
    localStorage.setItem(KEY, key);
    showApp();
  } catch {
    $("login-err").textContent = "连接失败，请重试";
  }
});

$("logout").addEventListener("click", () => { localStorage.removeItem(KEY); location.reload(); });
$("menu-btn").addEventListener("click", () => document.body.classList.toggle("nav-open"));
$("new-chat").addEventListener("click", () => {
  session = "";
  $("events").innerHTML = "";
  document.title = "miniagent";
  showSessionID("");
});

// ---- events rendering ----
// Markdown subset: #/##/###, ``` code, -/* list, > quote, ---, **bold**, *italic*, `code`, ~~del~~.
// HTML is escaped first (XSS guard); block assembly runs after inline so code spans are untouched.
function mdEsc(s) { return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;"); }
function mdInline(s) {
  return s
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/\*([^*]+)\*/g, "<em>$1</em>")
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/~~([^~]+)~~/g, "<del>$1</del>");
}
function mdRender(text) {
  const lines = mdEsc(text).split("\n");
  const out = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (line.trim() === "") { i++; continue; }
    if (line.startsWith("```")) {
      let code = "";
      i++;
      while (i < lines.length && !lines[i].startsWith("```")) { code += lines[i] + "\n"; i++; }
      i++;
      out.push(`<pre class="md-code"><code>${code.trimEnd()}</code></pre>`);
      continue;
    }
    if (line.startsWith("### ")) { out.push(`<h4>${mdInline(line.slice(4))}</h4>`); i++; continue; }
    if (line.startsWith("## ")) { out.push(`<h3>${mdInline(line.slice(3))}</h3>`); i++; continue; }
    if (line.startsWith("# ")) { out.push(`<h2>${mdInline(line.slice(2))}</h2>`); i++; continue; }
    if (line.trim() === "---") { out.push("<hr>"); i++; continue; }
    if (line.startsWith("> ")) {
      let q = "";
      while (i < lines.length && lines[i].startsWith("> ")) { q += lines[i].slice(2) + "\n"; i++; }
      out.push(`<blockquote>${q.trim().split("\n").map(mdInline).join("<br>")}</blockquote>`);
      continue;
    }
    if (line.startsWith("- ") || line.startsWith("* ")) {
      let items = "";
      while (i < lines.length && (lines[i].startsWith("- ") || lines[i].startsWith("* "))) {
        items += `<li>${mdInline(lines[i].slice(2))}</li>`;
        i++;
      }
      out.push(`<ul>${items}</ul>`);
      continue;
    }
    out.push(`<p>${mdInline(line)}</p>`);
    i++;
  }
  return out.join("");
}

function fmtTime(ms) { if (!ms) return ""; const d = new Date(ms), p = (n) => String(n).padStart(2, "0"); return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`; }
function evDiv(cls, tag, ts) {
  const d = document.createElement("div");
  d.className = "ev " + cls;
  if (tag) {
    const t = document.createElement("div");
    t.className = "tag";
    const label = document.createElement("span");
    label.textContent = tag;
    t.appendChild(label);
    const time = document.createElement("span");
    time.className = "time";
    time.textContent = fmtTime(ts);
    t.appendChild(time);
    d.appendChild(t);
  }
  return d;
}
function appendUserPrompt(text) {
  const d = evDiv("user", "user", Date.now());
  d.appendChild(document.createTextNode(text));
  $("events").appendChild(d);
}
let curText = null, curReasoning = null;
function appendDelta(kind, text, ts) {
  let d = kind === "text" ? curText : curReasoning;
  if (!d) {
    d = evDiv(kind === "text" ? "text" : "reasoning", kind === "text" ? "assistant" : "assistant · thinking", ts);
    if (kind === "reasoning") d.style.opacity = "0.75";
    $("events").appendChild(d);
    const span = document.createElement("span");
    d.appendChild(span);
    d._span = span;
    d._md = "";
    if (kind === "text") curText = d; else curReasoning = d;
  }
  d._md += text;
  d._span.textContent += text;
}
function appendToolUse(ev) {
  const d = document.createElement("details");
  d.className = "ev tool";
  const s = document.createElement("summary");
  const name = document.createElement("span");
  name.textContent = `🔧 ${ev.name}`;
  const time = document.createElement("span");
  time.className = "time";
  time.textContent = fmtTime(ev.ts);
  s.appendChild(name); s.appendChild(time);
  d.appendChild(s);
  const pre = document.createElement("pre");
  pre.textContent = ev.input || "";
  d.appendChild(pre);
  d._pre = pre;
  $("events").appendChild(d);
  curText = curReasoning = null;
  return d;
}
function appendToolResult(ev, toolNode) {
  const target = toolNode || appendToolUse({ name: ev.name || "tool", input: "" });
  const pre = target._pre;
  pre.textContent += (pre.textContent ? "\n" : "") + `→ ${ev.output || ""}${ev.is_error ? "  [error]" : ""}`;
}
// finishText flushes the accumulated markdown (stream is done or interrupted by a tool event).
function finishText() {
  for (const d of [curText, curReasoning]) {
    if (d && d._md) {
      const md = document.createElement("div");
      md.className = "md";
      md.innerHTML = mdRender(d._md);
      d.replaceChild(md, d._span);
      d._md = "";
    }
  }
  curText = curReasoning = null;
}

function showSessionID(id) { const el = $("session-id"); el.textContent = id ? `会话 ${id}` : ""; el.title = id || "当前会话 ID"; }
function renderEvent(ev) {
  switch (ev.type) {
    case "session":
      session = ev.id;
      document.title = `miniagent · ${ev.id}`;
      showSessionID(ev.id);
      break;
    case "text_delta": appendDelta("text", ev.text, ev.ts); break;
    case "reasoning_delta": appendDelta("reasoning", ev.text, ev.ts); break;
    case "tool_use": appendToolUse(ev); break;
    case "tool_result": appendToolResult(ev, null); break;
    case "result": {
      finishText();
      const d = evDiv("result", "result", ev.ts);
      const md = document.createElement("div");
      md.className = "md";
      md.innerHTML = mdRender(ev.text || "(no text)");
      d.appendChild(md);
      const u = document.createElement("div");
      u.className = "usage";
      u.textContent = `steps=${ev.steps} in=${ev.input_tokens} out=${ev.output_tokens}${ev.compacted ? " compacted" : ""}`;
      d.appendChild(u);
      $("events").appendChild(d);
      break;
    }
    case "error": {
      finishText();
      const d = evDiv("error", "error", ev.ts || Date.now());
      d.appendChild(document.createTextNode(ev.error || ev.message || "unknown error"));
      $("events").appendChild(d);
      break;
    }
  }
  const box = $("events");
  box.scrollTop = box.scrollHeight;
}

// ---- send a turn (streams NDJSON) ----
async function sendTurn() {
  if (sending) return;
  const prompt = $("prompt").value.trim();
  const workdir = $("workdir").value.trim();
  if (!prompt) { $("prompt").focus(); return; }
  if (!workdir) { $("workdir").focus(); return; }
  const sel = $("model-sel");
  const opt = sel.options[sel.selectedIndex];
  const body = {
    prompt,
    workdir,
    session,
    provider: opt ? opt.dataset.provider || "" : "",
    model: opt ? opt.dataset.model || "" : "",
    thinking: $("thinking-sel").value,
  };
  sending = true;
  $("send").disabled = true;
  appendUserPrompt(prompt);
  $("prompt").value = "";
  try {
    const r = await fetch("/api/turn", { method: "POST", headers: { ...authHeaders(), "Content-Type": "application/json" }, body: JSON.stringify(body) });
    if (r.status === 401) { showLogin("密钥已失效"); return; }
    if (!r.ok && !r.headers.get("Content-Type")?.includes("ndjson")) {
      const j = await r.json().catch(() => ({}));
      renderEvent({ type: "error", error: j.error || `HTTP ${r.status}` });
      return;
    }
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line) continue;
        try { renderEvent(JSON.parse(line)); } catch { /* tolerate half-line */ }
      }
    }
  } catch (e) {
    renderEvent({ type: "error", error: String(e) });
  } finally {
    sending = false;
    $("send").disabled = false;
    loadSessions();
  }
}
$("send").addEventListener("click", sendTurn);
$("prompt").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) sendTurn();
});

// ---- models / sessions ----
async function loadModels() {
  try {
    const r = await api("/api/models");
    if (!r.ok) return;
    const models = await r.json();
    const sel = $("model-sel");
    sel.innerHTML = "<option value=\"\">模型: 默认</option>";
    for (const m of models) {
      const o = document.createElement("option");
      o.value = `${m.provider}/${m.model}`;
      // provider/model ride in dataset, not value: model ids may contain "/" and a value
      // split("/") mis-slices them (same ambiguity ModelRef exists to avoid).
      o.dataset.provider = m.provider;
      o.dataset.model = m.model;
      o.textContent = `${m.provider}/${m.model}`;
      sel.appendChild(o);
    }
  } catch { /* dropdown stays default */ }
}
let sessionMeta = {}; // id → { workdir, model, ... } captured from the session list

async function loadSessions() {
  try {
    const r = await api("/api/sessions");
    if (!r.ok) return;
    const list = await r.json();
    const box = $("session-list");
    box.innerHTML = "";
    for (const s of list) {
      sessionMeta[s.id] = s;
      const b = document.createElement("button");
      b.className = "sess-item" + (s.id === session ? " active" : "");
      b.type = "button";
      const top = document.createElement("div");
      top.textContent = s.model || s.id;
      const sid = document.createElement("div");
      sid.className = "sid";
      sid.textContent = [s.id, s.workdir || "", s.created ? new Date(s.created).toLocaleString() : ""].filter(Boolean).join(" · ");
      b.appendChild(top); b.appendChild(sid);
      b.addEventListener("click", () => openSession(s.id));
      const del = document.createElement("span");
      del.className = "sess-del";
      del.textContent = "✕";
      del.title = "删除会话";
      del.addEventListener("click", (e) => { e.stopPropagation(); deleteSession(s.id); });
      b.appendChild(del);
      box.appendChild(b);
    }
  } catch { /* keep old list */ }
}

async function deleteSession(id) {
  if (!confirm(`删除会话 ${id}？此操作不可恢复。`)) return;
  try {
    const r = await api(`/api/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (r.status === 401) { showLogin("密钥已失效"); return; }
    if (r.status === 409) { alert("该会话有轮次正在进行，无法删除"); return; }
    if (!r.ok && r.status !== 404) { alert(`删除失败: HTTP ${r.status}`); return; }
    delete sessionMeta[id];
    if (session === id) {
      session = "";
      $("events").innerHTML = "";
      document.title = "miniagent";
      showSessionID("");
    }
    loadSessions();
  } catch { alert("删除失败：网络错误"); }
}

async function openSession(id) {
  session = id;
  document.title = `miniagent · ${id}`;
  document.body.classList.remove("nav-open");
  $("events").innerHTML = "";
  curText = curReasoning = null;
  const meta = sessionMeta[id];
  if (meta && meta.workdir) $("workdir").value = meta.workdir;
  showSessionID(id);
  try {
    const r = await api(`/api/sessions/${encodeURIComponent(id)}`);
    if (r.status === 401) { showLogin("密钥已失效"); return; }
    if (!r.ok) return;
    const text = await r.text();
    for (const line of text.split("\n")) {
      if (!line.trim()) continue;
      try { renderEvent(JSON.parse(line)); } catch { /* half-line */ }
    }
  } catch { /* ignore */ }
}

// ---- boot: probe auth requirement ----
(async () => {
  try {
    const r = await fetch("/api/whoami");
    const j = await r.json();
    if (!j.auth_required) { showApp(); return; }
    if (key) {
      const probe = await fetch("/api/sessions", { headers: authHeaders() });
      if (probe.ok) { showApp(); return; }
    }
    showLogin();
  } catch {
    showLogin("无法连接服务器");
  }
})();
