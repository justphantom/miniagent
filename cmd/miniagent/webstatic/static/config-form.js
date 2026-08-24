"use strict";

// config-form.js — config schema, field rendering, nested object helpers, and client validation.

import { $, MASK, state } from "./config-state.js";

// ── Field schema ──────────────────────────────────────────

// special:"providers" 走独立渲染（卡片列表），其余按 fields 通用渲染。
const CONFIG_SECTIONS = [
  { group: "providers", label: "提供商", special: "providers" },
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
    { key: "web.key", label: "API 密钥", type: "secret", help: "留空=无认证(仅限回环地址)；******** 表示保持原值不变" },
    { key: "web.max_concurrent_turns", label: "最大并发轮次", type: "number", default: "0", help: "0=不限" },
    { key: "web.allowed_hosts", label: "允许 Host 白名单", type: "array", help: "每行一个，反代域名/外部 IP" },
  ]},
  { group: "session", label: "会话", fields: [
    { key: "session.dir", label: "会话目录", type: "text" },
  ]},
];

// ── 工具函数 ──────────────────────────────────────────

function getNested(obj, path) {
  return path.split(".").reduce((o, k) => (o != null ? o[k] : undefined), obj);
}

function setNested(obj, path, val) {
  const parts = path.split(".");
  let cur = obj;
  for (let i = 0; i < parts.length - 1; i++) {
    if (cur[parts[i]] == null) cur[parts[i]] = {};
    cur = cur[parts[i]];
  }
  cur[parts[parts.length - 1]] = val;
}

function deleteNested(obj, path) {
  const parts = path.split(".");
  let cur = obj;
  for (let i = 0; i < parts.length - 1; i++) {
    if (cur[parts[i]] == null) return;
    cur = cur[parts[i]];
  }
  delete cur[parts[parts.length - 1]];
}

function markDirty() {
  state.dirty = true;
  updateSaveBtn();
}

function updateSaveBtn() {
  const btn = $("cfg-save");
  if (btn) btn.textContent = state.dirty ? "保存配置（有未保存更改）" : "保存配置";
}

// ── 字段渲染 ──────────────────────────────────────────

function renderField(labelText, type, currentVal, onChange, help, def) {
  const wrap = document.createElement("div");
  wrap.className = "cfg-field";
  const label = document.createElement("label");
  label.className = "cfg-label";
  label.textContent = labelText;
  label.title = help || "";
  wrap.appendChild(label);
  if (help) {
    const h = document.createElement("span");
    h.className = "cfg-help muted";
    h.textContent = help;
    wrap.appendChild(h);
  }
  const input = document.createElement("div");
  input.className = "cfg-input";
  const val = currentVal !== undefined && currentVal !== null ? currentVal : def ?? "";

  switch (type) {
    case "bool": {
      const cb = document.createElement("input");
      cb.type = "checkbox";
      cb.checked = val === true;
      cb.indeterminate = currentVal === null || currentVal === undefined;
      cb.addEventListener("change", () => {
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
      inp.placeholder = def || "";
      inp.addEventListener("input", () => {
        const v = inp.value.trim();
        onChange(v === "" ? null : Number(v));
      });
      input.appendChild(inp);
      break;
    }
    case "secret": {
      const inp = document.createElement("input");
      inp.type = "text";
      inp.value = val !== "" && val !== MASK ? MASK : (val === MASK ? MASK : "");
      inp.placeholder = def || "";
      inp.onfocus = () => { if (inp.value === MASK) inp.value = ""; };
      inp.onblur = () => { if (inp.value === "") inp.value = MASK; };
      inp.addEventListener("input", () => { onChange(inp.value.trim() || null); });
      input.appendChild(inp);
      break;
    }
    case "duration":
    case "text": {
      const inp = document.createElement("input");
      inp.type = "text";
      inp.value = val !== "" ? val : "";
      inp.placeholder = type === "duration" ? "如 30s / 5m / 2h" : (def || "");
      if (type === "duration") inp.placeholder = "如 30s / 5m / 2h";
      inp.addEventListener("input", () => { onChange(inp.value.trim() || null); });
      input.appendChild(inp);
      break;
    }
    case "array": {
      const arr = Array.isArray(val) ? [...val] : [];
      const list = document.createElement("div");
      list.className = "cfg-array";
      const sync = () => onChange(arr.filter(Boolean));
      const addRow = (v) => {
        const row = document.createElement("div");
        row.className = "cfg-array-row";
        const inp = document.createElement("input");
        inp.type = "text";
        inp.value = v || "";
        const del = document.createElement("button");
        del.type = "button"; del.className = "ghost cfg-array-del"; del.textContent = "✕";
        del.addEventListener("click", () => { row.remove(); arr.splice([...list.children].indexOf(row), 1); sync(); });
        inp.addEventListener("input", () => {
          const idx = [...list.children].indexOf(row);
          arr[idx] = inp.value.trim();
          sync();
        });
        row.appendChild(inp); row.appendChild(del);
        list.appendChild(row);
      };
      arr.forEach(addRow);
      const addBtn = document.createElement("button");
      addBtn.type = "button"; addBtn.className = "ghost";
      addBtn.textContent = "+ 添加";
      addBtn.addEventListener("click", () => { arr.push(""); addRow(""); sync(); });
      input.appendChild(list); input.appendChild(addBtn);
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

// ── 客户端校验 ──────────────────────────────────────────

function clientValidate(cfg) {
  const errs = [];
  const providers = Array.isArray(cfg.providers) ? cfg.providers : [];
  if (providers.length === 0) errs.push("providers 为空，至少需要一个提供商");
  const seen = new Set();
  for (let i = 0; i < providers.length; i++) {
    const p = providers[i];
    if (!p.name) errs.push(`providers[${i}].name 为空`);
    else if (seen.has(p.name)) errs.push(`provider 名称 "${p.name}" 重复`);
    else seen.add(p.name);
    if (!p.chat_url) errs.push(`providers[${i}].name 缺少 chat_url`);
    if (p.models && p.models.length > 0) {
      const mseen = new Set();
      for (let j = 0; j < p.models.length; j++) {
        if (!p.models[j].name) errs.push(`providers[${i}] 模型[${j}] 名称空`);
        else if (mseen.has(p.models[j].name)) errs.push(`providers[${i}] 模型名 "${p.models[j].name}" 重复`);
        else mseen.add(p.models[j].name);
      }
    }
  }
  if (providers.length > 0) {
    const def = cfg.defaults || {};
    if (!def.provider) errs.push("defaults.provider 为空");
    if (!def.model) errs.push("defaults.model 为空");
    if (def.provider && !providers.some((p) => p.name === def.provider)) {
      errs.push(`defaults.provider "${def.provider}" 未在 providers 中声明`);
    }
  }
  return errs;
}

export { CONFIG_SECTIONS, getNested, setNested, deleteNested, markDirty, updateSaveBtn, renderField, clientValidate };