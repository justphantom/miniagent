"use strict";

// config.js — WebUI 配置管理页面。
// 全字段表单：渲染 config 所有字段，提交后写回文件。
// 字段定义在 CONFIG_SCHEMA 中，按 sections 分组。

import { api, authHeaders } from "./store.js";
import { eventsViewport, jumpToBottom } from "./views.js";

const $ = (id) => document.getElementById(id);

// ── 字段 schema ──────────────────────────────────────────

// 每个 section 是一个可折叠组，fields 是字段定义。
// field 格式：{key, label, type, help?, placeholder?, default?, provider?}
// key 是 config 对象中的路径（点分隔）。type: text|number|bool|dselect|array|kv|duration
const CONFIG_SECTIONS = [
  { group: "defaults", label: "默认参数", fields: [
    { key: "defaults.provider", label: "默认提供商", type: "text" },
    { key: "defaults.model", label: "默认模型", type: "text" },
    { key: "defaults.thinking", label: "默认思考级别", type: "text", help: "off / low / medium / high; 按 provider.thinking.map 声明" },
    { key: "defaults.system_prompt", label: "系统提示词", type: "text", help: "覆盖内置默认值" },
    { key: "defaults.rules_file", label: "规则文件", type: "text", help: "工作目录下的文件名（如 AGENTS.md），自动追加到系统提示词" },
    { key: "defaults.summary_request", label: "摘要请求提示", type: "text" },
    { key: "defaults.summarizer_prompt", label: "摘要器提示", type: "text" },
    { key: "defaults.subagent_guidance", label: "子代理引导", type: "text" },
    { key: "defaults.summary_create_instruction", label: "摘要创建指令", type: "text" },
    { key: "defaults.summary_update_instruction", label: "摘要更新指令", type: "text" },
    { key: "defaults.summary_template", label: "摘要模板", type: "text" },
  ]},
  { group: "run", label: "运行参数", fields: [
    { key: "run.max_tokens", label: "最大输出 Token", type: "number" },
    { key: "run.max_iterations", label: "最大迭代次数", type: "number" },
    { key: "run.max_tokens_total", label: "总 Token 预算（0=不限）", type: "number" },
    { key: "run.context_window", label: "上下文窗口", type: "number" },
    { key: "run.max_duration", label: "最大运行时长", type: "duration", help: "如 30m / 2h" },
    { key: "run.shell_timeout", label: "Shell 超时", type: "duration" },
    { key: "run.file_op_timeout", label: "文件操作超时", type: "duration" },
    { key: "run.write_timeout", label: "写入超时", type: "duration" },
    { key: "run.http_timeout", label: "HTTP 超时", type: "duration" },
    { key: "run.web_timeout", label: "网页抓取超时", type: "duration" },
    { key: "run.stream", label: "流式输出", type: "bool" },
    { key: "run.confirm_destructive", label: "确认破坏性操作", type: "bool", help: "开启后写/编辑/危险 shell 需确认" },
    { key: "run.context_use_real_usage", label: "使用真实用量", type: "bool", help: "为空则启用" },
    { key: "run.max_tool_result_chars", label: "工具结果最大字符", type: "number" },
    { key: "run.max_file_result_chars", label: "文件结果最大字符", type: "number" },
    { key: "run.max_parallel_tools", label: "最大并行工具数", type: "number" },
    { key: "run.context_keep_recent", label: "保留最近轮数", type: "number" },
    { key: "run.summary_max_chars", label: "摘要最大字符", type: "number" },
    { key: "run.max_read_file_bytes", label: "最大读取文件字节", type: "number" },
    { key: "run.max_shell_output_chars", label: "Shell 输出最大字符", type: "number" },
    { key: "run.shell_stream_window_bytes", label: "Shell 流窗口字节", type: "number" },
    { key: "run.max_session_bytes", label: "会话最大字节", type: "number" },
    { key: "run.summary_max_tokens", label: "摘要最大 Token", type: "number" },
    { key: "run.grep_max_matches", label: "Grep 最大匹配数", type: "number" },
    { key: "run.context_trim_tool_chars", label: "上下文修剪工具字符", type: "number" },
    { key: "run.context_keep_reasoning", label: "保留推理轮数", type: "number" },
    { key: "run.context_keep_tool_args", label: "保留工具参数轮数", type: "number" },
    { key: "run.context_keep_reasoning_chars", label: "保留推理字符数", type: "number" },
    { key: "run.preserve_recent_tokens", label: "保留最近 Token 预算", type: "number" },
    { key: "run.tool_output_dir", label: "工具输出目录", type: "text" },
    { key: "run.tool_output_retention", label: "工具输出保留时长", type: "duration", help: "如 168h" },
  ]},
  { group: "compaction", label: "摘要压缩", fields: [
    { key: "compaction.provider", label: "提供商", type: "text", help: "为空则使用默认提供商" },
    { key: "compaction.model", label: "模型", type: "text" },
    { key: "compaction.auto", label: "自动压缩", type: "bool", help: "检测用量溢出时自动触发" },
    { key: "compaction.reserved", label: "保留 Token", type: "number" },
  ]},
  { group: "web", label: "Web 服务", fields: [
    { key: "web.listen", label: "监听地址", type: "text", default: "127.0.0.1:8787" },
    { key: "web.key", label: "API 密钥", type: "text", help: "留空=无认证(仅限回环地址)" },
    { key: "web.max_concurrent_turns", label: "最大并发轮次", type: "number", default: "0", help: "0=不限" },
    { key: "web.allowed_hosts", label: "允许 Host 白名单", type: "array", help: "每行一个，反代域名/外部 IP" },
  ]},
  { group: "session", label: "会话", fields: [
    { key: "session.dir", label: "会话目录", type: "text" },
  ]},
];

// ── 工具函数 ──────────────────────────────────────────

// getNested 按点分隔路径取配置值
function getNested(obj, path) {
  return path.split(".").reduce((o, k) => (o != null ? o[k] : undefined), obj);
}

// setNested 按点分隔路径设值（直接修改传入对象）
function setNested(obj, path, val) {
  const parts = path.split(".");
  let cur = obj;
  for (let i = 0; i < parts.length - 1; i++) {
    if (cur[parts[i]] == null) cur[parts[i]] = {};
    cur = cur[parts[i]];
  }
  cur[parts[parts.length - 1]] = val;
}

// renderField 根据 field 类型渲染输入控件，读取当前值并绑定变更事件
function renderField(field, currentVal, onChange) {
  const wrap = document.createElement("div");
  wrap.className = "cfg-field";
  const label = document.createElement("label");
  label.className = "cfg-label";
  label.textContent = field.label;
  label.title = field.help || "";
  wrap.appendChild(label);

  const help = field.help ? Object.assign(document.createElement("span"), { className: "cfg-help muted", textContent: field.help }) : null;
  if (help) wrap.appendChild(help);

  const input = document.createElement("div");
  input.className = "cfg-input";

  const val = currentVal !== undefined && currentVal !== null ? currentVal : field.default ?? "";

  switch (field.type) {
    case "bool": {
      const cb = document.createElement("input");
      cb.type = "checkbox";
      cb.checked = val === true;
      // 三态：null=default, true=on, false=off
      cb.indeterminate = currentVal === null || currentVal === undefined;
      cb.addEventListener("change", () => {
        // 点击勾选从 default→true 或 false→true；再点从 true→false；再点从 false→null（indeterminate）
        if (cb.indeterminate) { cb.checked = true; cb.indeterminate = false; onChange(true); return; }
        if (!cb.checked) { cb.checked = false; cb.indeterminate = true; onChange(null); return; }
        onChange(cb.checked);
      });
      input.appendChild(cb);
      break;
    }
    case "number": {
      const inp = document.createElement("input");
      inp.type = "number";
      inp.value = val !== "" ? val : "";
      inp.placeholder = field.default || "";
      inp.addEventListener("input", () => {
        const v = inp.value.trim();
        onChange(v === "" ? null : Number(v));
      });
      input.appendChild(inp);
      break;
    }
    case "duration":
    case "text": {
      const inp = document.createElement("input");
      inp.type = field.type === "duration" ? "text" : "text";
      inp.value = val !== "" ? val : "";
      inp.placeholder = field.default || "";
      if (field.type === "duration") inp.placeholder = "如 30s / 5m / 2h";
      inp.addEventListener("input", () => {
        const v = inp.value.trim();
        onChange(v || null);
      });
      input.appendChild(inp);
      break;
    }
    case "array": {
      const arr = Array.isArray(val) ? [...val] : [];
      const list = document.createElement("div");
      list.className = "cfg-array";
      const addRow = () => {
        const row = document.createElement("div");
        row.className = "cfg-array-row";
        const inp = document.createElement("input");
        inp.type = "text";
        inp.addEventListener("input", () => { arr[row._idx] = inp.value.trim() || null; onChange(arr.filter(Boolean)); });
        const del = document.createElement("button");
        del.type = "button";
        del.className = "ghost cfg-array-del";
        del.textContent = "✕";
        del.addEventListener("click", () => { row.remove(); arr.splice(row._idx, 1); onChange(arr.filter(Boolean)); });
        row.appendChild(inp);
        row.appendChild(del);
        list.appendChild(row);
        row._idx = arr.length - 1;
        inp.value = arr[row._idx] || "";
      };
      arr.forEach((v, i) => {
        const row = document.createElement("div");
        row.className = "cfg-array-row";
        row._idx = i;
        const inp = document.createElement("input");
        inp.type = "text";
        inp.value = v || "";
        inp.addEventListener("input", () => { arr[i] = inp.value.trim() || null; onChange(arr.filter(Boolean)); });
        const del = document.createElement("button");
        del.type = "button"; del.className = "ghost cfg-array-del"; del.textContent = "✕";
        del.addEventListener("click", () => { row.remove(); arr.splice(i, 1); arr.forEach((_, j) => { const rows = list.children; if (rows[j]) rows[j]._idx = j; }); onChange(arr.filter(Boolean)); });
        row.appendChild(inp); row.appendChild(del);
        list.appendChild(row);
      });
      const addBtn = document.createElement("button");
      addBtn.type = "button"; addBtn.className = "ghost";
      addBtn.textContent = "+ 添加";
      addBtn.addEventListener("click", () => { arr.push(""); addRow(); onChange(arr.filter(Boolean)); });
      input.appendChild(list);
      input.appendChild(addBtn);
      break;
    }
    default: {
      const span = document.createElement("span");
      span.textContent = String(val);
      input.appendChild(span);
    }
  }
  wrap.appendChild(input);
  return wrap;
}

// ── 主渲染函数 ──────────────────────────────────────────

let configData = {};   // 当前编辑中的 config 对象
let dirty = false;

export function renderConfigPage() {
  const vp = eventsViewport();
  vp.innerHTML = "";

  const page = document.createElement("div");
  page.id = "config-page";
  page.innerHTML = `<div class="cfg-header">
    <h2>配置管理</h2>
    <span id="cfg-status" class="cfg-status"></span>
  </div>
  <div id="cfg-msg" class="cfg-msg" hidden></div>
  <div class="cfg-loading">加载中…</div>`;
  vp.appendChild(page);

  loadConfig();
}

async function loadConfig() {
  const loadEl = document.querySelector(".cfg-loading");
  const msgEl = $("cfg-msg");
  try {
    const r = await api("/api/config");
    const resp = await r.json();
    if (!resp.config) throw new Error("config 为空");
    configData = resp.config;
    dirty = false;
    renderForm(resp.writable, resp.path);
    loadEl?.remove();
  } catch (e) {
    msgEl.hidden = false;
    msgEl.className = "cfg-msg err";
    msgEl.textContent = "加载配置失败：" + e.message;
    loadEl && (loadEl.textContent = "加载失败");
  }
}

function renderForm(writable, filePath) {
  const container = document.querySelector("#config-page") || document.getElementById("config-page");
  const form = document.createElement("div");
  form.id = "cfg-form";
  form.className = "cfg-form";

  if (!writable) {
    const note = document.createElement("div");
    note.className = "cfg-note muted";
    note.textContent = "当前配置无法写回文件（无配置文件路径），以下为只读视图。";
    form.appendChild(note);
  } else {
    const note = document.createElement("div");
    note.className = "cfg-note muted";
    note.textContent = `配置文件路径：${filePath}。保存后需重启服务生效。`;
    form.appendChild(note);
  }

  // 渲染每个 section
  for (const sec of CONFIG_SECTIONS) {
    const details = document.createElement("details");
    details.className = "cfg-section";
    details.open = true;
    const summary = document.createElement("summary");
    const h3 = document.createElement("h3");
    h3.textContent = sec.label;
    summary.appendChild(h3);
    details.appendChild(summary);

    for (const f of sec.fields) {
      const currentVal = getNested(configData, f.key);
      const fieldEl = renderField(f, currentVal, (newVal) => {
        setNested(configData, f.key, newVal);
        dirty = true;
        updateSaveBtn();
      });
      details.appendChild(fieldEl);
    }
    form.appendChild(details);
  }

  // 提交按钮
  const btnRow = document.createElement("div");
  btnRow.className = "cfg-actions";
  const saveBtn = document.createElement("button");
  saveBtn.id = "cfg-save";
  saveBtn.className = "cfg-save";
  saveBtn.textContent = "保存配置";
  saveBtn.disabled = !writable;
  saveBtn.addEventListener("click", saveConfig);
  btnRow.appendChild(saveBtn);
  form.appendChild(btnRow);
  container.appendChild(form);
}

function updateSaveBtn() {
  const btn = $("cfg-save");
  if (btn) btn.textContent = dirty ? "保存配置（有未保存更改）" : "保存配置";
}

async function saveConfig() {
  const msgEl = $("cfg-msg");
  const saveBtn = $("cfg-save");
  if (!saveBtn) return;
  saveBtn.disabled = true;
  saveBtn.textContent = "保存中…";
  try {
    const r = await fetch("/api/config", {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...authHeaders() },
      body: JSON.stringify(configData),
    });
    const resp = await r.json();
    msgEl.hidden = false;
    if (r.ok) {
      msgEl.className = "cfg-msg ok";
      msgEl.textContent = resp.message || "配置已保存";
      dirty = false;
      updateSaveBtn();
    } else {
      msgEl.className = "cfg-msg err";
      msgEl.textContent = resp.error || "保存失败";
    }
  } catch (e) {
    msgEl.hidden = false;
    msgEl.className = "cfg-msg err";
    msgEl.textContent = "保存失败：" + e.message;
  }
  saveBtn.disabled = false;
  saveBtn.textContent = "保存配置";
}