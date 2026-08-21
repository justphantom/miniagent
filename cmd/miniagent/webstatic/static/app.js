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
});

// ---- events rendering ----
function evDiv(cls, tag) {
  const d = document.createElement("div");
  d.className = "ev " + cls;
  if (tag) { const t = document.createElement("div"); t.className = "tag"; t.textContent = tag; d.appendChild(t); }
  return d;
}
function appendUserPrompt(text) {
  const d = evDiv("user", "user");
  d.appendChild(document.createTextNode(text));
  $("events").appendChild(d);
}
let curText = null, curReasoning = null;
function appendDelta(kind, text) {
  let d = kind === "text" ? curText : curReasoning;
  if (!d) {
    d = evDiv(kind === "text" ? "text" : "reasoning", kind === "text" ? "assistant" : "assistant · thinking");
    if (kind === "reasoning") d.style.opacity = "0.75";
    $("events").appendChild(d);
    const span = document.createElement("span");
    d.appendChild(span);
    d._span = span;
    if (kind === "text") curText = d; else curReasoning = d;
  }
  d._span.textContent += text;
}
function appendToolUse(ev) {
  const d = document.createElement("details");
  d.className = "ev tool";
  const s = document.createElement("summary");
  s.textContent = `🔧 ${ev.name}`;
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
function finishText() { curText = curReasoning = null; }

function renderEvent(ev) {
  switch (ev.type) {
    case "session":
      session = ev.id;
      document.title = `miniagent · ${ev.id}`;
      break;
    case "text_delta": appendDelta("text", ev.text); break;
    case "reasoning_delta": appendDelta("reasoning", ev.text); break;
    case "tool_use": appendToolUse(ev); break;
    case "tool_result": appendToolResult(ev, null); break;
    case "result": {
      finishText();
      const d = evDiv("result", "result");
      d.appendChild(document.createTextNode(ev.text || "(no text)"));
      const u = document.createElement("div");
      u.className = "usage";
      u.textContent = `steps=${ev.steps} in=${ev.input_tokens} out=${ev.output_tokens}${ev.compacted ? " compacted" : ""}`;
      d.appendChild(u);
      $("events").appendChild(d);
      break;
    }
    case "error": {
      finishText();
      const d = evDiv("error", "error");
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

async function loadSessions() {
  try {
    const r = await api("/api/sessions");
    if (!r.ok) return;
    const list = await r.json();
    const box = $("session-list");
    box.innerHTML = "";
    for (const s of list) {
      const b = document.createElement("button");
      b.className = "sess-item" + (s.id === session ? " active" : "");
      b.type = "button";
      const top = document.createElement("div");
      top.textContent = s.model || s.id;
      const sid = document.createElement("div");
      sid.className = "sid";
      sid.textContent = `${s.id} · ${s.created || ""}`;
      b.appendChild(top); b.appendChild(sid);
      b.addEventListener("click", () => openSession(s.id));
      box.appendChild(b);
    }
  } catch { /* keep old list */ }
}

async function openSession(id) {
  session = id;
  document.title = `miniagent · ${id}`;
  document.body.classList.remove("nav-open");
  $("events").innerHTML = "";
  curText = curReasoning = null;
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
